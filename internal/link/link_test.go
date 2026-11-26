package link

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestFramesRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	for i, body := range []any{Welcome{Link: 1, Caps: []string{"a"}}, map[string]int{"n": 2}} {
		if err := w.Write("t"+string(rune('0'+i)), body); err != nil {
			t.Fatal(err)
		}
	}
	r := NewReader(&buf)
	f, err := r.Read()
	if err != nil || f.Type != "t0" {
		t.Fatalf("first frame %+v, %v", f, err)
	}
	var wel Welcome
	if err := json.Unmarshal(f.Body, &wel); err != nil || wel.Link != 1 || wel.Caps[0] != "a" {
		t.Fatalf("welcome %+v, %v", wel, err)
	}
	if f, err := r.Read(); err != nil || f.Type != "t1" {
		t.Fatalf("second frame %+v, %v", f, err)
	}
	if _, err := r.Read(); err != io.EOF {
		t.Fatalf("after the last frame: %v, want io.EOF", err)
	}
}

// A login banner or a shell rc file's output on the home device must fail
// the link before any hall data, and say what printed.
func TestOutputThatIsNotAFrameFailsClosed(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteString("Last login: Wed Nov 26 09:55:01 on ttys001\n")
	_ = NewWriter(&buf).Write(TypeChallenge, Challenge{Hall: "h"})
	_, err := NewReader(&buf).Read()
	var stray *StrayError
	if !errors.As(err, &stray) || !errors.Is(err, ErrStray) {
		t.Fatalf("err %v, want a StrayError", err)
	}
	if stray.Seen != "Last login: Wed Nov 26 09:55:01 on ttys001" || !strings.Contains(err.Error(), "ssh -T") {
		t.Fatalf("message %q", err)
	}
}

func TestAFrameCutShortOrTooLongIsAnError(t *testing.T) {
	var buf bytes.Buffer
	_ = NewWriter(&buf).Write("t", map[string]string{"k": "v"})
	cut := buf.Bytes()[:buf.Len()-3]
	if _, err := NewReader(bytes.NewReader(cut)).Read(); err != io.ErrUnexpectedEOF {
		t.Fatalf("cut frame: %v", err)
	}
	if _, err := NewReader(bytes.NewReader(magic[:2])).Read(); err != io.ErrUnexpectedEOF {
		t.Fatalf("cut header: %v", err)
	}
	head := append(magic[:], binary.BigEndian.AppendUint32(nil, MaxFrame+1)...)
	if _, err := NewReader(bytes.NewReader(head)).Read(); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized frame: %v", err)
	}
	if err := NewWriter(io.Discard).Write("t", strings.Repeat("x", MaxFrame)); err == nil {
		t.Fatal("wrote an oversized frame")
	}
}

func TestNegotiationPicksTheHighestCommonVersion(t *testing.T) {
	for _, c := range []struct {
		a, b Range
		want int
		ok   bool
	}{
		{Range{1, 1}, Range{1, 1}, 1, true},
		{Range{1, 2}, Range{2, 3}, 2, true},
		{Range{2, 3}, Range{1, 2}, 2, true},
		{Range{1, 1}, Range{2, 3}, 0, false},
		{Range{3, 4}, Range{1, 2}, 0, false},
		{Range{0, 0}, Range{0, 0}, 0, false},
	} {
		got, ok := Negotiate(c.a, c.b)
		if ok != c.ok || ok && got != c.want {
			t.Errorf("Negotiate(%v, %v) = %d, %v; want %d, %v", c.a, c.b, got, ok, c.want, c.ok)
		}
	}
	// Each release accepts its own version and the previous one.
	if s := Supported(); s.Max != Version || s.Min != max(1, Version-1) {
		t.Fatalf("Supported() = %v", s)
	}
	if _, ok := Negotiate(Range{Version + 1, Version + 2}, Supported()); ok {
		t.Fatal("a release two versions ahead negotiated with this one")
	}
}

