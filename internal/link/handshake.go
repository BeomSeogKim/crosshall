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
	"strings"
	"unicode"
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
		local.Name, Printable(local.Release), local.Link, remote.Name, Printable(remote.Release), remote.Link, older.Name, newer.Link.Min)
}

// Printable makes text a peer sent safe to show: control and other
// non-printing characters are dropped and the length is bounded.
func Printable(s string) string {
	var b strings.Builder
	for _, c := range s {
		if b.Len() >= 64 {
			b.WriteString("…")
			break
		}
		if unicode.IsPrint(c) {
			b.WriteRune(c)
		}
	}
	return b.String()
}

// Frame types of the handshake, in order: the home's challenge, the
// spoke's hello, the home's proof, the spoke's proof, then welcome. Either
// side may answer with refuse instead.
const (
	TypeChallenge = "challenge"
	TypeHello     = "hello"
	TypeProve     = "prove"
	TypeAuth      = "auth"
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
	TimeMS  int64    `json:"time_ms,omitempty"`
}

// Hello names the spoke and adds its own nonce. It proves nothing yet: the
// home proves first that it holds this device's enrollment.
type Hello struct {
	Device  string   `json:"device"`
	Release string   `json:"release"`
	Link    Range    `json:"link"`
	Caps    []string `json:"caps,omitempty"`
	Nonce   string   `json:"nonce"`
}

// Proved carries one side's proof over both nonces.
type Proved struct {
	Proof string `json:"proof"`
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

// Refuse ends a handshake. The receiver shows its own text for a code it
// knows, never the sender's message.
type Refuse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// RefusedError is a refusal, with a message written on this side.
type RefusedError struct{ Refuse }

func (e *RefusedError) Error() string { return e.Message }

// Session is an accepted link.
type Session struct {
	Link        int
	Caps        []string
	PeerRelease string
	Device      string
	PeerTimeMS  int64
}

// Roles separate the two proofs, so neither can be reflected as the other.
const (
	RoleHome  = "home"
	RoleSpoke = "spoke"
)

// Proof binds a device's secret to one role in one handshake: the hall,
// the device and both sides' fresh nonces.
func Proof(secret []byte, role, hall, device, homeNonce, spokeNonce string) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte("crosshall link\x00" + role + "\x00" + hall + "\x00" + device + "\x00" + homeNonce + "\x00" + spokeNonce))
	return hex.EncodeToString(m.Sum(nil))
}

// CheckProof reports whether proof is the role's proof for the handshake.
// An empty secret or a malformed nonce proves nothing.
func CheckProof(secret []byte, role, hall, device, homeNonce, spokeNonce, proof string) bool {
	if len(secret) == 0 || !ValidNonce(homeNonce) || !ValidNonce(spokeNonce) {
		return false
	}
	return hmac.Equal([]byte(proof), []byte(Proof(secret, role, hall, device, homeNonce, spokeNonce)))
}

// NewNonce returns 128 random bits in hex.
func NewNonce() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

// ValidNonce reports a nonce as NewNonce makes them.
func ValidNonce(n string) bool {
	b, err := hex.DecodeString(n)
	return err == nil && len(b) == 16 && n == strings.ToLower(n)
}

// Home is the home side's configuration of a handshake. Both callbacks
// reach whoever keeps the device secrets (the home dispatcher), so no
// secret reaches the process that runs the handshake.
type Home struct {
	Hall, Release string
	Link          Range
	Caps          []string
	TimeMS        func() int64
	// Prove returns the home's proof for the device, or known false for a
	// device the hall has no record of.
	Prove func(device, homeNonce, spokeNonce string) (known bool, proof string, err error)
	// Verify checks the spoke's proof.
	Verify func(device, homeNonce, spokeNonce, proof string) (ok bool, err error)
	nonce  func() string
}

// Accept runs the home side. A refusal is sent to the spoke and returned;
// a callback error ends the handshake with no welcome.
func (h Home) Accept(r *Reader, w *Writer) (Session, error) {
	nonce := NewNonce
	if h.nonce != nil {
		nonce = h.nonce
	}
	c := Challenge{Hall: h.Hall, Release: h.Release, Link: h.Link, Caps: h.Caps, Nonce: nonce()}
	if h.TimeMS != nil {
		c.TimeMS = h.TimeMS()
	}
	if err := w.Write(TypeChallenge, c); err != nil {
		return Session{}, err
	}
	var hello Hello
	if err := expect(r, TypeHello, &hello); err != nil {
		return Session{}, err
	}
	if !ValidNonce(hello.Nonce) {
		return Session{}, errors.New("link: the spoke sent a malformed nonce")
	}
	refuse := func(code, msg string) (Session, error) {
		ref := Refuse{Code: code, Message: msg}
		if err := w.Write(TypeRefuse, ref); err != nil {
			return Session{}, errors.Join(&RefusedError{ref}, err)
		}
		return Session{}, &RefusedError{ref}
	}
	v, ok := Negotiate(h.Link, hello.Link)
	if !ok {
		return refuse(RefuseNoVersion, Incompatible(
			Peer{Name: "the home device", Release: h.Release, Link: h.Link},
			Peer{Name: "the spoke", Release: hello.Release, Link: hello.Link}))
	}
	known, proof, err := h.Prove(hello.Device, c.Nonce, hello.Nonce)
	if err != nil {
		return Session{}, err
	}
	if !known {
		return refuse(RefuseUnknownDevice, "crosshall link: the spoke's device is not linked to this hall")
	}
	if err := w.Write(TypeProve, Proved{Proof: proof}); err != nil {
		return Session{}, err
	}
	var auth Proved
	if err := expect(r, TypeAuth, &auth); err != nil {
		return Session{}, err
	}
	ok, err = h.Verify(hello.Device, c.Nonce, hello.Nonce, auth.Proof)
	if err != nil {
		return Session{}, err
	}
	if !ok {
		return refuse(RefuseBadProof, "crosshall link: the spoke's link secret does not match this hall's")
	}
	caps := shared(h.Caps, hello.Caps)
	if err := w.Write(TypeWelcome, Welcome{Link: v, Caps: caps}); err != nil {
		return Session{}, err
	}
	return Session{Link: v, Caps: caps, PeerRelease: Printable(hello.Release), Device: hello.Device}, nil
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
	nonce        func() string
}

