package dispatch

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/hook/manifests"
	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

func adapterFilesFixture(t *testing.T) (*server, *os.Root) {
	t.Helper()
	s := reviewServer(t)
	s.cfg.Rendezvous = filepath.Join(t.TempDir(), "rendezvous")
	s.ready.Instance = "test-dispatcher"
	root, err := openRendezvous(s.cfg.Rendezvous)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	return s, root
}

func adapterLeafFixture(t *testing.T, s *server, root *os.Root, registered bool) *os.Root {
	t.Helper()
	digest := strings.Repeat("a", 64)
	members, err := spool.OpenServerChild(root, "m", false)
	if err != nil {
		t.Fatal(err)
	}
	defer members.Close()
	leaf, err := spool.OpenServerChild(members, digest[:16], true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { leaf.Close() })
	id, err := spool.DirectoryIdentity(leaf)
	if err != nil {
		t.Fatal(err)
	}
	if !registered {
		id = "replaced"
	}
	s.fixtureSQL(t, fmt.Sprintf(`
		INSERT INTO principal (id, kind, vendor, created_at_ms) VALUES (1, 'session', 'codex', 1);
		INSERT INTO principal_key (vendor, key, principal_id, created_at_ms)
		VALUES ('codex', 'synthetic-key', 1, 1);
		INSERT INTO leaf (digest, vendor, key, principal_id, generation, file_identity, created_at_ms)
		VALUES ('%s', 'codex', 'synthetic-key', 1, 'test-gen', '%s', 1)
	`, digest, id))
	return leaf
}

func TestAdapterStampsInvalidateBeforeCommitAndNeverReuseASequence(t *testing.T) {
	s, root := adapterFilesFixture(t)
	leaf := adapterLeafFixture(t, s, root, true)
	initial, err := s.publishAdapterStamp(leaf, "test-gen", true, true)
	if err != nil {
		t.Fatal(err)
	}
	forcedRollback := errors.New("abort after publication")
	var advanced leafAdapterStamp
	apply := func(notice, reminder bool, failure error) error {
		return s.db.Update(t.Context(), func(tx store.Tx) error {
			_, err := tx.ExecContext(t.Context(), `INSERT INTO adapter_dirty
				(principal_id, notice, reminder) VALUES (1, ?, ?)`, notice, reminder)
			if err != nil {
				return err
			}
			if err := s.applyAdapterStamps(t.Context(), tx); err != nil {
				return err
			}
			var ok bool
			advanced, ok = s.adapterStamp(leaf, "test-gen")
			if !ok {
				t.Fatal("the stamp was not published inside the transaction")
			}
			return failure
		})
	}
	if err := apply(true, false, forcedRollback); !errors.Is(err, forcedRollback) {
		t.Fatalf("rollback: %v", err)
	}
	rolledBack := advanced
	if rolledBack.NoticeSeq <= initial.NoticeSeq || rolledBack.ReminderSeq != initial.ReminderSeq {
		t.Fatalf("notice invalidation: initial %+v, got %+v", initial, rolledBack)
	}
	if err := apply(true, false, nil); err != nil {
		t.Fatal(err)
	}
	notice := advanced
	if notice.NoticeSeq <= rolledBack.NoticeSeq || notice.ReminderSeq != rolledBack.ReminderSeq {
		t.Fatalf("the commit reused a rolled-back sequence: previous %+v, got %+v", rolledBack, notice)
	}
	if err := apply(false, true, nil); err != nil {
		t.Fatal(err)
	}
	if advanced.NoticeSeq != notice.NoticeSeq || advanced.ReminderSeq <= notice.ReminderSeq {
		t.Fatalf("reminder invalidation: previous %+v, got %+v", notice, advanced)
	}
}