func TestNoCommonVersionNamesBothReleasesAndTheSideToUpgrade(t *testing.T) {
	msg := Incompatible(Peer{Name: "this device", Release: "v0.5.0", Link: Range{2, 3}},
		Peer{Name: "mini", Release: "v0.3.1", Link: Range{1, 1}})
	for _, want := range []string{"v0.5.0", "link 2-3", "v0.3.1", "link 1", "upgrade crosshall on mini", "speaks link 2"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q lacks %q", msg, want)
		}
	}
}

// handshake runs both sides over pipes and returns their results.
func handshake(home Home, spoke Spoke) (Session, error, Session, error) {
	hr, sw := io.Pipe()
	sr, hw := io.Pipe()
	type result struct {
		s   Session
		err error
	}
	done := make(chan result, 1)
	go func() {
		s, err := home.Accept(NewReader(hr), NewWriter(hw))
		hw.Close()
		hr.Close()
		done <- result{s, err}
	}()
	ss, serr := spoke.Connect(NewReader(sr), NewWriter(sw))
	sw.Close()
	sr.Close()
	h := <-done
	return h.s, h.err, ss, serr
}

var testSecret = []byte("0123456789abcdef0123456789abcdef")

// keys answers Prove and Verify as a home dispatcher that knows dev-1.
func keys(secret []byte, hall string) (func(string, string, string) (bool, string, error), func(string, string, string, string) (bool, error)) {
	prove := func(d, hn, sn string) (bool, string, error) {
		if d != "dev-1" {
			return false, "", nil
		}
		return true, Proof(secret, RoleHome, hall, d, hn, sn), nil
	}
	verify := func(d, hn, sn, proof string) (bool, error) {
		return d == "dev-1" && CheckProof(secret, RoleSpoke, hall, d, hn, sn, proof), nil
	}
	return prove, verify
}

func pair() (Home, Spoke) {
	prove, verify := keys(testSecret, "hall-1")
	home := Home{Hall: "hall-1", Release: "v0.4.0", Link: Range{1, 2}, Caps: []string{"offers", "wakes"},
		Prove: prove, Verify: verify}
	spoke := Spoke{Target: "mini", Hall: "hall-1", Device: "dev-1", Secret: testSecret, Release: "v0.4.1",
		Link: Range{2, 3}, Caps: []string{"wakes", "later"}}
	return home, spoke
}

func TestAHandshakeAgreesOnVersionAndSharedCapabilities(t *testing.T) {
	home, spoke := pair()
	hs, herr, ss, serr := handshake(home, spoke)
	if herr != nil || serr != nil {
		t.Fatalf("home %v, spoke %v", herr, serr)
	}
	if hs.Link != 2 || ss.Link != 2 || len(hs.Caps) != 1 || hs.Caps[0] != "wakes" || ss.Caps[0] != "wakes" {
		t.Fatalf("home %+v, spoke %+v", hs, ss)
	}
	if hs.Device != "dev-1" || hs.PeerRelease != "v0.4.1" || ss.PeerRelease != "v0.4.0" {
		t.Fatalf("home %+v, spoke %+v", hs, ss)
	}
}

func TestAHandshakeRefusesWhatItCannotTrust(t *testing.T) {
	cases := map[string]struct {
		edit       func(*Home, *Spoke)
		homeCode   string // the refusal the home sends, if any
		spokeError string
	}{
		"unknown device": {func(h *Home, s *Spoke) { s.Device = "dev-2" }, RefuseUnknownDevice, "not linked to the hall on mini"},
		// The home cannot prove the spoke's secret, so the spoke stops first.
		"wrong secret":                     {func(h *Home, s *Spoke) { s.Secret = []byte("other") }, "", "did not prove"},
		"another hall":                     {func(h *Home, s *Spoke) { h.Hall = "hall-2" }, "", "another hall"},
		"no common version, spoke sees it": {func(h *Home, s *Spoke) { s.Link = Range{3, 4} }, "", "no common link version"},
	}
	for name, c := range cases {
		home, spoke := pair()
		c.edit(&home, &spoke)
		_, herr, _, serr := handshake(home, spoke)
		if serr == nil || !strings.Contains(serr.Error(), c.spokeError) {
			t.Errorf("%s: spoke error %v, want %q", name, serr, c.spokeError)
		}
		var ref *RefusedError
		if c.homeCode != "" && (!errors.As(herr, &ref) || ref.Code != c.homeCode) {
			t.Errorf("%s: home error %v, want refusal %s", name, herr, c.homeCode)
		}
		if herr == nil {
			t.Errorf("%s: the home accepted", name)
		}
	}
}

