package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/hook/manifests"
	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

func privateState(t *testing.T) *os.Root {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	state, err := spool.OpenServerRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { state.Close() })
	return state
}

// A prompt of a newer build's hook asks to recover its enrollment with a
// revision this dispatcher does not know. The dispatcher refuses it as any
// non-member callback, with the same exit and code, but says why, and
// owner doctor repeats it once. Healthy recoveries and other refusals are
// unchanged.
func TestANewerHooksRecoveryIsRefusedWithItsReason(t *testing.T) {
	s := reviewServer(t)
	s.state = privateState(t)
	s.ready.Instance = "instance-a"
	s.fixtureSQL(t, `INSERT INTO adapter_policy(adapter,enabled,reminder,epoch,changed_at_ms)
		VALUES ('claude',1,0,1,1)`)
	s.cfg.Resolve = func(context.Context, store.Tx, Arrival, wire.Request) (Resolution, error) {
		return Resolution{Caller: core.Caller{Role: core.RoleUnenrolled}, Vendor: "claude"}, nil
	}
	var handled int
	s.cfg.Decode = func(req wire.Request, _ Resolution) (domain.Request, error) {
		return domain.Request{OpKey: req.OpKey, Command: domain.Who{}}, nil
	}
	s.cfg.Handle = handlerFunc(func(context.Context, store.Tx, core.Caller, domain.Request) (core.Result, error) {
		handled++
		return domain.OK(wire.HookResult{})
	})
	send := func(revision, opKey string, epoch uint64) (core.Result, string) {
		t.Helper()
		args := wire.HookEventArgs{
			Profile:   wire.HookProfile{Adapter: "claude", ManifestRev: revision, VendorVersion: "2.1.327", Platform: "darwin-arm64", Mode: "print"},
			LaunchRef: "session", Fact: wire.HookPromptSubmitted, TurnRef: "t1", EventRef: "t1:UserPromptSubmit",
			HookAtMS: 1, WantOutput: true, EnrollRepoRoot: "/repo", Epoch: epoch,
		}
		data, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		r := s.fixtureRequest(t, Arrival{}, wire.Request{Verb: wire.VerbHookEvent, OpKey: opKey, Args: data})
		var refusal struct{ Message string }
		_ = json.Unmarshal(r.Data, &refusal)
		return r, refusal.Message
	}
	key := "hook:claude:session:t1:UserPromptSubmit"

	for range 2 {
		r, msg := send("claude-99", key, 1)
		if r.Status != core.ExitNotEnrolled || r.Code != core.CodeNotEnrolled || msg != UnknownRevisionMessage("claude-99") {
			t.Fatalf("newer hook: %+v %q", r, msg)
		}
	}
	if got := unknownRevisionWarnings(s.state, "instance-a"); !slices.Equal(got, []string{UnknownRevisionMessage("claude-99")}) {
		t.Fatalf("doctor warnings: %q", got)
	}
	// Other refusals keep the generic answer and record nothing.
	for _, c := range []struct {
		name, revision, key string
		epoch               uint64
	}{
		{"bad operation key", "claude-98", "hook:claude:session:other", 1},
		{"changed policy epoch", "claude-98", key, 2},
		{"revision not a token", "claude 98", key, 1},
		{"revision too long", "claude-" + strings.Repeat("9", maxRevisionToken), key, 1},
	} {
		if r, msg := send(c.revision, c.key, c.epoch); r.Status != core.ExitNotEnrolled || msg != "hook event requires a live member" {
			t.Errorf("%s: %+v %q", c.name, r, msg)
		}
	}
	if handled != 0 || len(unknownRevisionWarnings(s.state, "instance-a")) != 1 {
		t.Fatalf("refusals reached the domain (%d) or were recorded", handled)
	}
	// The current and the legacy revision still reach the domain to recover.
	for _, revision := range []string{"claude-3", "claude-1"} {
		if r, _ := send(revision, key, 1); r.Status != core.ExitOK {
			t.Fatalf("%s recovery: %+v", revision, r)
		}
	}
	if handled != 2 {
		t.Fatalf("healthy recoveries reached the domain %d times", handled)
	}
}

func TestOwnerDoctorNamesTheRevisionsTheLiveDispatcherRefused(t *testing.T) {
	state := privateState(t)
	if got := unknownRevisionWarnings(state, "instance-a"); got != nil {
		t.Fatalf("no file: %v", got)
	}
	var notes revisionNotes
	notes.note(state, "instance-a", "claude", "claude-9")
	notes.note(state, "instance-a", "claude", "claude-9")
	notes.note(state, "instance-a", "codex", "codex-7")
	want := []string{UnknownRevisionMessage("claude-9"), UnknownRevisionMessage("codex-7")}
	if got := unknownRevisionWarnings(state, "instance-a"); !slices.Equal(got, want) {
		t.Fatalf("deduplicated warnings: %q", got)
	}
	for i := range 2 * maxUnknownRevisions {
		notes.note(state, "instance-a", "claude", fmt.Sprintf("claude-x%d", i))
	}
	if got := unknownRevisionWarnings(state, "instance-a"); len(got) != maxUnknownRevisions || !slices.Equal(got[:2], want) {
		t.Fatalf("bounded warnings: %d %q", len(got), got)
	}
	// A restarted dispatcher has another ready instance, even with the same PID.
	if got := unknownRevisionWarnings(state, "instance-b"); got != nil {
		t.Fatalf("after a restart: %v", got)
	}
}

// The largest file the notes can write, eight entries of the longest adapter
// name and revision token, fits the reader's limit.
func TestTheLargestUnknownRevisionsFileIsRead(t *testing.T) {
	state := privateState(t)
	longest := ""
	for _, adapter := range manifests.Names() {
		if len(adapter) > len(longest) {
			longest = adapter
		}
	}
	instance := strings.Repeat("f", 64)
	var notes revisionNotes
	for i := range maxUnknownRevisions {
		revision := fmt.Sprintf("%d%s", i, strings.Repeat("-", maxRevisionToken-1))
		if !revisionToken(revision) {
			t.Fatalf("%q is not a token", revision)
		}
		notes.note(state, instance, longest, revision)
	}
	info, err := state.Stat(unknownRevisionsFile)
	if err != nil || info.Size() > unknownRevisionsLimit {
		t.Fatalf("file size %v, limit %d: %v", info, unknownRevisionsLimit, err)
	}
	if got := unknownRevisionWarnings(state, instance); len(got) != maxUnknownRevisions {
		t.Fatalf("read %d warnings of %d", len(got), maxUnknownRevisions)
	}
}
