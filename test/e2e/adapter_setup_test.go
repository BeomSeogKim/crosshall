//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/ident"
)

const setupClaudeOriginal = "{\n  \"z-user\": 7,\n  \"hooks\": {\"SessionStart\": [{\"hooks\":[{\"type\":\"command\",\"command\":\"claude-user-hook\"}]}]},\n  \"permissions\": {\"allow\":[\"Bash(existing)\"]},\n  \"a-user\": false\n}\n"
const setupCodexOriginal = "{\n \"z-user\": 8,\n \"hooks\": {\"SessionStart\": [{\"hooks\":[{\"type\":\"command\",\"command\":\"codex-user-hook\"}]}]},\n \"a-user\": true\n}\n"
const setupSessionPrefix = "synthetic-setup-"

type setupWorld struct {
	*world
	executable string
	env        []string
	paths      map[string]string
}

func newSetupWorld(t *testing.T) *setupWorld {
	t.Helper()
	w := newWorld(t)
	f := &setupWorld{world: w, paths: make(map[string]string)}
	binDir := filepath.Join(w.home, "owner's binaries")
	if err := os.Mkdir(binDir, 0700); err != nil {
		t.Fatal(err)
	}
	f.executable = filepath.Join(binDir, "crosshall")
	content, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.executable, content, 0700); err != nil {
		t.Fatal(err)
	}
	// A clean environment points every possible vendor probe at test data.
	f.env = append(w.baseEnv(), "PATH="+binDir)
	for _, fake := range []struct {
		adapter, homeVar, file, version, initial string
		mode                                     os.FileMode
	}{
		{"claude", "CLAUDE_CONFIG_DIR", "settings.json", "2.1.327 (Claude Code)", setupClaudeOriginal, 0640},
		{"codex", "CODEX_HOME", "hooks.json", "codex-cli 0.170.0", setupCodexOriginal, 0600},
	} {
		home := filepath.Join(w.home, fake.adapter+" test config")
		if err := os.Mkdir(home, 0700); err != nil {
			t.Fatal(err)
		}
		f.env = append(f.env, fake.homeVar+"="+home)
		f.paths[fake.adapter] = filepath.Join(home, fake.file)
		if err := os.WriteFile(f.paths[fake.adapter], []byte(fake.initial), fake.mode); err != nil {
			t.Fatal(err)
		}
		probe := "#!/bin/sh\n[ \"$1\" = --version ] || exit 9\nprintf '%s\\n' '" + fake.version + "'\n"
		if err := os.WriteFile(filepath.Join(binDir, fake.adapter), []byte(probe), 0700); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f *setupWorld) command(env []string, input string, args ...string) outcome {
	f.t.Helper()
	cmd := exec.CommandContext(f.t.Context(), f.executable, append([]string{"--json"}, args...)...)
	cmd.Dir, cmd.Env, cmd.Stdin = f.home, env, strings.NewReader(input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	var result outcome
	if err := cmd.Run(); err != nil {
		if process, ok := err.(*exec.ExitError); ok {
			result.exit = process.ExitCode()
		} else {
			f.t.Fatalf("crosshall %v: %v", args, err)
		}
	}
	result.stdout, result.stderr = stdout.String(), stderr.String()
	if err := json.Unmarshal(stdout.Bytes(), &result.obj); err != nil || result.obj == nil {
		f.t.Fatalf("crosshall %v: missing JSON result in %q (%v); stderr %s", args, result.stdout, err, result.stderr)
	}
	return result
}

func (f *setupWorld) apply(input string, args ...string) outcome {
	f.t.Helper()
	result := f.command(f.env, input, args...)
	if result.exit != 0 {
		f.t.Fatalf("crosshall %v: exit %d, stdout %s, stderr %s", args, result.exit, result.stdout, result.stderr)
	}
	return result
}

type setupFile struct {
	bytes []byte
	info  os.FileInfo
}

func readSetupFile(t *testing.T, path string) setupFile {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return setupFile{b, info}
}

func assertSetupFile(t *testing.T, path string, previous setupFile, sameInode bool) {
	t.Helper()
	current := readSetupFile(t, path)
	if !bytes.Equal(current.bytes, previous.bytes) || current.info.Mode() != previous.info.Mode() ||
		(sameInode && !os.SameFile(current.info, previous.info)) {
		t.Errorf("configuration %s changed: mode %v, was %v; same inode %t",
			filepath.Base(path), current.info.Mode(), previous.info.Mode(), os.SameFile(current.info, previous.info))
	}
}

type setupHook struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
	Timeout int      `json:"timeout"`
}