// scripted plays the home from a test: it sends the challenge and then
// whatever the test writes, and records every frame the spoke sends.
func scripted(t *testing.T, spoke Spoke, play func(c Challenge, h Hello, r *Reader, w *Writer)) ([]string, error) {
	t.Helper()
	hr, sw := io.Pipe()
	sr, hw := io.Pipe()
	seen := make(chan []string, 1)
	go func() {
		var types []string
		r, w := NewReader(hr), NewWriter(hw)
		c := Challenge{Hall: "hall-1", Release: "v0.4.0", Link: Range{1, 2}, Caps: []string{"offers", "wakes"}, Nonce: NewNonce()}
		w.Write(TypeChallenge, c)
		var h Hello
		if f, err := r.Read(); err == nil {
			types = append(types, f.Type)
			json.Unmarshal(f.Body, &h)
			play(c, h, &Reader{r: r.r}, w)
		}
		hw.Close()
		for {
			f, err := r.Read()
			if err != nil {
				break
			}
			types = append(types, f.Type)
		}
		seen <- types
	}()
	_, err := spoke.Connect(NewReader(sr), NewWriter(sw))
	sw.Close()
	return <-seen, err
}

// A process at the ssh destination that presents the right hall ID but
// holds no enrollment gets a hello and nothing else: no device proof.
func TestTheSpokeSendsNoProofToAHomeThatCannotProveItself(t *testing.T) {
	_, spoke := pair()
	for name, proof := range map[string]func(c Challenge, h Hello) string{
		"no secret": func(c Challenge, h Hello) string { return "00" },
		"another secret": func(c Challenge, h Hello) string {
			return Proof([]byte("x"), RoleHome, c.Hall, h.Device, c.Nonce, h.Nonce)
		},
		"the spoke's role": func(c Challenge, h Hello) string {
			return Proof(testSecret, RoleSpoke, c.Hall, h.Device, c.Nonce, h.Nonce)
		},
		"an older handshake": func(c Challenge, h Hello) string {
			return Proof(testSecret, RoleHome, c.Hall, h.Device, c.Nonce, NewNonce())
		},
	} {
		types, err := scripted(t, spoke, func(c Challenge, h Hello, r *Reader, w *Writer) {
			w.Write(TypeProve, Proved{Proof: proof(c, h)})
		})
		if err == nil || !strings.Contains(err.Error(), "did not prove") {
			t.Errorf("%s: %v", name, err)
		}
		if !slices.Equal(types, []string{TypeHello}) {
			t.Errorf("%s: the spoke sent %v", name, types)
		}
	}
}

