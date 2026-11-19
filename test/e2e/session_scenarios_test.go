//go:build e2e

package e2e

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

func TestFirstJoinReplaysWithoutAcknowledgment(t *testing.T) {
	w := newWorld(t)
	w.owner().must("owner", "channel", "create", "drafts")
	member := w.member()
	const key = "first-join"
	client := spool.Client{StateRoot: w.state, Rendezvous: w.rendezvous()}
	id := ident.Identity{Vendor: "route", Key: member.route}
	// The caller loses the result before rendering or acknowledging it.
	first, err := client.Member(t.Context(), id, wire.Request{
		Verb: "join", OpKey: key, Args: json.RawMessage(`{"channel":"drafts","alias":"reviewer"}`),
	})
	if err != nil || first.Status != core.ExitOK {
		t.Fatalf("first join: exit %d, %v", first.Status, err)
	}
	if _, err := os.Stat(filepath.Join(member.leaf(), "ack")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected acknowledgment after first join: %v", err)
	}

	db := w.db()
	checkMembership := func() {
		t.Helper()
		var principals, bindings, operations int
		err := db.QueryRow(`SELECT
			(SELECT count(*) FROM principal WHERE kind = 'session'),
			(SELECT count(*) FROM binding),
			(SELECT count(*) FROM op JOIN binding USING (principal_id)
			 WHERE op_key = ? AND verb = 'join' AND binding.alias = 'reviewer')`, key).
			Scan(&principals, &bindings, &operations)
		if err != nil || principals != 1 || bindings != 1 || operations != 1 {
			t.Fatalf("first join persisted %d principals, %d bindings, %d matching operations: %v",
				principals, bindings, operations, err)
		}
	}
	checkMembership()
	replay := member.must("join", "drafts", "--as", "reviewer", "--key", key)
	hint, _ := replay.obj["hint"].(string)
	if replay.obj["restored"] != false || replay.obj["alias"] != "reviewer" || replay.obj["channel"] != "drafts" || hint == "" {
		t.Fatalf("wanted the original first-join result, got %s", replay.stdout)
	}
	checkMembership()
}

func TestMarkerOnlyOwnerRefusalLeavesSpoolEmpty(t *testing.T) {
	w := newWorld(t)
	w.owner().must("owner", "start")
	w.kill() // Any published request would remain available for inspection.
	marked := &participant{w: w, extra: []string{"CODEX_SESSION_ID=fixture"}}
	got := marked.run("owner", "channel", "create", "drafts")
	if got.exit != 3 || got.obj["code"] != "owner_only" {
		t.Fatalf("owner command with agent marker: exit %d, %s", got.exit, got.stdout)
	}
	for _, dir := range []string{"in", "work", "out"} {
		entries, err := os.ReadDir(filepath.Join(w.state, "owner", dir))
		if err != nil || len(entries) != 0 {
			t.Fatalf("owner spool %s after refusal: %v, %v", dir, entries, err)
		}
	}
}

func TestMalformedRouteAtCLI(t *testing.T) {
	w := newWorld(t)
	p := &participant{w: w, route: "invalid-route"}
	if got := p.run("whoami"); got.exit != 4 || got.obj["code"] != "not_enrolled" {
		t.Fatalf("whoami with malformed route: exit %d, %s", got.exit, got.stdout)
	}
}

func TestRunStartsChildWithOnlyFreshRouteIdentity(t *testing.T) {
	w := newWorld(t)
	parent := w.member()
	vendors := []string{"CLAUDE_CODE_SESSION_ID", "CODEX_THREAD_ID", "ANTIGRAVITY_CONVERSATION_ID"}
	for _, name := range vendors {
		parent.extra = append(parent.extra, name+"=fixture")
	}
	cmd := exec.Command(binary, "run", "--", "env")
	cmd.Dir, cmd.Env = w.home, parent.env()
	output, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	var routes []string
	for _, line := range strings.Split(string(output), "\n") {
		name, value, _ := strings.Cut(line, "=")
		if slices.Contains(vendors, name) {
			t.Fatalf("run passed %s to the child", name)
		}
		if name == "CROSSHALL_ROUTE" {
			routes = append(routes, value)
		}
	}
	if len(routes) != 1 || routes[0] == parent.route {
		t.Fatal("child did not receive exactly one fresh route")
	}
	route, err := hex.DecodeString(routes[0])
	if err != nil || len(route) != 16 {
		t.Fatal("child received an invalid route")
	}
}

func TestDevBinaryRefusesDefaultStateDirectory(t *testing.T) {
	w := newWorld(t)
	w.state = filepath.Join(w.home, ".local", "state", "crosshall")
	cmd := exec.Command(binary, "--json", "owner", "start")
	cmd.Dir = w.home
	cmd.Env = []string{"HOME=" + w.home, "PATH=/usr/bin:/bin", "TMPDIR=" + w.home}
	output, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 || !bytes.Contains(output, []byte("CROSSHALL_STATE_DIR")) {
		t.Fatalf("development start using the default state: %v, %s", err, output)
	}
	if _, err := os.Stat(w.state); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("default state directory after refused start: %v", err)
	}
}

