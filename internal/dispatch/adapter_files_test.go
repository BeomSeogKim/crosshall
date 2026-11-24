package dispatch

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/hook"
	"github.com/BeomSeogKim/crosshall/internal/hook/manifests"
	"github.com/BeomSeogKim/crosshall/internal/ident"
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
	if s.state, err = os.OpenRoot(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.state.Close() })
	return s, root
}

func adapterLeafFixture(t *testing.T, s *server, root *os.Root, registered bool) *os.Root {
	t.Helper()
	digest := (ident.Identity{Vendor: "codex", Key: "synthetic-key"}).Digest()
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

func TestHookJoinHintReceiptIsBoundToTheLiveExchange(t *testing.T) {
	s, root := adapterFilesFixture(t)
	leaf := adapterLeafFixture(t, s, root, true)
	s.fixtureSQL(t, `INSERT INTO channel (id, name, state, created_at_ms) VALUES (1, 'dev', 'active', 1);
		INSERT INTO principal (id, kind, vendor, created_at_ms) VALUES (2, 'session', 'claude', 1);
		INSERT INTO binding (id, principal_id, channel_id, alias, joined_at_ms, roster_baseline_seq, roster_ack_seq)
		VALUES (2, 1, 1, 'fixture', 1, 2, 2), (3, 2, 1, 'peer', 1, NULL, 2);
		INSERT INTO roster_change (channel_id, seq, principal_id, field) VALUES (1, 3, 1, 'joined');
		INSERT INTO adapter_policy (adapter, enabled, reminder, epoch, changed_at_ms) VALUES ('codex', 1, 0, 1, 1);
		INSERT INTO adapter_launch (principal_id, launch_ref, started_hook_at_ms, profile, verified)
		VALUES (1, 'pid-1', 1, '{}', 1);`)
	principal, binding := int64(1), int64(2)
	member := core.Caller{Role: core.RoleMember, PrincipalID: &principal, BindingID: &binding}
	offer := wire.HookJoinHintAck{Generation: "test-gen", BindingID: 2, RouteEpoch: 1, RosterSeq: 3}
	if err := publishJoinHintOffer(leaf, &offer); err != nil {
		t.Fatal(err)
	}
	concurrent := offer
	concurrent.Offer = ""
	if err := publishJoinHintOffer(leaf, &concurrent); err != nil || !concurrent.SameOffer(offer) {
		t.Fatalf("concurrent live offer changed its token: %+v (%v)", concurrent, err)
	}
	check := func(ack wire.HookJoinHintAck, want bool) {
		t.Helper()
		data, err := json.Marshal(ack)
		if err != nil {
			t.Fatal(err)
		}
		if err := leaf.WriteFile("seen-join-hint", data, 0600); err != nil {
			t.Fatal(err)
		}
		if err := s.db.Update(t.Context(), func(tx store.Tx) error {
			if err := acknowledgeHookJoinHint(t.Context(), tx, Arrival{Dir: leaf}, wire.Request{Generation: "test-gen"}, member); err != nil {
				return err
			}
			// A receipt records that the start line arrived and moves the
			// snapshot; the rules stay due with the member's first command.
			var got, rules bool
			var snapshot int
			if err := tx.QueryRowContext(t.Context(), "SELECT start_line_seen, hint_acked, roster_baseline_seq FROM binding WHERE id = 2").
				Scan(&got, &rules, &snapshot); err != nil {
				return err
			}
			if got != want || rules || want && uint64(snapshot) != ack.RosterSeq || !want && snapshot != 2 {
				t.Fatalf("receipt %+v changed ack to %t (rules %t) snapshot %d, want ack %t", ack, got, rules, snapshot, want)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	check(wire.HookJoinHintAck{Generation: "old-gen", BindingID: 2, RouteEpoch: 1, RosterSeq: 3, Offer: offer.Offer}, false)
	check(wire.HookJoinHintAck{Generation: "test-gen", BindingID: 1, RouteEpoch: 1, RosterSeq: 3, Offer: offer.Offer}, false)
	check(wire.HookJoinHintAck{Generation: "test-gen", BindingID: 2, RouteEpoch: 2, RosterSeq: 3, Offer: offer.Offer}, false)
	check(wire.HookJoinHintAck{Generation: "test-gen", BindingID: 2, RouteEpoch: 1, RosterSeq: 4, Offer: offer.Offer}, false)

	lock, err := spool.OpenServerFile(leaf, "join-hint.lock", os.O_CREATE|os.O_RDWR)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	profile := wire.HookProfile{Adapter: "codex", ManifestRev: "codex-1", Platform: "darwin-arm64", VendorVersion: "unverified", Mode: "unverified"}
	now := time.Now().UnixMilli()
	svc := &domain.Service{}
	end := domain.HookEvent{HookEventArgs: wire.HookEventArgs{
		Profile: profile, LaunchRef: "pid-1", LaunchVerified: true, EventRef: "end", Fact: wire.HookSessionEnded, HookAtMS: now - 2, Epoch: 1,
	}}
	if err := s.db.Update(ctx, func(tx store.Tx) error {
		result, err := svc.Handle(ctx, tx, member, domain.Request{Command: end, OpKey: "end", Vendor: "codex"})
		if err != nil || result.Status != core.ExitOK {
			t.Fatalf("clean session end: %+v (%v)", result, err)
		}
		return s.applyAdapterStamps(ctx, tx)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := leaf.Lstat(hook.JoinHintOfferFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("clean departure did not revoke the offer while output held the lock: %v", err)
	}
	check(offer, false)
	resume := domain.HookEnroll{HookEnrollArgs: wire.HookEnrollArgs{
		Profile: profile, LaunchRef: "pid-2", LaunchVerified: true, EventRef: "resume", Source: "resume", HookAtMS: now - 1, Epoch: 1,
	}}
	var after wire.HookResult
	if err := s.db.Update(ctx, func(tx store.Tx) error {
		caller := core.Caller{Role: core.RoleUnenrolled, PrincipalID: &principal}
		result, err := svc.Handle(ctx, tx, caller, domain.Request{Command: resume, OpKey: "resume", Vendor: "codex"})
		if err != nil || result.Status != core.ExitOK {
			t.Fatalf("hook resume: %+v (%v)", result, err)
		}
		if err := s.addHookAck(ctx, tx, Arrival{Dir: leaf}, wire.Request{Verb: wire.VerbHookEnroll, Generation: offer.Generation}, &result); err != nil {
			return err
		}
		return json.Unmarshal(result.Data, &after)
	}); err != nil {
		t.Fatal(err)
	}
	if after.Enrollment != "restored" || after.JoinHintAck == nil || after.JoinHintAck.RouteEpoch != offer.RouteEpoch ||
		after.JoinHintAck.Offer == offer.Offer || after.JoinHintAck.Offer == "" {
		t.Fatalf("hook rejoin did not retain pending guidance with a fresh token: %+v", after.JoinHintAck)
	}
	check(offer, false)
	check(*after.JoinHintAck, true)
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
	if got := read(adapterPolicyName); got != "agy 0 0 0\nclaude 1 1 2\ncodex 0 0 0\n" {
		t.Fatalf("initial policy: %q", got)
	}
	// The state root keeps the same policy for a hook that finds the
	// rendezvous cleared by a reboot.
	if copied, err := s.state.ReadFile(hook.StatePolicyName); err != nil || string(copied) != read(adapterPolicyName) {
		t.Fatalf("state root policy copy: %q, %v", copied, err)
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
		if _, err := s.state.Lstat(hook.StatePolicyName); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the state root still has the old policy: %v", err)
		}
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
	if got := read(adapterPolicyName); got != "agy 0 0 0\nclaude 0 1 3\ncodex 0 0 0\n" {
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

// The hook policy file carries enabled, reminder and the epoch only, so a
// publish or turn-end switch change, which hooks do not read from it,
// leaves it in place; a change it carries still republishes it.
func TestAdapterSwitchOnlyChangesKeepThePolicyFile(t *testing.T) {
	s, root := adapterFilesFixture(t)
	s.fixtureSQL(t, `INSERT INTO adapter_policy (adapter, enabled, reminder, epoch, changed_at_ms) VALUES ('claude', 1, 0, 2, 1)`)
	s.cfg.Resolve = func(context.Context, store.Tx, Arrival, wire.Request) (Resolution, error) {
		return Resolution{Caller: core.Caller{Role: core.RoleOwner}}, nil
	}
	var command domain.AdapterSet
	s.cfg.Decode = func(req wire.Request, _ Resolution) (domain.Request, error) {
		return domain.Request{OpKey: req.OpKey, Command: command}, nil
	}
	const sentinel = "sentinel policy\n"
	s.cfg.Handle = handlerFunc(func(context.Context, store.Tx, core.Caller, domain.Request) (core.Result, error) {
		for _, dir := range []*os.Root{root, s.state} {
			name := adapterPolicyName
			if dir == s.state {
				name = hook.StatePolicyName
			}
			if raw, err := dir.ReadFile(name); err != nil || string(raw) != sentinel {
				t.Fatalf("policy file %s during the change: %q, %v", name, raw, err)
			}
		}
		return domain.OK(store.AdapterPolicy{Adapter: "claude"})
	})
	plant := func() {
		t.Helper()
		if err := root.WriteFile(adapterPolicyName, []byte(sentinel), 0600); err != nil {
			t.Fatal(err)
		}
		if err := s.state.WriteFile(hook.StatePolicyName, []byte(sentinel), 0600); err != nil {
			t.Fatal(err)
		}
	}
	off, on := "off", true
	for i, c := range []struct {
		set       wire.AdapterPolicyArgs
		republish bool
	}{
		{wire.AdapterPolicyArgs{Adapter: "claude", TurnEnd: &off}, false},
		{wire.AdapterPolicyArgs{Adapter: "claude", Publish: &off}, false},
		{wire.AdapterPolicyArgs{Adapter: "claude", Enabled: &on, TurnEnd: &off}, true},
	} {
		plant()
		command = domain.AdapterSet{AdapterPolicyArgs: c.set}
		req := wire.Request{Verb: wire.VerbAdapterSet, OpKey: fmt.Sprintf("switch-%d", i)}
		if got := s.fixtureRequest(t, Arrival{Owner: true}, req); got.Status != core.ExitOK {
			t.Fatalf("%+v: %+v", c.set, got)
		}
		raw, err := root.ReadFile(adapterPolicyName)
		if err != nil || (string(raw) == sentinel) == c.republish {
			t.Fatalf("%+v: policy file after the change %q (%v), republished %v", c.set, raw, err, c.republish)
		}
	}
}

func TestPublishReadyRefreshesDirtyNoticeFromUpgrade(t *testing.T) {
	s, root := adapterFilesFixture(t)
	leaf := adapterLeafFixture(t, s, root, true)
	initial, err := s.publishAdapterStamp(leaf, "test-gen", true, true)
	if err != nil {
		t.Fatal(err)
	}
	s.fixtureSQL(t, "INSERT INTO adapter_dirty(principal_id, notice) VALUES (1,1)")
	if err := s.publishReady(t.Context(), root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.withdrawReady)
	after, ok := s.adapterStamp(leaf, "test-gen")
	if !ok || after.NoticeSeq <= initial.NoticeSeq || after.ReminderSeq != initial.ReminderSeq {
		t.Fatalf("stamp at readiness %+v, before upgrade %+v", after, initial)
	}
}
