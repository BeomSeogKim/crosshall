package domain

import (
	"context"
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/ident"
)

func TestRestoreRotatesTheNamedBinding(t *testing.T) {
	f, a, b, _ := channel3(t)
	f.svc.RestoreKey = ident.RestoreKey
	aid := *f.caller(a).PrincipalID
	binding := int64(f.count("SELECT id FROM binding WHERE principal_id=?", aid))
	executeGuardFixture(t, f, `UPDATE principal SET round_limit = 3 WHERE id = ?`, aid)
	if _, err := f.db.Exec("UPDATE binding SET state='departed' WHERE id=?", binding); err != nil {
		t.Fatal(err)
	}
	holder := f.newSession("claude")
	wantOK(t, f.join(holder, Join{Channel: "checkout", Alias: "form"}, "holder-join"))

	replacement := f.newSession("codex") // A failed first join can leave a provisional leaf.
	cmd := Restore{BindingID: binding, Vendor: replacement.vendor, Key: replacement.key}
	wantCode(t, f.exec(f.caller(b), Request{OpKey: "rotate", Command: cmd}), core.ExitRefused, core.CodeOwnerOnly)
	wantCode(t, f.owner(Restore{BindingID: binding, Vendor: b.vendor, Key: b.key}, "busy"), core.ExitRefused, core.CodeKeyInUse)
	wantCode(t, f.owner(Restore{BindingID: binding, Vendor: "unknown", Key: "bad"}, "invalid"), core.ExitUsage, "")
	wantCode(t, f.owner(Restore{BindingID: binding + 100, Vendor: "codex", Key: "unused"}, "missing"), core.ExitRefused, core.CodeNotFound)
	r := f.owner(cmd, "rotate")
	wantOK(t, r)
	if got := decode[RestoreResult](t, r); got.BindingID != binding {
		t.Fatalf("restored binding %d, want %d", got.BindingID, binding)
	}
	wantOK(t, f.owner(cmd, "rotate"))
	changed := cmd
	changed.BindingID += 100
	wantCode(t, f.owner(changed, "rotate"), core.ExitRefused, core.CodeKeyConflict)
	wantCode(t, f.owner(cmd, "different"), core.ExitRefused, core.CodeKeyInUse)
	wantCode(t, f.owner(Restore{BindingID: binding, Vendor: a.vendor, Key: a.key}, "old"), core.ExitRefused, core.CodeKeyInUse)
	if got := f.count("SELECT COUNT(*) FROM principal_key WHERE vendor=? AND key=? AND principal_id=? AND state='live'", cmd.Vendor, cmd.Key, aid); got != 1 {
		t.Fatal("replacement belongs to the wrong principal")
	}
	if got := f.count("SELECT COUNT(*) FROM binding WHERE id=? AND principal_id=? AND state='departed'", binding, aid); got != 1 {
		t.Fatal("restore changed membership")
	}
	if got := f.count("SELECT COUNT(*) FROM binding WHERE principal_id=? AND alias='form' AND state='live'", *f.caller(holder).PrincipalID); got != 1 {
		t.Fatal("restore changed the current alias holder")
	}
	if got := f.count("SELECT COUNT(*) FROM principal WHERE id=? AND vendor=? AND vendor_version IS NULL", aid, replacement.vendor); got != 1 {
		t.Fatal("restored principal still describes the previous session's vendor or version")
	}
	if f.count("SELECT round_limit FROM principal WHERE id=?", aid) != 3 || f.count("SELECT COUNT(*) FROM leaf WHERE state='fenced' AND digest=?", a.digest()) != 1 || f.count("SELECT principal_id FROM leaf WHERE digest=?", replacement.digest()) != int(aid) {
		t.Fatal("restore did not preserve the round limit, fence the old leaf and attach the provisional replacement")
	}
	if f.count("SELECT COUNT(*) FROM event WHERE kind='binding_restored'") != 1 || f.count("SELECT COUNT(*) FROM op WHERE verb='owner restore'") != 1 || strings.Contains(string(r.Data), cmd.Key) || f.count("SELECT COUNT(*) FROM event WHERE instr(detail,?)>0 OR instr(subject,?)>0", cmd.Key, cmd.Key) != 0 {
		t.Fatal("restore event, key privacy or replay mismatch")
	}
}