// A spoke's proof from an earlier handshake does not pass a fresh one: the
// home's nonce is new each time.
func TestAReplayedSpokeProofFails(t *testing.T) {
	home, _ := pair()
	var old Proved
	{
		var hello Hello
		hr, sw := io.Pipe()
		sr, hw := io.Pipe()
		done := make(chan error, 1)
		go func() { _, err := home.Accept(NewReader(hr), NewWriter(hw)); hw.Close(); done <- err }()
		r, w := NewReader(sr), NewWriter(sw)
		var c Challenge
		expect(r, TypeChallenge, &c)
		hello = Hello{Device: "dev-1", Release: "v", Link: Range{1, 2}, Nonce: NewNonce()}
		w.Write(TypeHello, hello)
		var p Proved
		expect(r, TypeProve, &p)
		old = Proved{Proof: Proof(testSecret, RoleSpoke, "hall-1", "dev-1", c.Nonce, hello.Nonce)}
		w.Write(TypeAuth, old)
		var wel Welcome
		if err := expect(r, TypeWelcome, &wel); err != nil {
			t.Fatal(err)
		}
		sw.Close()
		<-done
		// Replay the same hello and proof to a new connection.
		hr, sw = io.Pipe()
		sr, hw = io.Pipe()
		go func() { _, err := home.Accept(NewReader(hr), NewWriter(hw)); hw.Close(); done <- err }()
		r, w = NewReader(sr), NewWriter(sw)
		expect(r, TypeChallenge, &c)
		w.Write(TypeHello, hello)
		expect(r, TypeProve, &p)
		w.Write(TypeAuth, old)
		var ref Refuse
		if err := expect(r, TypeRefuse, &ref); err != nil || ref.Code != RefuseBadProof {
			t.Fatalf("replay: %+v, %v", ref, err)
		}
		sw.Close()
		if err := <-done; err == nil {
			t.Fatal("the home accepted a replayed proof")
		}
	}
}

func TestMalformedNoncesAndEmptySecretsProveNothing(t *testing.T) {
	n := NewNonce()
	if !ValidNonce(n) || ValidNonce("") || ValidNonce("abc") || ValidNonce(strings.ToUpper(n)) || ValidNonce(n+"00") {
		t.Fatal("ValidNonce")
	}
	if CheckProof(nil, RoleSpoke, "h", "d", n, n, Proof(nil, RoleSpoke, "h", "d", n, n)) {
		t.Fatal("an empty secret proved something")
	}
	if CheckProof(testSecret, RoleSpoke, "h", "d", "", n, Proof(testSecret, RoleSpoke, "h", "d", "", n)) {
		t.Fatal("an empty nonce proved something")
	}
	// A spoke with no secret does not start; a challenge without a nonce
	// gets no hello.
	_, spoke := pair()
	spoke.Secret = nil
	if _, err := spoke.Connect(NewReader(strings.NewReader("")), NewWriter(io.Discard)); err == nil {
		t.Fatal("connected without a secret")
	}
	_, spoke = pair()
	var buf bytes.Buffer
	NewWriter(&buf).Write(TypeChallenge, Challenge{Hall: "hall-1", Link: Range{1, 2}})
	var out bytes.Buffer
	if _, err := spoke.Connect(NewReader(&buf), NewWriter(&out)); err == nil || out.Len() != 0 {
		t.Fatalf("no nonce: %v, sent %d bytes", err, out.Len())
	}
	// The home refuses a hello without a nonce before asking for any proof.
	home, _ := pair()
	asked := false
	home.Prove = func(string, string, string) (bool, string, error) { asked = true; return true, "", nil }
	hr, sw := io.Pipe()
	sr, hw := io.Pipe()
	done := make(chan error, 1)
	go func() { _, err := home.Accept(NewReader(hr), NewWriter(hw)); hw.Close(); done <- err }()
	r, w := NewReader(sr), NewWriter(sw)
	var c Challenge
	expect(r, TypeChallenge, &c)
	w.Write(TypeHello, Hello{Device: "dev-1", Link: Range{1, 2}})
	if err := <-done; err == nil || asked {
		t.Fatalf("hello without a nonce: %v, asked %t", err, asked)
	}
	sw.Close()
}

// A failing verifier never yields a welcome.
func TestAVerifierErrorEndsTheHandshake(t *testing.T) {
	for name, edit := range map[string]func(*Home){
		"prove": func(h *Home) {
			h.Prove = func(string, string, string) (bool, string, error) { return false, "", errors.New("store busy") }
		},
		"verify": func(h *Home) {
			h.Verify = func(string, string, string, string) (bool, error) { return true, errors.New("store busy") }
		},
	} {
		home, spoke := pair()
		edit(&home)
		_, herr, _, serr := handshake(home, spoke)
		if herr == nil || !strings.Contains(herr.Error(), "store busy") || serr == nil {
			t.Errorf("%s: home %v, spoke %v", name, herr, serr)
		}
	}
}