func TestDevClockRefusesPhysicalDefaultBeforeInspectingStore(t *testing.T) {
	w := newWorld(t)
	root := filepath.Join(w.home, ".local", "state", "crosshall")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	err = db.Update(t.Context(), func(tx store.Tx) error {
		_, err := tx.ExecContext(t.Context(), `INSERT INTO channel (name, created_at_ms) VALUES ('existing', 1000)`)
		return err
	})
	if err := errors.Join(err, db.Close()); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "store.sqlite3")
	before, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}

	homeAlias := filepath.Join(t.TempDir(), "home")
	if err := os.Symlink(w.home, homeAlias); err != nil {
		t.Fatal(err)
	}
	w.state = filepath.Join(homeAlias, ".local", "state", "crosshall")
	owner := w.owner()
	owner.extra = []string{"CROSSHALL_TEST_CLOCK_OFFSET=2h"}
	for _, verb := range []string{"doctor", "start"} {
		out := owner.run("owner", verb)
		message, _ := out.obj["message"].(string)
		if out.exit != 1 || out.obj["code"] != "local_error" || !strings.Contains(message, "outside the default state root") {
			t.Fatalf("owner %s did not refuse the default state root: exit %d, %s", verb, out.exit, out.stdout)
		}
	}
	after, err := os.ReadFile(file)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("default store changed after refusal: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || entries[0].Name() != "store.sqlite3" {
		t.Fatalf("unexpected files in checkpointed default root: %v (%v)", entries, err)
	}
}

func TestDevDoctorStillRefusesLinkedFinalDirectory(t *testing.T) {
	w := newWorld(t)
	if err := os.Symlink(t.TempDir(), w.state); err != nil {
		t.Fatal(err)
	}
	out := w.owner().run("owner", "doctor")
	if out.exit != 1 || !strings.Contains(out.stdout, "directory state must be owned by this user") {
		t.Fatalf("doctor did not reject a link at the state root: exit %d, %s", out.exit, out.stdout)
	}
}

func TestAntigravityVersionWarningAtJoinAndInDiagnostics(t *testing.T) {
	w := newWorld(t)
	owner := w.owner()
	owner.must("owner", "channel", "create", "drafts")
	const warning = "identity variable unverified on agy: CLI version unknown (language server cli-1.5.10)"
	for _, format := range []string{"plain", "json"} {
		p := &participant{w: w, extra: []string{"ANTIGRAVITY_CONVERSATION_ID=agy-test-" + format, "ANTIGRAVITY_LS_VERSION=cli-1.5.10"}}
		check := func(caller *participant, count int, args ...string) {
			t.Helper()
			var printed string
			if format == "plain" {
				printed = caller.mustPlain(args...)
			} else {
				got := caller.must(args...)
				var data struct{ Warnings []string }
				if err := json.Unmarshal([]byte(got.stdout), &data); err != nil {
					t.Fatal(err)
				}
				printed = strings.Join(data.Warnings, "\n")
			}
			if got := strings.Count(printed, warning); got != count {
				t.Errorf("%s %v: got warning %d times, want %d: %s", format, args, got, count, printed)
			}
		}
		check(p, 1, "join", "drafts", "--as", format, "--key", "first")
		check(p, 0, "who")
		check(p, 0, "join", "drafts")
		check(p, 0, "join", "drafts", "--as", format, "--key", "first")
		check(p, 1, "whoami")
		check(owner, 1, "owner", "doctor")
		p.must("leave")
		check(p, 0, "join", "drafts")
	}
}

func TestAntigravityJoinReplayAfterLeafRecreation(t *testing.T) {
	w := newWorld(t)
	w.owner().must("owner", "channel", "create", "drafts")
	id := ident.Identity{Vendor: "agy", Key: "agy-replay"}
	member := &participant{w: w, extra: []string{"ANTIGRAVITY_CONVERSATION_ID=" + id.Key}}
	join := []string{"join", "drafts", "--as", "editor", "--key", "first"}
	leaf := filepath.Join(w.rendezvous(), "m", id.Digest()[:16])
	recreateOnNextJoin := func(name string) {
		t.Helper()
		if err := os.Rename(leaf, filepath.Join(w.rendezvous(), name)); err != nil {
			t.Fatal(err)
		}
	}

	// The join commits, but its first response cannot be displayed.
	readOnly, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()
	cmd := exec.Command(binary, join...)
	cmd.Dir, cmd.Env, cmd.Stdout = w.home, member.env(), readOnly
	var exit *exec.ExitError
	if err := cmd.Run(); !errors.As(err, &exit) || exit.ExitCode() != int(core.ExitError) {
		t.Fatalf("join with unwritable stdout: %v", err)
	}
	recreateOnNextJoin("before-display")
	first := member.must(join...)
	const warning = "identity variable unverified on agy: CLI version unknown"
	hint, _ := first.obj["hint"].(string)
	if hint == "" || strings.Count(first.stdout, warning) != 1 {
		t.Fatalf("first displayed replay lost its warning: %s", first.stdout)
	}

	member.must("who") // Deliver the acknowledgement from that display.
	var acked bool
	if err := w.db().QueryRow("SELECT hint_acked FROM binding WHERE alias='editor'").Scan(&acked); err != nil || !acked {
		t.Fatalf("expected durable join acknowledgement, got %t: %v", acked, err)
	}
	for _, state := range []string{"live", "departed"} {
		if state == "departed" {
			member.must("leave")
		}
		recreateOnNextJoin(state)
		replay := member.must(join...)
		if replay.obj["hint"] != hint || replay.obj["restored"] != false || strings.Contains(replay.stdout, warning) {
			t.Errorf("%s binding: acknowledged replay should keep the original join and omit the warning: %s", state, replay.stdout)
		}
	}
}