func setupHookFor(t *testing.T, file setupFile, event string) setupHook {
	t.Helper()
	var config struct {
		Hooks map[string][]struct {
			Hooks []setupHook `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(file.bytes, &config); err != nil {
		t.Fatal(err)
	}
	for _, group := range config.Hooks[event] {
		for _, hook := range group.Hooks {
			if strings.Contains(hook.Command, "crosshall") {
				return hook
			}
		}
	}
	t.Fatalf("no Crosshall hook for %s", event)
	return setupHook{}
}

func (f *setupWorld) invokeHook(repo, adapter, event, fields string) string {
	f.t.Helper()
	cmd := f.installedHookCommand(f.t.Context(), repo, adapter, event, fields)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil || stderr.Len() != 0 {
		f.t.Fatalf("invoke installed %s %s: %v, stderr %q", adapter, event, err, stderr.String())
	}
	return stdout.String()
}

func (f *setupWorld) installedHookCommand(ctx context.Context, repo, adapter, event, fields string) *exec.Cmd {
	f.t.Helper()
	hook := setupHookFor(f.t, readSetupFile(f.t, f.paths[adapter]), event)
	var cmd *exec.Cmd
	if adapter == "claude" {
		if hook.Command != f.executable || !slices.Equal(hook.Args, []string{"hook", adapter, event}) {
			f.t.Fatalf("Claude encoding: %+v", hook)
		}
		cmd = exec.CommandContext(ctx, hook.Command, hook.Args...)
	} else {
		if len(hook.Args) != 0 || !strings.HasSuffix(hook.Command, " hook codex "+event) {
			f.t.Fatalf("Codex encoding: %+v", hook)
		}
		cmd = exec.CommandContext(ctx, "/bin/sh", "-c", hook.Command)
	}
	if hook.Timeout != 1 {
		f.t.Errorf("%s %s timeout = %d", adapter, event, hook.Timeout)
	}
	payload := fmt.Sprintf(`{"hook_event_name":%q,"session_id":%q,"cwd":%q,%s}`, event, setupSessionPrefix+adapter, repo, fields)
	cmd.Dir, cmd.Env, cmd.Stdin = repo, f.env, strings.NewReader(payload)
	return cmd
}

func (f *setupWorld) enrollmentRepo() string {
	f.t.Helper()
	owner := f.owner()
	owner.must("owner", "channel", "create", "setup-check")
	repo := filepath.Join(f.home, "project")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0600); err != nil {
		f.t.Fatal(err)
	}
	owner.must("owner", "channel", "default", "setup-check", "--repo", repo)
	// A hook tells a session who it is only once it has company.
	f.member().must("join", "setup-check", "--as", "peer")
	return repo
}

func (f *setupWorld) enrollmentEvidence(adapter string) string {
	var stage string
	err := f.db().QueryRow(`SELECT CASE
		WHEN l.principal_id IS NULL THEN 'provisional leaf without principal'
		WHEN EXISTS (SELECT 1 FROM binding b WHERE b.principal_id = l.principal_id AND b.state = 'live') THEN 'live binding'
		ELSE 'linked leaf without live binding'
		END FROM leaf l WHERE l.vendor = ? AND l.key = ?`, adapter, setupSessionPrefix+adapter).Scan(&stage)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		stage = "no registered leaf"
	case err != nil:
		stage = err.Error()
	}
	return fmt.Sprintf("SessionStart counter %q, enrollment stage %q", f.hookCounter(adapter, "SessionStart"), stage)
}

func (f *setupWorld) hookCounter(adapter, event string) string {
	directory := filepath.Join(f.rendezvous(), "hooks")
	days, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return "no counter record"
	}
	if err != nil {
		return err.Error()
	}
	for _, day := range slices.Backward(days) {
		data, err := os.ReadFile(filepath.Join(directory, day.Name()))
		if err != nil {
			return err.Error()
		}
		for _, line := range slices.Backward(strings.Split(string(data), "\n")) {
			if fields := strings.Fields(line); len(fields) == 4 && fields[0] == adapter && fields[1] == event {
				return line
			}
		}
	}
	return "no counter record"
}

func TestAdapterSetupReportsStalledEnrollment(t *testing.T) {
	f := newSetupWorld(t)
	f.apply("", "setup", "codex", "--apply")
	repo := f.enrollmentRepo()
	pids := f.dispatcherPIDs()
	if len(pids) != 1 {
		t.Fatalf("want one fixture dispatcher, got %v", pids)
	}
	dispatcher, err := os.FindProcess(pids[0])
	if err != nil {
		t.Fatal(err)
	}
	pauseFixtureProcess(t, dispatcher)
	out := f.invokeHook(repo, "codex", "SessionStart", `"source":"startup"`)
	evidence := f.enrollmentEvidence("codex")
	resumeFixtureProcess(t, dispatcher)
	t.Log(evidence)
	if out != "" || !strings.Contains(evidence, `enrollment stage "no registered leaf"`) {
		t.Fatalf("stalled SessionStart returned %q with %s", out, evidence)
	}
	if !strings.Contains(evidence, `counter "codex SessionStart deadline `) &&
		!strings.Contains(evidence, `counter "no counter record"`) {
		t.Fatalf("unexpected hook counter for a timed-out enrollment: %s", evidence)
	}
}

func TestAdapterSetupLifecycle(t *testing.T) {
	f := newSetupWorld(t)
	originals := make(map[string]setupFile)
	for adapter, path := range f.paths {
		originals[adapter] = readSetupFile(t, path)
	}
	noWrites := func() {
		t.Helper()
		for adapter, path := range f.paths {
			assertSetupFile(t, path, originals[adapter], true)
		}
		if _, err := os.Stat(f.state); !os.IsNotExist(err) {
			t.Fatalf("preview or refusal created dispatcher state: %v", err)
		}
	}

	preview := f.apply("", "setup", "--dry-run")
	if preview.obj["dry_run"] != true || preview.obj["applied"] != false {
		t.Fatalf("unexpected dry-run result: %s", preview.stdout)
	}
	for _, want := range []string{"2.1.327", "0.170.0", "SessionStart", `"kind":"read"`, `"kind":"send"`} {
		if !strings.Contains(preview.stdout, want) {
			t.Errorf("dry run omitted %q", want)
		}
	}
	agentEnv := append(slices.Clone(f.env), "CROSSHALL_ROUTE=synthetic-agent")
	refused := f.command(agentEnv, "", "setup", "--dry-run")
	if refused.exit == 0 || refused.obj["code"] != "owner_only" {
		t.Fatalf("agent dry run was not refused: %d %s", refused.exit, refused.stdout)
	}
	// rc.6 piped answers are no longer read: an unnamed setup without a
	// terminal prints the scripted form, and a named one only previews.
	if scripted := f.command(f.env, "yes\nyes\nno\n", "setup"); scripted.exit != 2 || scripted.obj["code"] != "usage" ||
		!strings.Contains(scripted.stdout, "crosshall setup claude codex --apply") {
		t.Fatalf("piped setup: exit %d, %s", scripted.exit, scripted.stdout)
	}
	if named := f.apply("yes\n", "setup", "claude", "codex"); !strings.Contains(named.stdout, `"outcome":"previewed"`) {
		t.Fatalf("named setup without a terminal: %s", named.stdout)
	}
	noWrites()

	applied := f.apply("", "setup", "claude", "codex", "--apply")
	if strings.Count(applied.stdout, `"outcome":"installed"`) != 2 {
		t.Fatalf("setup did not apply: %s", applied.stdout)
	}
	installed := make(map[string]setupFile)
	for adapter, path := range f.paths {
		file := readSetupFile(t, path)
		installed[adapter] = file
		if file.info.Mode() != originals[adapter].info.Mode() {
			t.Errorf("%s file mode = %v, want %v", adapter, file.info.Mode(), originals[adapter].info.Mode())
		}
		pos := -1
		for _, key := range []string{`"z-user"`, `"hooks"`, `"a-user"`} {
			next := bytes.Index(file.bytes, []byte(key))
			if next <= pos {
				t.Fatalf("%s lost key order at %s: %s", adapter, key, file.bytes)
			}
			pos = next
		}
		if !bytes.Contains(file.bytes, []byte(adapter+"-user-hook")) {
			t.Errorf("setup removed the existing %s hook", adapter)
		}
	}
	// --apply adds no allow rules and keeps the owner's own.
	if claude := installed["claude"].bytes; !bytes.Contains(claude, []byte("Bash(existing)")) ||
		bytes.Contains(claude, []byte("Bash(crosshall read")) || bytes.Contains(claude, []byte("Bash(crosshall note")) {
		t.Fatalf("Claude permissions after --apply: %s", claude)
	}
	journalPath := filepath.Join(f.state, "install-journal.json")
	journal := readSetupFile(t, journalPath)
	if journal.info.Mode().Perm() != 0600 {
		t.Errorf("install journal mode = %v", journal.info.Mode())
	}
	f.apply("", "setup", "claude", "codex", "--apply")
	for adapter, path := range f.paths {
		assertSetupFile(t, path, installed[adapter], true)
	}
	assertSetupFile(t, journalPath, journal, true)

	repo := f.enrollmentRepo()
	writer := f.member()
	writer.must("join", "setup-check", "--as", "writer")
	for _, adapter := range []string{"claude", "codex"} {
		alias := adapter + "1"
		f.waitForEnrollmentWelcome(repo, adapter, "SessionStart", func(string) string { return `"source":"startup"` })
		ids := notes(t, writer, alias, 1)
		digest := (ident.Identity{Vendor: adapter, Key: setupSessionPrefix + adapter}).Digest()
		leaf := filepath.Join(f.rendezvous(), "m", digest[:16])
		deadline := time.Now().Add(fixtureEventTimeout)
		var got string
		for attempt := 0; time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if len(fixtureQueuedPackets(t, leaf)) != 0 {
				continue
			}
			attempt++
			turn := fmt.Sprintf("turn-on-%d", attempt)
			fields := fmt.Sprintf(`"prompt_id":%q,"turn_id":%q`, turn, turn)
			if prompt := f.invokeHook(repo, adapter, "UserPromptSubmit", fields); prompt != "" {
				got = hostContext(t, prompt)
				break
			}
		}
		if got != pendingContext(ids) {
			f.logDispatcher()
			t.Fatalf("%s installed hook pending mail %q, want %q; %s",
				adapter, got, pendingContext(ids), f.enrollmentEvidence(adapter))
		}
		notes(t, writer, alias, 1)
	}

	f.apply("", "setup", "off")
	for adapter, path := range f.paths {
		assertSetupFile(t, path, installed[adapter], true)
		out := f.invokeHook(repo, adapter, "UserPromptSubmit", `"prompt_id":"turn-off","turn_id":"turn-off"`)
		if out != "" {
			t.Errorf("%s hook still has an effect after setup off: %q", adapter, out)
		}
	}
	assertSetupFile(t, journalPath, journal, true)
	db := f.db()
	for _, adapter := range []string{"claude", "codex"} {
		var enabled bool
		if err := db.QueryRow("SELECT enabled FROM adapter_policy WHERE adapter = ?", adapter).Scan(&enabled); err != nil || enabled {
			t.Errorf("%s policy enabled = %t: %v", adapter, enabled, err)
		}
	}

	f.apply("", "uninstall", "--dry-run")
	for adapter, path := range f.paths {
		assertSetupFile(t, path, installed[adapter], true)
	}
	assertSetupFile(t, journalPath, journal, true)
	f.apply("", "uninstall", "--apply")
	for adapter, path := range f.paths {
		assertSetupFile(t, path, originals[adapter], false)
	}
}

func TestAdapterUninstallWithoutHooksDoesNotWritePolicy(t *testing.T) {
	f := newSetupWorld(t)
	originals := make(map[string]setupFile)
	for adapter, path := range f.paths {
		originals[adapter] = readSetupFile(t, path)
	}
	f.apply("", "owner", "start")
	for _, args := range [][]string{{"uninstall"}, {"uninstall", "claude"}} {
		out := f.apply("yes\n", args...)
		if out.stderr != "" || out.obj["kind"] != "uninstall" || out.obj["ok"] != true || out.obj["applied"] == true {
			t.Fatalf("%v wrote a plan or applied changes: %s, stderr %q", args, out.stdout, out.stderr)
		}
	}
	for adapter, path := range f.paths {
		assertSetupFile(t, path, originals[adapter], true)
	}
	if _, err := os.Stat(filepath.Join(f.state, "install-journal.json")); !os.IsNotExist(err) {
		t.Errorf("uninstall created the install journal: %v", err)
	}
	db := f.db()
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM adapter_policy").Scan(&count); err != nil || count != 0 {
		t.Fatalf("uninstall wrote %d policies: %v", count, err)
	}
	f.apply("", "owner", "adapter", "set", "claude", "--enabled", "on")
	out := f.apply("", "uninstall", "--apply")
	if out.obj["applied"] != true {
		t.Fatalf("uninstall failed to disable a saved policy: %s", out.stdout)
	}
	var enabled bool
	if err := db.QueryRow("SELECT enabled FROM adapter_policy WHERE adapter = 'claude'").Scan(&enabled); err != nil || enabled {
		t.Fatalf("Claude policy after uninstall was enabled = %t: %v", enabled, err)
	}
}