func TestAdapterStampsSkipAReplacedLeaf(t *testing.T) {
	s, root := adapterFilesFixture(t)
	leaf := adapterLeafFixture(t, s, root, false)
	s.fixtureSQL(t, `INSERT INTO adapter_dirty (principal_id, notice, reminder) VALUES (1, 1, 0)`)
	if err := s.db.Update(t.Context(), func(tx store.Tx) error {
		return s.applyAdapterStamps(t.Context(), tx)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := leaf.Lstat(adapterStampName); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replaced leaf must not receive a stamp: %v", err)
	}
}

func TestAdapterStampWriteFailureRemovesTheOldAcknowledgementTarget(t *testing.T) {
	s, root := adapterFilesFixture(t)
	leaf := adapterLeafFixture(t, s, root, true)
	if _, err := s.publishAdapterStamp(leaf, "test-gen", true, true); err != nil {
		t.Fatal(err)
	}
	if err := leaf.Mkdir("stamp.part", 0700); err != nil {
		t.Fatal(err)
	}
	if err := leaf.WriteFile("stamp.part/blocked", nil, 0600); err != nil {
		t.Fatal(err)
	}
	s.fixtureSQL(t, `INSERT INTO adapter_dirty (principal_id, notice, reminder) VALUES (1, 1, 0)`)
	if err := s.db.Update(t.Context(), func(tx store.Tx) error {
		return s.applyAdapterStamps(t.Context(), tx)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := leaf.Lstat(adapterStampName); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale stamp remained after publication failure: %v", err)
	}
}

func TestHookAcknowledgementUsesItsOwnSequenceAndLiveInstance(t *testing.T) {
	s, root := adapterFilesFixture(t)
	leaf := adapterLeafFixture(t, s, root, true)
	if _, err := s.publishAdapterStamp(leaf, "test-gen", true, true); err != nil {
		t.Fatal(err)
	}
	stamp, err := s.publishAdapterStamp(leaf, "test-gen", true, false)
	if err != nil {
		t.Fatal(err)
	}
	ack := func(kind, turn string, reply *bool) *wire.HookAck {
		t.Helper()
		result, err := domain.OK(wire.HookResult{Epoch: 1, Ack: &wire.HookAck{Kind: kind, TurnRef: turn}})
		if err != nil {
			t.Fatal(err)
		}
		req := wire.Request{Generation: "test-gen", Verb: wire.VerbHookEvent, Reply: reply}
		if err := s.addHookAck(t.Context(), nil, Arrival{Dir: leaf}, req, &result); err != nil {
			t.Fatal(err)
		}
		var output wire.HookResult
		if err := json.Unmarshal(result.Data, &output); err != nil {
			t.Fatal(err)
		}
		return output.Ack
	}
	notice := ack("notice", "", nil)
	reminder := ack("reminder", "third-turn", nil)
	if notice == nil || reminder == nil || notice.Seq != stamp.NoticeSeq || reminder.Seq != stamp.ReminderSeq ||
		reminder.TurnRef != "third-turn" || notice.Instance != "test-dispatcher" || notice.Generation != "test-gen" {
		t.Fatalf("unexpected notice or reminder ack: %+v, %+v", notice, reminder)
	}
	noReply := false
	if got := ack("notice", "", &noReply); got != nil {
		t.Fatalf("one-way observation acknowledged: %+v", got)
	}
	s.ready.Instance = "restarted-dispatcher"
	s.adapterSequence = 0
	if _, ok := s.adapterStamp(leaf, "test-gen"); ok {
		t.Fatal("new dispatcher accepted the previous stamp")
	}
	if after := ack("notice", "", nil); after == nil || after.Instance != s.ready.Instance {
		t.Fatalf("new ack from stale dispatcher: %+v", after)
	}
}

func TestAdapterMirrorsFailClosedBetweenCommitAndPublication(t *testing.T) {
	s, root := adapterFilesFixture(t)
	s.fixtureSQL(t, `
		INSERT INTO adapter_policy (adapter, enabled, reminder, epoch, changed_at_ms)
		VALUES ('claude', 1, 1, 2, 1);
		INSERT INTO channel (id, name, state, created_at_ms)
		VALUES (1, 'live', 'active', 1), (2, 'old', 'archived', 1);
		INSERT INTO repo_default (repo_root, channel_id)
		VALUES ('/example/beta', 1), ('/example/alpha', 1), ('/example/closed', 2)
	`)
	if err := s.publishAdapterFiles(t.Context()); err != nil {
		t.Fatal(err)
	}
	read := func(name string) string {
		t.Helper()
		raw, err := root.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	requireMissing := func(name string) {
		t.Helper()
		if _, err := root.Lstat(name); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("obsolete %s still visible: %v", name, err)
		}
	}
	if got := read(adapterPolicyName); got != "claude 1 1 2\ncodex 0 0 0\n" {
		t.Fatalf("initial policy: %q", got)
	}
	wantRepos := []string{
		fmt.Sprintf("%x\n", sha256.Sum256([]byte("/example/alpha"))),
		fmt.Sprintf("%x\n", sha256.Sum256([]byte("/example/beta"))),
	}
	slices.Sort(wantRepos)
	if got := read(adapterRepoSetName); got != strings.Join(wantRepos, "") {
		t.Fatalf("repository snapshot: %q", got)
	}
	if err := s.db.Update(t.Context(), func(tx store.Tx) error {
		if _, err := tx.ExecContext(t.Context(),
			"UPDATE adapter_policy SET enabled = 0, epoch = 3 WHERE adapter = 'claude'"); err != nil {
			return err
		}
		if err := s.invalidateAdapterFile(wire.VerbAdapterSet); err != nil {
			return err
		}
		requireMissing(adapterPolicyName)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := root.Mkdir("policy.part", 0700); err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile("policy.part/blocked", nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.publishAdapterFiles(t.Context()); err == nil {
		t.Fatal("publication unexpectedly succeeded")
	}
	requireMissing(adapterPolicyName)
	if err := root.RemoveAll("policy.part"); err != nil {
		t.Fatal(err)
	}
	if err := s.invalidateAdapterFile(domain.VerbChannelDefault); err != nil {
		t.Fatal(err)
	}
	requireMissing(adapterRepoSetName)
	if err := s.publishAdapterFiles(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := read(adapterPolicyName); got != "claude 0 1 3\ncodex 0 0 0\n" {
		t.Fatalf("republished policy: %q", got)
	}
}

// The turn_end mirror lists only the profiles with tested turn_end evidence,
// so a Stop hook can skip waiting where the check is unavailable. An evidence
// import invalidates it before commit, like the policy mirror.
func TestTurnEndMirrorListsTestedProfiles(t *testing.T) {
	s, root := adapterFilesFixture(t)
	s.fixtureSQL(t, `
		INSERT INTO capability_evidence (vendor, vendor_version, platform, mode, manifest_rev, cap, level, result_ref, checked_at_ms) VALUES
		('claude', '2.1.327', 'darwin-arm64', 'print', 'claude-1', 'turn_end', 'tested', 'r', 1),
		('codex', '0.170.0', 'darwin-arm64', 'exec', 'codex-1', 'turn_end', 'candidate', 'r', 1),
		('claude', '2.1.400', 'darwin-arm64', 'print', 'claude-1', 'boundary', 'tested', 'r', 1)
	`)
	if err := s.publishAdapterFiles(t.Context()); err != nil {
		t.Fatal(err)
	}
	raw, err := root.ReadFile("turn_end")
	if err != nil || string(raw) != "claude 2.1.327 darwin-arm64 print claude-1\n" {
		t.Fatalf("turn_end mirror %q (%v)", raw, err)
	}
	if err := s.invalidateAdapterFile(domain.VerbAdapterEvidence); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Stat("turn_end"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an evidence import left the turn_end mirror in place: %v", err)
	}
}

// The hook policy names every adapter the build ships a manifest for, so a
// new manifest's hook is not off for want of a policy line.
func TestAdapterPolicyNamesEveryManifest(t *testing.T) {
	s, root := adapterFilesFixture(t)
	for _, name := range manifests.Names() {
		s.fixtureSQL(t, `INSERT INTO adapter_policy (adapter, enabled, reminder, epoch, changed_at_ms)
			VALUES ('`+name+`', 1, 0, 5, 1)`)
	}
	if err := s.publishAdapterFiles(t.Context()); err != nil {
		t.Fatal(err)
	}
	raw, err := root.ReadFile(adapterPolicyName)
	if err != nil {
		t.Fatal(err)
	}
	var want strings.Builder
	for _, name := range manifests.Names() {
		want.WriteString(name + " 1 0 5\n")
	}
	if string(raw) != want.String() {
		t.Fatalf("policy %q, want %q", raw, want.String())
	}
}
