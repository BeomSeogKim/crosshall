package domain

import (
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/link"
)

func decodeAs[T any](t *testing.T, r core.Result) T {
	t.Helper()
	wantOK(t, r)
	var v T
	if err := json.Unmarshal(r.Data, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func secretHex(b byte) string { return strings.Repeat(hex.EncodeToString([]byte{b}), 32) }

func TestEnrollingADeviceGivesItAnIDAndCheckableProofs(t *testing.T) {
	f := newFixture(t)
	hall := decodeAs[LinkHallResult](t, f.owner(LinkHall{}, "")).Hall
	if len(hall) != 32 {
		t.Fatalf("hall id %q", hall)
	}
	got := decodeAs[LinkEnrollResult](t, f.owner(LinkEnroll{Name: "macbook", Secret: secretHex(1)}, "k1"))
	if got.Hall != hall || len(got.Device) != 32 || got.Rotated {
		t.Fatalf("enroll %+v", got)
	}
	secret, _ := hex.DecodeString(secretHex(1))
	proof := link.Proof(secret, hall, got.Device, "n1")
	for name, c := range map[string]struct {
		cmd       LinkVerify
		known, ok bool
	}{
		"right proof":     {LinkVerify{Device: got.Device, Nonce: "n1", Proof: proof}, true, true},
		"other nonce":     {LinkVerify{Device: got.Device, Nonce: "n2", Proof: proof}, true, false},
		"unknown device":  {LinkVerify{Device: "nope", Nonce: "n1", Proof: proof}, false, false},
		"another's proof": {LinkVerify{Device: got.Device, Nonce: "n1", Proof: link.Proof([]byte("x"), hall, got.Device, "n1")}, true, false},
	} {
		v := decodeAs[LinkVerifyResult](t, f.owner(c.cmd, ""))
		if v.Known != c.known || v.OK != c.ok {
			t.Errorf("%s: %+v", name, v)
		}
	}
	// The secret is stored for verification only: no audit or op row holds it.
	if n := f.count(`SELECT COUNT(*) FROM event WHERE detail LIKE ?`, "%"+secretHex(1)[:16]+"%"); n != 0 {
		t.Fatalf("%d events hold the secret", n)
	}
	if n := f.count(`SELECT COUNT(*) FROM op WHERE result LIKE ?`, "%"+secretHex(1)[:16]+"%"); n != 0 {
		t.Fatalf("%d ops hold the secret", n)
	}
}

// Linking a device again under its name keeps its ID and retires the old
// secret; a retried enroll replays its first answer.
func TestEnrollingAgainRotatesTheSecret(t *testing.T) {
	f := newFixture(t)
	first := decodeAs[LinkEnrollResult](t, f.owner(LinkEnroll{Name: "macbook", Secret: secretHex(1)}, "k1"))
	if again := decodeAs[LinkEnrollResult](t, f.owner(LinkEnroll{Name: "macbook", Secret: secretHex(1)}, "k1")); again != first {
		t.Fatalf("replay %+v, first %+v", again, first)
	}
	second := decodeAs[LinkEnrollResult](t, f.owner(LinkEnroll{Name: "macbook", Secret: secretHex(2)}, "k2"))
	if second.Device != first.Device || !second.Rotated {
		t.Fatalf("second %+v, first %+v", second, first)
	}
	old, _ := hex.DecodeString(secretHex(1))
	v := decodeAs[LinkVerifyResult](t, f.owner(LinkVerify{Device: first.Device, Nonce: "n",
		Proof: link.Proof(old, first.Hall, first.Device, "n")}, ""))
	if !v.Known || v.OK {
		t.Fatalf("old secret: %+v", v)
	}
	if n := f.count(`SELECT COUNT(*) FROM device`); n != 1 {
		t.Fatalf("%d devices", n)
	}
}

func TestOnlyTheOwnerLinksDevices(t *testing.T) {
	f := newFixture(t)
	s := f.newSession("claude")
	f.owner(CreateChannel{Name: "general"}, "")
	wantOK(t, f.join(s, Join{Channel: "general"}, "j1"))
	c := f.caller(s)
	for _, cmd := range []Command{LinkEnroll{Name: "m", Secret: secretHex(1)}, LinkHall{}, LinkVerify{Device: "d"}} {
		wantCode(t, f.exec(c, Request{OpKey: "x", Command: cmd}), core.ExitRefused, core.CodeOwnerOnly)
	}
}

func TestEnrollRejectsABadNameOrAShortSecret(t *testing.T) {
	f := newFixture(t)
	for _, cmd := range []LinkEnroll{
		{Name: "Mac Book", Secret: secretHex(1)},
		{Name: "m", Secret: "abcd"},
		{Name: "m", Secret: strings.Repeat("zz", 32)},
	} {
		if r := f.owner(cmd, "k"); r.Status != core.ExitUsage {
			t.Errorf("%+v: status %d", cmd, r.Status)
		}
	}
	if n := f.count(`SELECT COUNT(*) FROM device`); n != 0 {
		t.Fatalf("%d devices", n)
	}
}
