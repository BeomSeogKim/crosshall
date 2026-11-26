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
	hn, sn := link.NewNonce(), link.NewNonce()
	// The home's proof is the one the spoke expects from this hall.
	p := decodeAs[LinkProveResult](t, f.owner(LinkProve{Device: got.Device, HomeNonce: hn, SpokeNonce: sn}, ""))
	if !p.Known || !link.CheckProof(secret, link.RoleHome, hall, got.Device, hn, sn, p.Proof) {
		t.Fatalf("home proof %+v", p)
	}
	if u := decodeAs[LinkProveResult](t, f.owner(LinkProve{Device: "nope", HomeNonce: hn, SpokeNonce: sn}, "")); u.Known || u.Proof != "" {
		t.Fatalf("unknown device: %+v", u)
	}
	if r := f.owner(LinkProve{Device: got.Device, HomeNonce: "", SpokeNonce: sn}, ""); r.Status != core.ExitUsage {
		t.Fatalf("a proof without a nonce: %d", r.Status)
	}
	proof := link.Proof(secret, link.RoleSpoke, hall, got.Device, hn, sn)
	for name, c := range map[string]struct {
		cmd       LinkVerify
		known, ok bool
	}{
		"right proof":     {LinkVerify{Device: got.Device, HomeNonce: hn, SpokeNonce: sn, Proof: proof}, true, true},
		"other nonce":     {LinkVerify{Device: got.Device, HomeNonce: link.NewNonce(), SpokeNonce: sn, Proof: proof}, true, false},
		"unknown device":  {LinkVerify{Device: "nope", HomeNonce: hn, SpokeNonce: sn, Proof: proof}, false, false},
		"the home's role": {LinkVerify{Device: got.Device, HomeNonce: hn, SpokeNonce: sn, Proof: p.Proof}, true, false},
		"another's proof": {LinkVerify{Device: got.Device, HomeNonce: hn, SpokeNonce: sn,
			Proof: link.Proof([]byte("x"), link.RoleSpoke, hall, got.Device, hn, sn)}, true, false},
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
	hn, sn := link.NewNonce(), link.NewNonce()
	v := decodeAs[LinkVerifyResult](t, f.owner(LinkVerify{Device: first.Device, HomeNonce: hn, SpokeNonce: sn,
		Proof: link.Proof(old, link.RoleSpoke, first.Hall, first.Device, hn, sn)}, ""))
	if !v.Known || v.OK || v.CredentialGen != 0 {
		t.Fatalf("old secret: %+v", v)
	}
	// The new secret proves under a newer credential generation, which the
	// links of the old one no longer match.
	fresh, _ := hex.DecodeString(secretHex(2))
	v = decodeAs[LinkVerifyResult](t, f.owner(LinkVerify{Device: first.Device, HomeNonce: hn, SpokeNonce: sn,
		Proof: link.Proof(fresh, link.RoleSpoke, first.Hall, first.Device, hn, sn)}, ""))
	if !v.OK || v.CredentialGen != 2 {
		t.Fatalf("new secret: %+v", v)
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
	for _, cmd := range []Command{LinkEnroll{Name: "m", Secret: secretHex(1)}, LinkHall{}, LinkProve{Device: "d"}, LinkVerify{Device: "d"}} {
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

// A member of a linked device cannot be restored into, taken over or
// moved from the home device: its key and leaf live on its device.
func TestHomeSessionsCannotTakeOverOrRestoreADeviceMember(t *testing.T) {
	f := newFixture(t)
	f.owner(CreateChannel{Name: "checkout"}, "")
	remote := f.newSession("claude")
	wantOK(t, f.join(remote, Join{Channel: "checkout", Alias: "remote"}, "j1"))
	if _, err := f.db.Exec(`INSERT INTO device (device_id, name, secret, created_at_ms, secret_at_ms) VALUES ('dev-a', 'macbook', x'00', 1, 1);
		UPDATE principal SET device_id = 'dev-a' WHERE kind = 'session'`); err != nil {
		t.Fatal(err)
	}
	snapshot := func() string {
		var keys, selection string
		f.db.QueryRow(`SELECT group_concat(vendor || key || state || principal_id) FROM principal_key`).Scan(&keys)
		f.db.QueryRow(`SELECT group_concat(principal_id || ':' || binding_id || ':' || generation) FROM member_selection`).Scan(&selection)
		return keys + "|" + selection
	}
	before := snapshot()
	var binding int64
	if err := f.db.QueryRow(`SELECT binding_id FROM member_selection`).Scan(&binding); err != nil {
		t.Fatal(err)
	}
	wantCode(t, f.owner(Restore{BindingID: binding, Vendor: "claude", Key: "ffffffffffffffffffffffffffffffff"}, "r1"),
		core.ExitRefused, CodeNotOnADevice)
	home := f.newSession("claude")
	wantCode(t, f.takeoverJoin(home, "remote", "t1"), core.ExitRefused, CodeNotOnADevice)
	if after := snapshot(); after != before {
		t.Fatalf("keys or selection changed:\nbefore %s\nafter  %s", before, after)
	}
}