func TestRestoreRollsBackWhenAuditFails(t *testing.T) {
	f, a, _, _ := channel3(t)
	f.svc.RestoreKey = ident.RestoreKey
	before := *f.caller(a).PrincipalID
	binding := int64(f.count("SELECT id FROM binding WHERE principal_id=?", before))
	if _, err := f.db.Exec("CREATE TRIGGER reject_restore BEFORE INSERT ON event WHEN NEW.kind='binding_restored' BEGIN SELECT RAISE(ABORT, 'test failure'); END"); err != nil {
		t.Fatal(err)
	}
	tx, err := f.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	cmd := Restore{BindingID: binding, Vendor: "codex", Key: "replacement"}
	_, err = f.svc.Handle(context.Background(), tx, core.Caller{Role: core.RoleOwner}, Request{OpKey: "rotate", Command: cmd})
	if err == nil {
		t.Fatal("expected audit failure")
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if f.count("SELECT COUNT(*) FROM principal_key WHERE key=? AND state='live'", a.key) != 1 || f.count("SELECT COUNT(*) FROM leaf WHERE state='live' AND digest=?", a.digest()) != 1 || f.count("SELECT COUNT(*) FROM principal_key WHERE key=?", cmd.Key) != 0 || f.count("SELECT COUNT(*) FROM op WHERE op_key='rotate'") != 0 {
		t.Fatal("rollback left a key, fence or operation behind")
	}
}

func TestRestoreReplayAfterAnotherRotation(t *testing.T) {
	f, a, _, _ := channel3(t)
	f.svc.RestoreKey = ident.RestoreKey
	binding := int64(f.count("SELECT id FROM binding WHERE principal_id=?", *f.caller(a).PrincipalID))
	first := Restore{BindingID: binding, Vendor: "route", Key: "ABCDEF0123456789ABCDEF0123456789"}
	wantOK(t, f.owner(first, "first"))
	second := Restore{BindingID: binding, Vendor: "codex", Key: "next-session"}
	wantOK(t, f.owner(second, "second"))
	first.Key = strings.ToLower(first.Key)
	wantOK(t, f.owner(first, "first"))
	if f.count("SELECT COUNT(*) FROM principal_key WHERE vendor=? AND key=? AND state='live'", second.Vendor, second.Key) != 1 ||
		f.count("SELECT COUNT(*) FROM principal_key WHERE vendor=? AND key=? AND state='retired'", first.Vendor, first.Key) != 1 ||
		f.count("SELECT COUNT(*) FROM event WHERE kind='binding_restored'") != 2 {
		t.Fatal("replaying an earlier restore changed the current identity")
	}
}

func TestRestoreRejectsDisplayedFingerprint(t *testing.T) {
	f, a, _, _ := channel3(t)
	f.svc.RestoreKey = ident.RestoreKey
	binding := int64(f.count("SELECT id FROM binding WHERE principal_id=?", *f.caller(a).PrincipalID))
	cmd := Restore{BindingID: binding, Vendor: "codex", Key: "sha256:abc012def345"}
	r := f.owner(cmd, "rotate")
	wantCode(t, r, core.ExitUsage, "")
	if !strings.Contains(string(r.Data), "whoami --full") {
		t.Fatal("expected directions to obtain the full key")
	}
	if f.count("SELECT COUNT(*) FROM principal_key WHERE key=? AND state='live'", a.key) != 1 ||
		f.count("SELECT COUNT(*) FROM leaf WHERE digest=? AND state='live'", a.digest()) != 1 ||
		f.count("SELECT COUNT(*) FROM principal_key WHERE key=?", cmd.Key) != 0 ||
		f.count("SELECT COUNT(*) FROM event WHERE kind='binding_restored'") != 0 ||
		f.count("SELECT COUNT(*) FROM op WHERE op_key='rotate'") != 0 {
		t.Fatal("a displayed fingerprint changed the binding's identity")
	}
}