// A home that refuses a spoke's versions sends the code; the spoke shows
// its own message, naming both releases.
func TestTheHomeRefusesASpokeWithNoCommonVersion(t *testing.T) {
	home, _ := pair()
	home.Link = Range{1, 1}
	hr, sw := io.Pipe()
	sr, hw := io.Pipe()
	go func() {
		home.Accept(NewReader(hr), NewWriter(hw))
		hw.Close()
	}()
	// A spoke that skips its own check, as an older spoke might.
	r, w := NewReader(sr), NewWriter(sw)
	var c Challenge
	if err := expect(r, TypeChallenge, &c); err != nil {
		t.Fatal(err)
	}
	if err := w.Write(TypeHello, Hello{Device: "dev-1", Release: "v0.5.0", Link: Range{2, 2}, Nonce: NewNonce()}); err != nil {
		t.Fatal(err)
	}
	var ref Refuse
	if err := expect(r, TypeRefuse, &ref); err != nil {
		t.Fatal(err)
	}
	if ref.Code != RefuseNoVersion || !strings.Contains(ref.Message, "v0.5.0") || !strings.Contains(ref.Message, "v0.4.0") {
		t.Fatalf("refusal %+v", ref)
	}
}

// The welcome must carry exactly the version and capabilities both sides
// agreed on.
func TestTheSpokeRejectsAWelcomeOffTheAgreedTerms(t *testing.T) {
	_, spoke := pair() // agreed with the scripted home: link 2, caps [wakes]
	for name, wel := range map[string]Welcome{
		"version above":   {Link: 9, Caps: []string{"wakes"}},
		"version below":   {Link: 1, Caps: []string{"wakes"}},
		"a cap one lacks": {Link: 2, Caps: []string{"wakes", "offers"}},
		"a cap left out":  {Link: 2},
	} {
		_, err := scripted(t, spoke, func(c Challenge, h Hello, r *Reader, w *Writer) {
			w.Write(TypeProve, Proved{Proof: Proof(testSecret, RoleHome, c.Hall, h.Device, c.Nonce, h.Nonce)})
			var auth Proved
			expect(r, TypeAuth, &auth)
			w.Write(TypeWelcome, wel)
		})
		if err == nil || !strings.Contains(err.Error(), "agreed terms") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Text from the peer is shown without control characters, and a refusal
// shows this side's message, not the peer's.
func TestPeerTextIsShownSafely(t *testing.T) {
	_, spoke := pair()
	_, err := scripted(t, spoke, func(c Challenge, h Hello, r *Reader, w *Writer) {
		w.Write(TypeRefuse, Refuse{Code: "odd\x1b[2J\n", Message: "\x1b]0;pwned\x07 clear your screen"})
	})
	if err == nil || strings.ContainsAny(err.Error(), "\x1b\x07\n") || strings.Contains(err.Error(), "pwned") ||
		!strings.Contains(err.Error(), "mini refused the link") {
		t.Fatalf("refusal: %q", err)
	}
	msg := Incompatible(Peer{Name: "this device", Release: "v1", Link: Range{1, 1}},
		Peer{Name: "mini", Release: "v9\x1b[31m" + strings.Repeat("x", 200), Link: Range{3, 3}})
	if strings.ContainsRune(msg, 0x1b) || len(msg) > 400 {
		t.Fatalf("message %q", msg)
	}
}

// A peer that prints a few bytes of something else and keeps the stream
// open is refused at once, not left waiting for a full header.
func TestAShortNonFramePrefixFailsWithTheStreamOpen(t *testing.T) {
	r, w := io.Pipe()
	defer w.Close()
	go w.Write([]byte("hi\n"))
	done := make(chan error, 1)
	go func() { _, err := NewReader(r).Read(); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrStray) {
			t.Fatalf("err %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Read waited for more bytes")
	}
}
