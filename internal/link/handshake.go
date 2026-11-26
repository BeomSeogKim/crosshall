package link

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

// Version is the link protocol this build speaks. It is separate from the
// spool wire version and from either device's store schema: a store
// migration on one device never changes what crosses the link.
const Version = 1

// Supported is the window this build accepts: its own version and the one
// before it (#400 decision 3).
func Supported() Range { return Range{Min: max(1, Version-1), Max: Version} }

// Range is an inclusive span of link versions.
type Range struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

func (r Range) String() string {
	if r.Min == r.Max {
		return fmt.Sprintf("link %d", r.Min)
	}
	return fmt.Sprintf("link %d-%d", r.Min, r.Max)
}

// Negotiate picks the highest version both ranges hold.
func Negotiate(a, b Range) (int, bool) {
	v := min(a.Max, b.Max)
	return v, v >= max(a.Min, b.Min) && v >= 1
}

// Peer names one side of a link for messages.
type Peer struct {
	Name    string // "this device", or the ssh target of the home
	Release string
	Link    Range
}

// Incompatible is the message for two sides with no common version: it
// names both releases and ranges, and which side to upgrade.
func Incompatible(local, remote Peer) string {
	older, newer := local, remote
	if remote.Link.Max < local.Link.Max {
		older, newer = remote, local
	}
	return fmt.Sprintf("crosshall link: %s runs %s (%s), %s runs %s (%s); no common link version: upgrade crosshall on %s to a release that speaks link %d",
		local.Name, local.Release, local.Link, remote.Name, remote.Release, remote.Link, older.Name, newer.Link.Min)
}

// Frame types of the handshake.
const (
	TypeChallenge = "challenge"
	TypeHello     = "hello"
	TypeWelcome   = "welcome"
	TypeRefuse    = "refuse"
)

// Challenge is the home's first frame.
type Challenge struct {
	Hall    string   `json:"hall"`
	Release string   `json:"release"`
	Link    Range    `json:"link"`
	Caps    []string `json:"caps,omitempty"`
	Nonce   string   `json:"nonce"`
}

// Hello is the spoke's answer.
type Hello struct {
	Device  string   `json:"device"`
	Release string   `json:"release"`
	Link    Range    `json:"link"`
	Caps    []string `json:"caps,omitempty"`
	Proof   string   `json:"proof"`
}

// Welcome accepts a spoke at one version with the shared capabilities.
type Welcome struct {
	Link int      `json:"link"`
	Caps []string `json:"caps,omitempty"`
}

// Refusal codes.
const (
	RefuseUnknownDevice = "unknown_device"
	RefuseBadProof      = "bad_proof"
	RefuseNoVersion     = "no_common_version"
)

// Refuse ends a handshake.
type Refuse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// RefusedError is a refusal received from the other side.
type RefusedError struct{ Refuse }

func (e *RefusedError) Error() string { return e.Message }

// Session is an accepted link.
type Session struct {
	Link        int
	Caps        []string
	PeerRelease string
	Device      string
}

// CheckProof reports whether proof is the device's proof for the nonce.
func CheckProof(secret []byte, hall, device, nonce, proof string) bool {
	return hmac.Equal([]byte(proof), []byte(Proof(secret, hall, device, nonce)))
}

// Proof binds a device's secret to one challenge of one hall.
func Proof(secret []byte, hall, device, nonce string) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(hall + "\x00" + device + "\x00" + nonce))
	return hex.EncodeToString(m.Sum(nil))
}

// NewNonce returns 128 random bits in hex.
func NewNonce() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

// Home is the home side's configuration of a handshake.
type Home struct {
	Hall, Release string
	Link          Range
	Caps          []string
	// Verify checks a device's proof for a challenge. The secret stays
	// with whoever holds it (the home dispatcher's store); known is false
	// for a device the hall has no record of.
	Verify func(device, nonce, proof string) (known, ok bool, err error)
	nonce  func() string
}

