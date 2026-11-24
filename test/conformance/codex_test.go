//go:build conformance

// From the repository root:
//
//	go test -count=1 -tags conformance -run '^TestCodexExecConformance$' ./test/conformance
//
// CROSSHALL_CONFORMANCE_OUT selects a directory for the resulting evidence.
// An output-only run on a branch without the importer requires
// CROSSHALL_CONFORMANCE_OUTPUT_ONLY=1.
package conformance

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/ident"
	_ "modernc.org/sqlite" // Read the disposable ledger with Crosshall's own driver.
)

var codexMarkers = map[string]string{
	"hint": "[crosshall] Joined #fixture", "roster": "@gate-peer",
	"pending": "[crosshall] pending: 1", "body": "synthetic private fixture note",
}

type codexInstall struct {
	Revision string `json:"revision"`
	Install  struct {
		HomeEnv  string            `json:"home_env"`
		File     string            `json:"file"`
		Encoding string            `json:"encoding"`
		Events   []string          `json:"events"`
		Matchers map[string]string `json:"matchers"`
		Timeout  int               `json:"timeout_s"`
	} `json:"install"`
}

type codexHook struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
}

type codexGroup struct {
	Matcher string      `json:"matcher,omitempty"`
	Hooks   []codexHook `json:"hooks"`
}

func codexQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'" }

func codexByteMeter(command, file string) string {
	return "{ " + command + " | /usr/bin/tee /dev/fd/3 | /usr/bin/wc -c >> " + codexQuote(file) + "; } 3>&1"
}

func TestCodexMeterForwardsHookBytes(t *testing.T) {
	count := filepath.Join(t.TempDir(), "callback count")
	input := []byte("line one\nline two\x00\n")
	cmd := exec.Command("/bin/sh", "-c", codexByteMeter("/bin/cat", count))
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	cmd.Stdin = bytes.NewReader(input)
	out, err := cmd.Output()
	codexMust(t, err)
	n, err := os.ReadFile(count)
	codexMust(t, err)
	if !bytes.Equal(out, input) || strings.TrimSpace(string(n)) != fmt.Sprint(len(input)) {
		t.Fatal("meter changed the hook bytes or recorded an incorrect count")
	}
}

func codexMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func codexPut(t *testing.T, file string, data []byte) {
	t.Helper()
	codexMust(t, os.WriteFile(file, data, 0600))
}