// Connect runs the spoke side. The spoke proves its secret only after the
// home has proved, over a nonce the spoke just made, that it holds this
// device's enrollment in the expected hall.
func (s Spoke) Connect(r *Reader, w *Writer) (Session, error) {
	if len(s.Secret) == 0 {
		return Session{}, errors.New("link: this device has no link secret; run crosshall link <ssh-target> again")
	}
	nonce := NewNonce
	if s.nonce != nil {
		nonce = s.nonce
	}
	var c Challenge
	if err := expect(r, TypeChallenge, &c); err != nil {
		return Session{}, err
	}
	if c.Hall != s.Hall {
		return Session{}, fmt.Errorf("crosshall link: %s serves another hall than the one this device linked to; run crosshall link %s again", s.Target, s.Target)
	}
	if !ValidNonce(c.Nonce) {
		return Session{}, fmt.Errorf("link: %s sent a malformed nonce", s.Target)
	}
	v, ok := Negotiate(s.Link, c.Link)
	if !ok {
		return Session{}, errors.New(Incompatible(
			Peer{Name: "this device", Release: s.Release, Link: s.Link},
			Peer{Name: s.Target, Release: c.Release, Link: c.Link}))
	}
	mine := nonce()
	if err := w.Write(TypeHello, Hello{Device: s.Device, Release: s.Release, Link: s.Link, Caps: s.Caps, Nonce: mine}); err != nil {
		return Session{}, err
	}
	var home Proved
	if err := s.expectOrRefused(r, TypeProve, &home, c); err != nil {
		return Session{}, err
	}
	if !CheckProof(s.Secret, RoleHome, s.Hall, s.Device, c.Nonce, mine, home.Proof) {
		return Session{}, fmt.Errorf("crosshall link: %s did not prove that it holds this device's link; the device's proof was not sent", s.Target)
	}
	if err := w.Write(TypeAuth, Proved{Proof: Proof(s.Secret, RoleSpoke, s.Hall, s.Device, c.Nonce, mine)}); err != nil {
		return Session{}, err
	}
	var wel Welcome
	if err := s.expectOrRefused(r, TypeWelcome, &wel, c); err != nil {
		return Session{}, err
	}
	caps := shared(c.Caps, s.Caps)
	if wel.Link != v || !slices.Equal(wel.Caps, caps) {
		return Session{}, fmt.Errorf("link: %s welcomed link %d with capabilities %q; the agreed terms are link %d with %q",
			s.Target, wel.Link, Printable(strings.Join(wel.Caps, ",")), v, strings.Join(caps, ","))
	}
	return Session{Link: v, Caps: caps, PeerRelease: Printable(c.Release), Device: s.Device, PeerTimeMS: c.TimeMS}, nil
}

// expectOrRefused reads the frame t, or turns a refusal into a message
// written on this side.
func (s Spoke) expectOrRefused(r *Reader, t string, v any, c Challenge) error {
	f, err := r.Read()
	if err != nil {
		return err
	}
	switch f.Type {
	case t:
		if err := json.Unmarshal(f.Body, v); err != nil {
			return fmt.Errorf("link: malformed %s", t)
		}
		return nil
	case TypeRefuse:
		var ref Refuse
		if err := json.Unmarshal(f.Body, &ref); err != nil {
			return errors.New("link: malformed refusal")
		}
		return &RefusedError{Refuse{Code: ref.Code, Message: s.refusal(ref.Code, c)}}
	}
	return fmt.Errorf("link: unexpected %q frame in the handshake", Printable(f.Type))
}

func (s Spoke) refusal(code string, c Challenge) string {
	switch code {
	case RefuseUnknownDevice:
		return fmt.Sprintf("crosshall link: this device is not linked to the hall on %s; run crosshall link %s again", s.Target, s.Target)
	case RefuseBadProof:
		return fmt.Sprintf("crosshall link: this device's link secret does not match the one on %s; run crosshall link %s again", s.Target, s.Target)
	case RefuseNoVersion:
		return Incompatible(Peer{Name: "this device", Release: s.Release, Link: s.Link},
			Peer{Name: s.Target, Release: c.Release, Link: c.Link})
	}
	return fmt.Sprintf("crosshall link: %s refused the link (%s)", s.Target, Printable(code))
}

func expect(r *Reader, t string, v any) error {
	f, err := r.Read()
	if err != nil {
		return err
	}
	if f.Type != t {
		return fmt.Errorf("link: unexpected %q frame, want %q", Printable(f.Type), t)
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