// Accept runs the home side: challenge, check the hello, then welcome or
// refuse. A refusal is sent to the spoke and returned.
func (h Home) Accept(r *Reader, w *Writer) (Session, error) {
	nonce := NewNonce
	if h.nonce != nil {
		nonce = h.nonce
	}
	c := Challenge{Hall: h.Hall, Release: h.Release, Link: h.Link, Caps: h.Caps, Nonce: nonce()}
	if err := w.Write(TypeChallenge, c); err != nil {
		return Session{}, err
	}
	var hello Hello
	if err := expect(r, TypeHello, &hello); err != nil {
		return Session{}, err
	}
	refuse := func(code, msg string) (Session, error) {
		ref := Refuse{Code: code, Message: msg}
		if err := w.Write(TypeRefuse, ref); err != nil {
			return Session{}, errors.Join(&RefusedError{ref}, err)
		}
		return Session{}, &RefusedError{ref}
	}
	known, ok, err := h.Verify(hello.Device, c.Nonce, hello.Proof)
	switch {
	case err != nil:
		return Session{}, err
	case !known:
		return refuse(RefuseUnknownDevice, "crosshall link: this device is not linked to the hall on the home device; run crosshall link <ssh-target> again")
	case !ok:
		return refuse(RefuseBadProof, "crosshall link: this device's link secret does not match the home device's; run crosshall link <ssh-target> again")
	}
	v, ok := Negotiate(h.Link, hello.Link)
	if !ok {
		return refuse(RefuseNoVersion, Incompatible(
			Peer{Name: "the spoke", Release: hello.Release, Link: hello.Link},
			Peer{Name: "the home device", Release: h.Release, Link: h.Link}))
	}
	caps := shared(h.Caps, hello.Caps)
	if err := w.Write(TypeWelcome, Welcome{Link: v, Caps: caps}); err != nil {
		return Session{}, err
	}
	return Session{Link: v, Caps: caps, PeerRelease: hello.Release, Device: hello.Device}, nil
}

// Spoke is the spoke side's configuration of a handshake.
type Spoke struct {
	// Target names the home in messages, as the user gave it to crosshall link.
	Target       string
	Hall, Device string
	Secret       []byte
	Release      string
	Link         Range
	Caps         []string
}

// Connect runs the spoke side. It checks the challenge's hall and versions
// before it sends anything that proves its secret.
func (s Spoke) Connect(r *Reader, w *Writer) (Session, error) {
	var c Challenge
	if err := expect(r, TypeChallenge, &c); err != nil {
		return Session{}, err
	}
	if c.Hall != s.Hall {
		return Session{}, fmt.Errorf("crosshall link: %s serves another hall than the one this device linked to; run crosshall link %s again", s.Target, s.Target)
	}
	if _, ok := Negotiate(s.Link, c.Link); !ok {
		return Session{}, errors.New(Incompatible(
			Peer{Name: "this device", Release: s.Release, Link: s.Link},
			Peer{Name: s.Target, Release: c.Release, Link: c.Link}))
	}
	if err := w.Write(TypeHello, Hello{Device: s.Device, Release: s.Release, Link: s.Link, Caps: s.Caps,
		Proof: Proof(s.Secret, s.Hall, s.Device, c.Nonce)}); err != nil {
		return Session{}, err
	}
	f, err := r.Read()
	if err != nil {
		return Session{}, err
	}
	switch f.Type {
	case TypeWelcome:
		var wel Welcome
		if err := json.Unmarshal(f.Body, &wel); err != nil {
			return Session{}, fmt.Errorf("link: malformed welcome")
		}
		if wel.Link < max(s.Link.Min, c.Link.Min) || wel.Link > min(s.Link.Max, c.Link.Max) {
			return Session{}, fmt.Errorf("link: the home device chose link %d outside %s", wel.Link, s.Link)
		}
		return Session{Link: wel.Link, Caps: wel.Caps, PeerRelease: c.Release, Device: s.Device}, nil
	case TypeRefuse:
		var ref Refuse
		if err := json.Unmarshal(f.Body, &ref); err != nil {
			return Session{}, fmt.Errorf("link: malformed refusal")
		}
		return Session{}, &RefusedError{ref}
	}
	return Session{}, fmt.Errorf("link: unexpected %q frame in the handshake", f.Type)
}

func expect(r *Reader, t string, v any) error {
	f, err := r.Read()
	if err != nil {
		return err
	}
	if f.Type != t {
		return fmt.Errorf("link: unexpected %q frame, want %q", f.Type, t)
	}
	if err := json.Unmarshal(f.Body, v); err != nil {
		return fmt.Errorf("link: malformed %s", t)
	}
	return nil
}

// shared keeps the capabilities both sides list, in a's order.
func shared(a, b []string) []string {
	var out []string
	for _, c := range a {
		if slices.Contains(b, c) {
			out = append(out, c)
		}
	}
	return out
}
