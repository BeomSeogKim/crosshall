package link

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
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

func pair() (Home, Spoke) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	home := Home{Hall: "hall-1", Release: "v0.4.0", Link: Range{1, 2}, Caps: []string{"offers", "wakes"},
		Secret: func(d string) ([]byte, bool) { return secret, d == "dev-1" }}
	spoke := Spoke{Target: "mini", Hall: "hall-1", Device: "dev-1", Secret: secret, Release: "v0.4.1",
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
		"unknown device":                   {func(h *Home, s *Spoke) { s.Device = "dev-2" }, RefuseUnknownDevice, "not linked"},
		"wrong secret":                     {func(h *Home, s *Spoke) { s.Secret = []byte("other") }, RefuseBadProof, "does not match"},
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
	}
}

// A home that refuses a spoke's versions tells it why, naming both sides.
func TestTheHomeRefusesASpokeWithNoCommonVersion(t *testing.T) {
	home, spoke := pair()
	home.Link = Range{1, 1}
	spoke.Link = Range{2, 2}
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
	if err := w.Write(TypeHello, Hello{Device: "dev-1", Release: "v0.5.0", Link: spoke.Link,
		Proof: Proof(spoke.Secret, c.Hall, "dev-1", c.Nonce)}); err != nil {
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

// A home that picks a version outside the spoke's range is not trusted.
func TestTheSpokeRejectsAWelcomeOutsideItsRange(t *testing.T) {
	_, spoke := pair()
	hr, sw := io.Pipe()
	sr, hw := io.Pipe()
	go func() {
		w, r := NewWriter(hw), NewReader(hr)
		w.Write(TypeChallenge, Challenge{Hall: "hall-1", Release: "v9", Link: Range{1, 9}, Nonce: "n"})
		var h Hello
		expect(r, TypeHello, &h)
		w.Write(TypeWelcome, Welcome{Link: 9})
		hw.Close()
	}()
	if _, err := spoke.Connect(NewReader(sr), NewWriter(sw)); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("err %v", err)
	}
}