func TestAdapterSetupRejectsAnExplicitlyEmptyVendorHome(t *testing.T) {
	f := newSetupWorld(t)
	path := f.paths["claude"]
	original := readSetupFile(t, path)
	env := slices.DeleteFunc(slices.Clone(f.env), func(kv string) bool {
		return strings.HasPrefix(kv, "CLAUDE_CONFIG_DIR=")
	})
	env = append(env, "CLAUDE_CONFIG_DIR=")
	result := f.command(env, "", "setup", "claude", "--apply")
	if result.exit == 0 || !strings.Contains(result.stdout, "CLAUDE_CONFIG_DIR") {
		t.Fatalf("setup with empty vendor home: exit %d, %s", result.exit, result.stdout)
	}
	assertSetupFile(t, path, original, true)
	if _, err := os.Stat(filepath.Join(f.home, ".claude")); !os.IsNotExist(err) {
		t.Fatalf("setup fell back to a default settings directory: %v", err)
	}
}

func TestAdapterUninstallLeavesEditedEntry(t *testing.T) {
	f := newSetupWorld(t)
	f.apply("", "setup", "claude", "codex", "--apply")
	path := f.paths["claude"]
	file := readSetupFile(t, path)
	unchanged := []byte(`"args":["hook","claude","SessionStart"],"timeout":1`)
	changed := []byte(`"args":["hook","claude","SessionStart"],"timeout":27`)
	if bytes.Count(file.bytes, unchanged) != 1 {
		t.Fatal("expected one installed Claude SessionStart hook to edit")
	}
	if err := os.WriteFile(path, bytes.Replace(file.bytes, unchanged, changed, 1), file.info.Mode().Perm()); err != nil {
		t.Fatal(err)
	}
	doctor := f.apply("", "owner", "doctor")
	type adapterDiagnostic struct {
		Adapter      string `json:"adapter"`
		Version      string `json:"version"`
		InstallState string `json:"install_state"`
		Drifted      int    `json:"drifted"`
	}
	var diagnostics struct {
		Adapters []adapterDiagnostic `json:"adapters"`
	}
	if err := json.Unmarshal([]byte(doctor.stdout), &diagnostics); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(diagnostics.Adapters, func(status adapterDiagnostic) bool {
		return status.Adapter == "claude" && status.Version == "2.1.327" &&
			status.InstallState == "drifted" && status.Drifted >= 1
	}) {
		t.Errorf("doctor omitted the vendor version and edited hook drift: %s", doctor.stdout)
	}
	result := f.apply("", "uninstall", "--apply")
	if !strings.Contains(result.stdout, `"kept":[{"event":"SessionStart"`) {
		t.Fatalf("uninstall did not report the edited entry: %s", result.stdout)
	}
	after := readSetupFile(t, path)
	if got := setupHookFor(t, after, "SessionStart"); got.Timeout != 27 {
		t.Errorf("uninstall changed the edited hook: %+v", got)
	}
	if !bytes.Contains(after.bytes, []byte("claude-user-hook")) ||
		bytes.Contains(after.bytes, []byte(`"args":["hook","claude","UserPromptSubmit"]`)) {
		t.Errorf("uninstall must keep the unrelated hook and remove the unchanged install: %s", after.bytes)
	}
}