// temp-owned hooks.json, matching the installed event groups. On resume the
// original stdout remains fd 3. tee forwards it to Codex while wc records a
// count for each callback, including callbacks which produced zero bytes.
func writeCodexHooks(t *testing.T, root, exe string, m codexInstall, metered bool) {
	t.Helper()
	i := m.Install
	if i.HomeEnv != "CODEX_HOME" || i.Encoding != "shell" || i.File != "hooks.json" || i.Timeout != 1 || m.Revision == "" {
		t.Fatal("unexpected Codex install description")
	}
	hooks := map[string][]codexGroup{}
	eventName := regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*$`)
	for _, event := range i.Events {
		if !eventName.MatchString(event) || hooks[event] != nil {
			t.Fatalf("invalid event %q", event)
		}
		command := codexQuote(exe) + " hook codex " + event
		if metered {
			file := filepath.Join(root, "bytes-"+event)
			command = codexByteMeter(command, file)
		}
		hooks[event] = []codexGroup{{Matcher: i.Matchers[event], Hooks: []codexHook{{"command", command, i.Timeout}}}}
	}
	// A separate silent handler waits for the peer to store its note between
	// SessionStart and UserPromptSubmit. The shipped handler has already run.
	barrier := "for n in 1 2 3 4 5 6 7 8 9 10; do test -f " + codexQuote(filepath.Join(root, "note-stored")) + " && exit 0; sleep 0.1; done; exit 1"
	hooks["SessionStart"] = append(hooks["SessionStart"], codexGroup{Hooks: []codexHook{{"command", barrier, 2}}})
	data, err := json.Marshal(map[string]any{"hooks": hooks})
	codexMust(t, err)
	codexPut(t, filepath.Join(root, "codex", i.File), data)
}

func codexCLI(exe string, env []string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, append([]string{"--json"}, args...)...)
	cmd.Env = env
	out, err := cmd.Output()
	var result struct {
		OK      bool
		Code    string
		Message string
	}
	if parseErr := json.Unmarshal(out, &result); parseErr != nil {
		return fmt.Errorf("crosshall %s: %w (process: %v)", args[0], parseErr, err)
	}
	if err != nil || !result.OK {
		return fmt.Errorf("crosshall %s: %s %s (%v)", args[0], result.Code, result.Message, err)
	}
	return nil
}

func startCodexDispatcher(t *testing.T, exe, state string, env []string) func() {
	t.Helper()
	codexMust(t, os.MkdirAll(state, 0700))
	rv := ident.Rendezvous(state, os.Geteuid())
	if _, err := os.Lstat(rv); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fixture rendezvous already occupied: %v", err)
	}
	cmd := exec.Command(exe, "__dispatch")
	cmd.Env, cmd.Dir = env, state
	codexMust(t, cmd.Start())
	done := make(chan struct{})
	var waitErr error
	go func() { waitErr = cmd.Wait(); close(done) }()
	var owned os.FileInfo
	stop := sync.OnceFunc(func() {
		select {
		case <-done:
		default:
			_ = cmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				_ = cmd.Process.Kill()
				<-done
			}
		}
		if waitErr != nil {
			t.Errorf("fixture dispatcher: %v", waitErr)
		}
		if owned != nil {
			current, err := os.Lstat(rv)
			if err != nil || !os.SameFile(owned, current) {
				t.Errorf("fixture rendezvous changed: %v", err)
				return
			}
			if err := os.RemoveAll(rv); err != nil {
				t.Error(err)
			}
		}
	})
	t.Cleanup(stop)
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); {
		var ready struct{ PID int }
		b, err := os.ReadFile(filepath.Join(rv, "ready"))
		if err == nil && json.Unmarshal(b, &ready) == nil {
			if ready.PID != cmd.Process.Pid {
				t.Fatal("unexpected fixture dispatcher")
			}
			owned, err = os.Lstat(rv)
			codexMust(t, err)
			return stop
		}
		select {
		case <-done:
			t.Fatalf("dispatcher exited before ready: %v", waitErr)
		case <-time.After(10 * time.Millisecond):
		}
	}
	t.Fatal("fixture dispatcher did not start")
	return nil
}

func openCodexLedger(t *testing.T, state string) *sql.DB {
	t.Helper()
	u := url.URL{Scheme: "file", Path: filepath.Join(state, "store.sqlite3"), RawQuery: "mode=ro"}
	db, err := sql.Open("sqlite", u.String())
	codexMust(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func seedCodexNote(ctx context.Context, db *sql.DB, exe, ready string, env []string) error {
	for {
		var alias string
		err := db.QueryRowContext(ctx, "SELECT b.alias FROM binding b JOIN principal p ON p.id=b.principal_id WHERE b.state='live' AND p.vendor='codex'").Scan(&alias)
		if err == nil {
			if err := codexCLI(exe, env, "note", "@"+alias, codexMarkers["body"]); err != nil {
				return err
			}
			return os.WriteFile(ready, nil, 0600)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func codexTurn(t *testing.T, prefix, env []string, dir string, model *codexFixture, args ...string) map[string]map[string]int {
	t.Helper()
	before := len(model.Requests())
	base := append(slices.Clone(prefix[1:]), "-a", "never", "exec", "--dangerously-bypass-hook-trust", "--skip-git-repo-check", "--sandbox", "workspace-write")
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, prefix[0], append(base, args...)...)
	cmd.Dir, cmd.Env = dir, env
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("codex exec failed: %v (timeout: %v)", err, ctx.Err())
	}
	requests := model.Requests()
	if strings.TrimSpace(string(out)) != "OK" || len(requests) != before+1 {
		t.Fatalf("expected OK and one model request, got %d request(s)", len(requests)-before)
	}
	return requests[before]
}

func codexLedger(t *testing.T, db *sql.DB) {
	t.Helper()
	count := func(want int, query string) {
		t.Helper()
		var got int
		codexMust(t, db.QueryRow(query).Scan(&got))
		if got != want {
			t.Fatalf("%s: got %d, want %d", query, got, want)
		}
	}
	count(1, "SELECT count(*) FROM offer JOIN offer_item ON offer.id=offer_item.offer_id")
	count(1, "SELECT count(*) FROM offer JOIN offer_item ON offer.id=offer_item.offer_id WHERE reason='prompt' AND message_id=1")
	count(1, "SELECT count(*) FROM recipient WHERE message_id=1")
	count(1, "SELECT count(*) FROM recipient WHERE message_id=1 AND picked_up_at_ms IS NULL")
	count(3, "SELECT count(DISTINCT kind) FROM observation WHERE kind IN ('prompt_submitted','turn_finishing','session_ended')")
	count(1, "SELECT count(*) FROM binding b JOIN principal p ON p.id=b.principal_id WHERE p.vendor='codex'")
	count(1, "SELECT count(*) FROM binding b JOIN principal p ON p.id=b.principal_id WHERE p.vendor='codex' AND b.state='live' AND b.departed_by_hook=0")
}

type codexResult struct {
	Cap   string `json:"cap"`
	Level string `json:"level"`
	Check string `json:"check"`
}

// Only the measured vendor build, platform, mode and adapter revision have a
// negative result. A different tuple needs its own conformance measurements.
func codexReminderEvidence(version, platform, mode, revision string) codexResult {
	if version == "0.170.0" && platform == "darwin-arm64" && mode == "exec" && revision == "codex-1" {
		return codexResult{"reminder", "unavailable", "The hook could not verify its launch version or mode. This run did not measure whether the read process receives the native tool-call ID."}
	}
	return codexResult{"reminder", "candidate", "No reminder verdict was established for this build, platform, mode and manifest revision."}
}

func TestCodexReminderEvidenceScope(t *testing.T) {
	for _, tc := range []struct{ version, platform, mode, revision, level string }{
		{"0.170.0", "darwin-arm64", "exec", "codex-1", "unavailable"},
		{"0.169.0-alpha.11.1", "darwin-arm64", "exec", "codex-1", "candidate"},
		{"0.170.0", "linux-arm64", "exec", "codex-1", "candidate"},
		{"0.170.0", "darwin-amd64", "exec", "codex-1", "candidate"},
		{"0.170.0", "darwin-arm64", "interactive", "codex-1", "candidate"},
		{"0.170.0", "darwin-arm64", "exec", "codex-2", "candidate"},
	} {
		got := codexReminderEvidence(tc.version, tc.platform, tc.mode, tc.revision)
		if got.Cap != "reminder" || got.Level != tc.level || got.Check == "" {
			t.Errorf("%v: got %+v", tc, got)
		}
	}
}

type codexEvidence struct {
	Vendor   string        `json:"vendor"`
	Version  string        `json:"vendor_version"`
	Platform string        `json:"platform"`
	Mode     string        `json:"mode"`
	Revision string        `json:"manifest_rev"`
	Time     int64         `json:"run_at_ms"`
	Results  []codexResult `json:"results"`
}

func codexOutsideVendorHomes(t *testing.T, path string) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(path)
	codexMust(t, err)
	real, err = filepath.Abs(real)
	codexMust(t, err)
	account, err := user.Current()
	codexMust(t, err)
	for _, n := range []string{".codex", ".claude"} {
		home := filepath.Join(account.HomeDir, n)
		if target, err := filepath.EvalSymlinks(home); err == nil {
			home = target
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if real == home || strings.HasPrefix(real, home+string(os.PathSeparator)) {
			t.Fatal("select a path outside the real vendor homes")
		}
	}
	return real
}

// codexEvidenceDir checks a directory the evidence file is written to: the
// Claude Code run's rule, an absolute, existing, unlinked directory outside
// the login home and outside the vendor homes wherever their links lead.
func codexEvidenceDir(dir string) (string, error) {
	if err := safeOutputDir(dir); err != nil {
		return "", err
	}
	return filepath.Clean(dir), nil
}

func codexBinary(t *testing.T, root string, buildFlags ...string) string {
	t.Helper()
	dir := filepath.Join(root, "owner's fixture bin")
	codexMust(t, os.Mkdir(dir, 0700))
	exe := filepath.Join(dir, "crosshall")
	if override := os.Getenv("CROSSHALL_CONFORMANCE_CROSSHALL"); override != "" && len(buildFlags) == 0 {
		source, err := os.Open(codexOutsideVendorHomes(t, override))
		codexMust(t, err)
		defer source.Close()
		dest, err := os.OpenFile(exe, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0700)
		codexMust(t, err)
		_, err = io.Copy(dest, source)
		closeErr := dest.Close()
		codexMust(t, err)
		codexMust(t, closeErr)
		return exe
	}
	// The build reuses Go's local module/build caches. Its toolchain and the
	// vendor process both run without account credentials or network fetches.
	account, err := user.Current()
	codexMust(t, err)
	goEnv := []string{"PATH=" + os.Getenv("PATH"), "GOENV=off", "GOPROXY=off", "GOSUMDB=off", "GOTOOLCHAIN=local", "GOTELEMETRY=off"}
	cacheCmd := exec.Command("go", "env", "-json", "GOMODCACHE", "GOCACHE")
	cacheCmd.Env = append(slices.Clone(goEnv), "HOME="+account.HomeDir)
	for _, key := range []string{"GOMODCACHE", "GOCACHE", "GOPATH"} {
		if value := os.Getenv(key); value != "" {
			cacheCmd.Env = append(cacheCmd.Env, key+"="+value)
		}
	}
	raw, err := cacheCmd.Output()
	codexMust(t, err)
	var caches struct{ GOMODCACHE, GOCACHE string }
	codexMust(t, json.Unmarshal(raw, &caches))
	args := append([]string{"build", "-o", exe}, buildFlags...)
	cmd := exec.Command("go", append(args, "./cmd/crosshall")...)
	cmd.Dir = "../.."
	cmd.Env = append(goEnv, "HOME="+filepath.Join(root, "home"), "TMPDIR="+filepath.Join(root, "tmp"),
		"GOMODCACHE="+codexOutsideVendorHomes(t, caches.GOMODCACHE), "GOCACHE="+codexOutsideVendorHomes(t, caches.GOCACHE))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build crosshall: %v\n%s", err, out)
	}
	return exe
}

func codexEvidenceFile(t *testing.T, dir string, e codexEvidence) (string, []byte) {
	t.Helper()
	dir, err := codexEvidenceDir(dir)
	codexMust(t, err)
	file := filepath.Join(dir, fmt.Sprintf("codex-%s-%s-exec.json", e.Version, e.Platform))
	raw, err := json.MarshalIndent(e, "", "  ")
	codexMust(t, err)
	raw = append(raw, '\n')
	f, err := os.OpenFile(file, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	codexMust(t, err)
	_, err = f.Write(raw)
	closeErr := f.Close()
	codexMust(t, err)
	codexMust(t, closeErr)
	return file, raw
}

func codexVerifyImport(t *testing.T, db *sql.DB, e codexEvidence, raw []byte) {
	t.Helper()
	rows, err := db.Query("SELECT cap,level,result_ref,checked_at_ms FROM capability_evidence WHERE vendor=? AND vendor_version=? AND platform=? AND mode=? AND manifest_rev=?", e.Vendor, e.Version, e.Platform, e.Mode, e.Revision)
	codexMust(t, err)
	defer rows.Close()
	want := map[string]string{}
	for _, r := range e.Results {
		want[r.Cap] = r.Level
	}
	hash := fmt.Sprintf("sha256:%x", sha256.Sum256(raw))
	for rows.Next() {
		var cap, level, ref string
		var when int64
		codexMust(t, rows.Scan(&cap, &level, &ref, &when))
		if want[cap] != level || ref != hash || when != e.Time {
			t.Fatalf("unexpected imported %q row (level=%s, ref=%s, time=%d)", cap, level, ref, when)
		}
		delete(want, cap)
	}
	codexMust(t, rows.Err())
	if len(want) != 0 {
		t.Fatalf("missing evidence rows: %v", want)
	}
}

func TestCodexExecConformance(t *testing.T) {
	codex, err := exec.LookPath("codex")
	if errors.Is(err, exec.ErrNotFound) {
		t.Skip("no codex binary on PATH")
	}
	codexMust(t, err)
	codex, err = filepath.Abs(codex)
	codexMust(t, err)
	temp, err := os.MkdirTemp("/tmp", "crosshall-codex-")
	codexMust(t, err)
	t.Cleanup(func() {
		if err := os.RemoveAll(temp); err != nil {
			t.Error(err)
		}
	})
	root, err := filepath.EvalSymlinks(temp)
	codexMust(t, err)
	env := codexEnv(t, root)
	exe := codexBinary(t, root)
	state := filepath.Join(root, "state")
	repo, other := filepath.Join(root, "repo"), filepath.Join(root, "unrelated")
	for _, dir := range []string{repo, other} {
		codexMust(t, os.Mkdir(dir, 0700))
	}
	git := exec.Command("git", "init", "-q", repo)
	git.Env = env
	codexMust(t, git.Run())
	var manifest codexInstall
	raw, err := os.ReadFile("../../internal/hook/manifests/codex.json")
	codexMust(t, err)
	codexMust(t, json.Unmarshal(raw, &manifest))
	writeCodexHooks(t, root, exe, manifest, false)
	model := startCodexFixture(t, codexMarkers)
	endpoint, err := url.Parse(model.URL)
	codexMust(t, err)
	prefix := codexCommand(t, codex, env, endpoint.Port())
	v := exec.Command(prefix[0], append(slices.Clone(prefix[1:]), "--version")...)
	v.Dir, v.Env = repo, env
	out, err := v.Output()
	codexMust(t, err)
	match := regexp.MustCompile(`^codex-cli ([A-Za-z0-9.+-]+)$`).FindStringSubmatch(strings.TrimSpace(string(out)))
	if match == nil {
		t.Fatal("Codex did not report an exact CLI version")
	}
	config := fmt.Sprintf("model_provider=\"fixture\"\nmodel=\"fixture\"\n[model_providers.fixture]\nname=\"Fixture\"\nwire_api=\"responses\"\nbase_url=%q\n", model.URL)
	codexPut(t, filepath.Join(root, "codex", "config.toml"), []byte(config))
	stop := startCodexDispatcher(t, exe, state, env)
	codexMust(t, codexCLI(exe, env, "owner", "channel", "create", "fixture"))
	codexMust(t, codexCLI(exe, env, "owner", "channel", "default", "fixture", "--repo", repo))
	codexMust(t, codexCLI(exe, env, "owner", "adapter", "set", "codex", "--enabled", "on"))
	var route [16]byte
	rand.Read(route[:])
	peer := append(slices.Clone(env), "CROSSHALL_ROUTE="+hex.EncodeToString(route[:]))
	codexMust(t, codexCLI(exe, peer, "join", "fixture", "--as", "gate-peer"))
	db := openCodexLedger(t, state)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	seeded := make(chan struct{})
	var seedErr error
	go func() {
		defer close(seeded)
		seedErr = seedCodexNote(ctx, db, exe, filepath.Join(root, "note-stored"), peer)
	}()
	defer func() { cancel(); <-seeded }()
	first := codexTurn(t, prefix, env, repo, model, "Say OK.")
	select {
	case <-seeded:
		codexMust(t, seedErr)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	want := map[string]map[string]int{"developer": {"hint": 1, "roster": 1, "pending": 1}}
	if !reflect.DeepEqual(first, want) {
		t.Fatalf("enrollment and first prompt: got %v, want %v", first, want)
	}
	writeCodexHooks(t, root, exe, manifest, true)
	codexTurn(t, prefix, env, repo, model, "resume", "--last", "Say OK again.")
	for _, event := range manifest.Install.Events {
		data, err := os.ReadFile(filepath.Join(root, "bytes-"+event))
		if event == "PermissionRequest" && errors.Is(err, os.ErrNotExist) {
			continue
		}
		codexMust(t, err)
		if event == "PermissionRequest" || !slices.Equal(strings.Fields(string(data)), []string{"0"}) {
			t.Fatalf("quiet turn %s callback counts: %q", event, data)
		}
	}
	platform := runtime.GOOS + "-" + runtime.GOARCH
	reminder := codexReminderEvidence(match[1], platform, "exec", manifest.Revision)
	if reminder.Level == "unavailable" {
		var unverified bool
		err := db.QueryRow(`SELECT COUNT(*) > 0 AND COUNT(*) = SUM(
			json_extract(profile, '$.vendor_version') = 'unverified' AND json_extract(profile, '$.mode') = 'unverified')
			FROM adapter_launch WHERE json_extract(profile, '$.adapter') = 'codex'`).Scan(&unverified)
		codexMust(t, err)
		if !unverified {
			t.Fatal("Codex supplied new launch provenance; re-evaluate reminder availability")
		}
	}
	codexLedger(t, db)
	codexMust(t, db.Close())
	codexMust(t, codexCLI(exe, env, "owner", "stop"))
	stop()
	if markers := codexTurn(t, prefix, env, other, model, "Say OK."); len(markers) != 0 {
		t.Fatalf("owner-stopped turn added context: %v", markers)
	}
	rendezvous := ident.Rendezvous(state, os.Geteuid())
	if _, err := os.Lstat(rendezvous); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rendezvous %q did not stay absent after owner stop: %v", rendezvous, err)
	}
	e := codexEvidence{"codex", match[1], platform, "exec", manifest.Revision, time.Now().UnixMilli(), []codexResult{
		{"enroll", "tested", "SessionStart enrolled once and supplied the hint and roster with developer role. Resume preserved membership."},
		{"boundary", "tested", "One prompt offered one unread ID with developer role, without its body or pickup. Four resumed callbacks emitted zero bytes. Quiet and owner-stopped turns each made one model request."},
		{"observe", "candidate", "Prompt, Stop and SessionEnd observations recorded. Approval was not exercised. The session-scoped end preserved membership."},
		reminder,
	}}
	file, contents := codexEvidenceFile(t, root, e)
	switch os.Getenv("CROSSHALL_CONFORMANCE_OUTPUT_ONLY") {
	case "":
		stopImport := startCodexDispatcher(t, exe, state, env)
		codexMust(t, codexCLI(exe, env, "owner", "adapter", "evidence", file))
		codexVerifyImport(t, openCodexLedger(t, state), e, contents)
		stopImport()
	case "1":
		t.Log("output-only requested; import skipped")
	default:
		t.Fatal("CROSSHALL_CONFORMANCE_OUTPUT_ONLY expects 1 or an unset variable")
	}
	if dir := os.Getenv("CROSSHALL_CONFORMANCE_OUT"); dir != "" {
		file, _ = codexEvidenceFile(t, dir, e)
	}
	t.Logf("evidence: %s\n%s", file, bytes.TrimSpace(contents))
}
