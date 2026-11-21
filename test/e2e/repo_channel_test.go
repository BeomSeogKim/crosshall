//go:build e2e

package e2e

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitRepository lays out the least Git itself accepts as a repository.
func gitRepository(t *testing.T, dir string) string {
	t.Helper()
	for _, name := range []string{"objects", "refs"} {
		if err := os.MkdirAll(filepath.Join(dir, ".git", name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// linkedWorktree lays out what `git worktree add` leaves: a .git file in the
// worktree and a directory under the repository's .git/worktrees.
func linkedWorktree(t *testing.T, repo, dir string) string {
	t.Helper()
	admin := filepath.Join(repo, ".git", "worktrees", filepath.Base(dir))
	for _, d := range []string{admin, dir} {
		if err := os.MkdirAll(d, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for path, text := range map[string]string{
		filepath.Join(admin, "HEAD"):      "ref: refs/heads/side\n",
		filepath.Join(admin, "commondir"): "../..\n",
		filepath.Join(dir, ".git"):        "gitdir: " + admin + "\n",
	} {
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// With the owner's switch on, a session that starts in a Git repository
// with no channel gets the repository's own channel, and a session in a
// linked worktree of that repository joins the same one.
func TestAutomaticJoiningGivesARepositoryItsChannel(t *testing.T) {
	f := newSetupWorld(t)
	f.apply("", "setup", "claude", "codex", "--apply")
	owner := f.owner()
	if got := owner.must("owner", "channel", "auto", "on"); !strings.Contains(got.stdout, `"on":true`) {
		t.Fatalf("switch on: %s", got.stdout)
	}
	repo := gitRepository(t, filepath.Join(f.home, "Shop API"))
	worktree := linkedWorktree(t, repo, filepath.Join(f.home, "shop-side"))

	if got := f.invokeHook(f.home, "claude", "SessionStart", `"source":"startup"`); got != "" {
		t.Fatalf("a start outside Git printed %q", got)
	}
	if got := startAs(f, "claude", "one", repo); !strings.Contains(got, "Joined #shop-api as @claude1") {
		t.Fatalf("first start in the repository printed %q", got)
	}
	if got := startAs(f, "codex", "two", worktree); !strings.Contains(got, "Joined #shop-api as @codex1") {
		t.Fatalf("start in a linked worktree printed %q", got)
	}
	var channels, defaults int
	var root string
	if err := f.db().QueryRow(`SELECT (SELECT COUNT(*) FROM channel), (SELECT COUNT(*) FROM repo_default),
		(SELECT repo_root FROM repo_default)`).Scan(&channels, &defaults, &root); err != nil || channels != 1 || defaults != 1 ||
		filepath.Base(root) != "Shop API" {
		t.Fatalf("channels %d, defaults %d, root %q (%v)", channels, defaults, root, err)
	}
}

// The owner's explicit default for a worktree wins over the repository's
// channel, a second repository of the same name gets a suffix, and with the
// switch off again nothing is created while a bound repository keeps its channel.
func TestAutomaticJoiningKeepsOwnerChoices(t *testing.T) {
	f := newSetupWorld(t)
	f.apply("", "setup", "claude", "codex", "--apply")
	owner := f.owner()
	owner.must("owner", "channel", "auto", "on")
	owner.must("owner", "channel", "create", "api")
	repo := gitRepository(t, filepath.Join(f.home, "one", "api"))
	worktree := linkedWorktree(t, repo, filepath.Join(f.home, "review"))
	owner.must("owner", "channel", "create", "review-team")
	owner.must("owner", "channel", "default", "review-team", "--repo", worktree)

	if got := startAs(f, "claude", "one", repo); !strings.Contains(got, "Joined #api-2 as @claude1") {
		t.Fatalf("a repository whose name is taken printed %q", got)
	}
	if got := startAs(f, "codex", "two", worktree); !strings.Contains(got, "Joined #review-team as @codex1") {
		t.Fatalf("a worktree with its own default printed %q", got)
	}
	owner.must("owner", "channel", "auto", "off")
	fresh := gitRepository(t, filepath.Join(f.home, "fresh"))
	if got := f.invokeHook(fresh, "codex", "SessionStart", `"source":"startup"`); strings.Contains(got, "Joined #") {
		t.Fatalf("with the switch off a start in an unbound repository printed %q", got)
	}
	if got := startAs(f, "codex", "third", repo); !strings.Contains(got, "Joined #api-2 as @codex") {
		t.Fatalf("with the switch off a start in a bound repository printed %q", got)
	}
	var channels int
	if err := f.db().QueryRow(`SELECT COUNT(*) FROM channel`).Scan(&channels); err != nil || channels != 3 {
		t.Fatalf("channels %d (%v)", channels, err)
	}
}

// A repository made with git init --separate-git-dir has one identity for
// its main checkout and its linked worktrees: they share the automatic
// channel, and the owner's default set in the main checkout, also one that
// an older version kept under the main checkout's path.
func TestAutomaticJoiningSeesOneRepositoryBehindASeparateGitDirectory(t *testing.T) {
	for _, explicit := range []string{"", "default", "older default", "older default, unreadable once"} {
		f := newSetupWorld(t)
		f.apply("", "setup", "claude", "codex", "--apply")
		owner := f.owner()
		owner.must("owner", "channel", "auto", "on")
		metadata := filepath.Join(f.home, "metadata")
		if err := os.MkdirAll(metadata, 0700); err != nil {
			t.Fatal(err)
		}
		main := filepath.Join(f.home, "main")
		for path, text := range map[string]string{
			filepath.Join(metadata, "HEAD"):            "ref: refs/heads/main\n",
			filepath.Join(metadata, "objects", "info"): "",
			filepath.Join(metadata, "refs", "info"):    "",
			filepath.Join(main, ".git"):                "gitdir: " + metadata + "\n",
		} {
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(text), 0600); err != nil {
				t.Fatal(err)
			}
		}
		admin := filepath.Join(metadata, "worktrees", "linked")
		linked := filepath.Join(f.home, "linked")
		for path, text := range map[string]string{
			filepath.Join(admin, "HEAD"):      "ref: refs/heads/side\n",
			filepath.Join(admin, "commondir"): "../..\n",
			filepath.Join(linked, ".git"):     "gitdir: " + admin + "\n",
		} {
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(text), 0600); err != nil {
				t.Fatal(err)
			}
		}
		want := "metadata"
		if explicit != "" {
			want = "chosen"
			owner.must("owner", "channel", "create", "chosen")
			owner.must("owner", "channel", "default", "chosen", "--repo", main)
		}
		if strings.HasPrefix(explicit, "older default") {
			owner.must("owner", "stop")
			canonical, err := filepath.EvalSymlinks(main)
			if err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite", "file:"+filepath.Join(f.state, "store.sqlite3"))
			if err != nil {
				t.Fatal(err)
			}
			res, err := db.Exec(`UPDATE repo_default SET repo_root = ?`, canonical)
			if rows, _ := res.RowsAffected(); err != nil || rows != 1 {
				t.Fatalf("rewrite the default as an older version kept it: %d rows (%v)", rows, err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			if explicit == "older default, unreadable once" {
				// The upgraded dispatcher cannot read the main checkout at its
				// first start, so the repository gets a channel of its own
				// before the owner's default is found.
				if err := os.Chmod(filepath.Join(main, ".git"), 0); err != nil {
					t.Fatal(err)
				}
				owner.must("owner", "channel", "auto", "on")
				startAs(f, "codex", "early", linked)
				if got := joinedChannel(f, "codex", "early"); got != "metadata" {
					t.Fatalf("%q: before the default was adopted the linked worktree joined %q", explicit, got)
				}
				if err := os.Chmod(filepath.Join(main, ".git"), 0600); err != nil {
					t.Fatal(err)
				}
				owner.must("owner", "stop")
			}
			owner.must("owner", "channel", "auto", "on") // the upgraded dispatcher starts
		}
		if explicit == "older default, unreadable once" {
			// The owner's default replaced the automatic one at the next start.
			var channel string
			var rows, automatic int
			if err := f.db().QueryRow(`SELECT COUNT(*), MAX(c.name), MAX(d.automatic) FROM repo_default d
				JOIN channel c ON c.id = d.channel_id`).Scan(&rows, &channel, &automatic); err != nil || rows != 1 || channel != "chosen" || automatic != 0 {
				t.Fatalf("%q: %d defaults, channel %q, automatic %d (%v)", explicit, rows, channel, automatic, err)
			}
			for session, dir := range map[string]string{"two": linked, "one": main} {
				startAs(f, "codex", session, dir)
				if got := joinedChannel(f, "codex", session); got != "chosen" {
					t.Fatalf("%q: a start in %s joined %q", explicit, dir, got)
				}
			}
			continue
		}
		if got := startAs(f, "codex", "two", linked); !strings.Contains(got, "Joined #"+want+" as @codex1") {
			t.Fatalf("%q: the linked worktree printed %q", explicit, got)
		}
		if got := startAs(f, "claude", "one", main); !strings.Contains(got, "Joined #"+want+" as @claude1") {
			t.Fatalf("%q: the main checkout printed %q", explicit, got)
		}
		var channels int
		if err := f.db().QueryRow(`SELECT COUNT(*) FROM channel`).Scan(&channels); err != nil || channels != 1 {
			t.Fatalf("%q: channels %d (%v)", explicit, channels, err)
		}
	}
}

// An Antigravity conversation keeps its first workspace as well: a later
// start of the same conversation from another place joins nothing there.
func TestAutomaticJoiningKeepsAnAntigravityConversationWhereItStarted(t *testing.T) {
	f := newSetupWorld(t)
	owner := f.owner()
	owner.must("owner", "adapter", "set", "agy", "--enabled", "on")
	first := gitRepository(t, filepath.Join(f.home, "first"))
	second := gitRepository(t, filepath.Join(f.home, "second"))
	start := func(conversation, workspace string) { startAntigravity(f, conversation, workspace) }
	state := func() (channels, members int) {
		t.Helper()
		if err := f.db().QueryRow(`SELECT (SELECT COUNT(*) FROM channel), (SELECT COUNT(*) FROM binding)`).Scan(&channels, &members); err != nil {
			t.Fatal(err)
		}
		return
	}
	const outside, switched = "0a6a3c1e-0000-4000-8000-000000000001", "0a6a3c1e-0000-4000-8000-000000000002"
	start(switched, first) // the switch is still off: nothing joins, the place is recorded
	owner.must("owner", "channel", "auto", "on")
	start(outside, f.home)
	start(outside, second)
	start(switched, second)
	if channels, members := state(); channels != 0 || members != 0 {
		t.Fatalf("a later start from another place made %d channels and %d members", channels, members)
	}
	start(switched, first)
	if channels, members := state(); channels != 1 || members != 1 {
		t.Fatalf("a later start in the first workspace made %d channels and %d members", channels, members)
	}
}

// Only a real work tree gets a channel of its own: not a directory whose
// .git holds a HEAD and nothing else, which Git rejects. And a session
// inside Git's own directories, wherever they live, is in no repository: it
// creates nothing, joins nothing, and is not recorded as started there.
func TestAutomaticJoiningNeedsARealWorkTree(t *testing.T) {
	f := newSetupWorld(t)
	f.apply("", "setup", "claude", "--apply")
	owner := f.owner()
	owner.must("owner", "adapter", "set", "agy", "--enabled", "on")
	owner.must("owner", "channel", "auto", "on")
	hollow := filepath.Join(f.home, "hollow")
	if err := os.MkdirAll(filepath.Join(hollow, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hollow, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0600); err != nil {
		t.Fatal(err)
	}
	repo := gitRepository(t, filepath.Join(f.home, "real"))
	objects := filepath.Join(repo, ".git", "objects")
	// git init --separate-git-dir=<checkout>/metadata
	apart := filepath.Join(f.home, "apart")
	for path, text := range map[string]string{
		filepath.Join(apart, "metadata", "HEAD"):            "ref: refs/heads/main\n",
		filepath.Join(apart, "metadata", "objects", "info"): "",
		filepath.Join(apart, "metadata", "refs", "info"):    "",
		filepath.Join(apart, ".git"):                        "gitdir: " + filepath.Join(apart, "metadata") + "\n",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	state := func() (channels, members int) {
		t.Helper()
		if err := f.db().QueryRow(`SELECT (SELECT COUNT(*) FROM channel), (SELECT COUNT(*) FROM binding)`).Scan(&channels, &members); err != nil {
			t.Fatal(err)
		}
		return
	}
	prompt := func(session, dir, turn string) string {
		return fireHook(f, "claude", session, dir, "UserPromptSubmit", fmt.Sprintf(`"prompt_id":%q,"turn_id":%q,"prompt":"synthetic"`, turn, turn))
	}
	const first, late = "0a6a3c1e-0000-4000-8000-00000000000a", "0a6a3c1e-0000-4000-8000-00000000000b"
	for session, dir := range map[string]string{
		"hollow": hollow, "inside": objects, "apart": filepath.Join(apart, "metadata", "objects"),
	} {
		if got := startAs(f, "claude", session, dir); got != "" {
			t.Errorf("a start in %s printed %q", dir, got)
		}
	}
	startAntigravity(f, first, objects)
	// The same conversations, now in the work tree: they did not start there.
	if got := prompt("inside", repo, "second"); got != "" {
		t.Errorf("a prompt in the work tree after a start inside .git printed %q", got)
	}
	startAntigravity(f, first, repo)
	if channels, members := state(); channels != 0 || members != 0 {
		t.Fatalf("starts outside a work tree made %d channels and %d members", channels, members)
	}
	if got := startAs(f, "claude", "work", repo); !strings.Contains(got, "Joined #real as @claude1") {
		t.Fatalf("the work tree printed %q", got)
	}
	// The repository has its channel now; inside .git is still no place to join it.
	if got := startAs(f, "claude", "late", objects); got != "" {
		t.Errorf("a start inside .git of a repository with a channel printed %q", got)
	}
	startAntigravity(f, late, objects)
	// Nor is a Git directory nested in the checkout, whoever it belongs to: a
	// bare backup, or the separate Git directory of another checkout.
	const nested = "0a6a3c1e-0000-4000-8000-00000000000c"
	for name, dir := range map[string]string{"backup": filepath.Join(repo, "backup.git"), "foreign": filepath.Join(repo, "foreign.git")} {
		for path, text := range map[string]string{
			filepath.Join(dir, "HEAD"):            "ref: refs/heads/main\n",
			filepath.Join(dir, "objects", "info"): "",
			filepath.Join(dir, "refs", "info"):    "",
		} {
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(text), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if got := startAs(f, "claude", name, filepath.Join(dir, "objects")); got != "" {
			t.Errorf("a start inside %s printed %q", dir, got)
		}
		startAntigravity(f, nested+name[:1], filepath.Join(dir, "objects"))
	}
	elsewhere := filepath.Join(f.home, "elsewhere")
	if err := os.MkdirAll(elsewhere, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(elsewhere, ".git"), []byte("gitdir: "+filepath.Join(repo, "foreign.git")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if channels, members := state(); channels != 1 || members != 1 {
		t.Fatalf("starts inside Git directories left %d channels and %d members", channels, members)
	}
}

func startAntigravity(f *setupWorld, conversation, workspace string) {
	f.t.Helper()
	payload, err := json.Marshal(map[string]any{
		"conversationId": conversation,
		"workspacePaths": []string{workspace},
		"transcriptPath": filepath.Join(f.home, ".gemini", "antigravity-cli", "brain", conversation,
			".system_generated", "logs", "transcript_full.jsonl"),
	})
	if err != nil {
		f.t.Fatal(err)
	}
	cmd := exec.CommandContext(f.t.Context(), f.executable, "hook", "agy", "SessionStart")
	cmd.Dir, cmd.Env, cmd.Stdin = workspace, f.env, strings.NewReader(string(payload))
	if out, err := cmd.CombinedOutput(); err != nil {
		f.t.Fatalf("Antigravity start: %v, output %q", err, out)
	}
}

// A start recorded by a version that named the worktree, not the
// repository, still lets its session join there after the upgrade.
func TestAWorktreeStartRecordFromAnOlderVersionStillEnrols(t *testing.T) {
	f := newSetupWorld(t)
	f.apply("", "setup", "claude", "--apply")
	owner := f.owner()
	repo := gitRepository(t, filepath.Join(f.home, "shop"))
	linked := linkedWorktree(t, repo, filepath.Join(f.home, "shop-side"))
	fireHook(f, "claude", "older", linked, "SessionStart", `"source":"startup"`)
	records, err := filepath.Glob(filepath.Join(f.state, "hook-start-repos", "*"))
	if err != nil || len(records) != 1 {
		t.Fatalf("start records %v (%v)", records, err)
	}
	canonical, err := filepath.EvalSymlinks(linked)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(records[0], fmt.Appendf(nil, "%x\n", sha256.Sum256([]byte(canonical))), 0600); err != nil {
		t.Fatal(err)
	}
	owner.must("owner", "channel", "create", "side")
	owner.must("owner", "channel", "default", "side", "--repo", linked)
	joined := func() (name string) {
		t.Helper()
		err := f.db().QueryRow(`SELECT c.name FROM binding b JOIN channel c ON c.id = b.channel_id WHERE b.state = 'live'`).Scan(&name)
		if err != nil && !strings.Contains(err.Error(), "no rows") {
			t.Fatal(err)
		}
		return name
	}
	// A prompt that passes its deadline joins nothing; the next one tries again.
	for turn := 0; turn < 3 && joined() == ""; turn++ {
		fireHook(f, "claude", "older", linked, "UserPromptSubmit",
			fmt.Sprintf(`"prompt_id":"p%d","turn_id":"p%d","prompt":"synthetic"`, turn, turn))
	}
	if got := joined(); got != "side" {
		t.Fatalf("the session joined %q", got)
	}
}

// joinedChannel names the channel the session is a live member of.
func joinedChannel(f *setupWorld, adapter, session string) string {
	f.t.Helper()
	var name string
	err := f.db().QueryRow(`SELECT c.name FROM binding b JOIN channel c ON c.id = b.channel_id
		JOIN principal_key k ON k.principal_id = b.principal_id
		WHERE k.vendor = ? AND k.key = ? AND b.state = 'live'`, adapter, session).Scan(&name)
	if err != nil && err != sql.ErrNoRows {
		f.t.Fatal(err)
	}
	return name
}

func fireHook(f *setupWorld, adapter, session, dir, event, fields string) string {
	f.t.Helper()
	cmd := f.installedHookCommand(f.t.Context(), dir, adapter, event, fields)
	cmd.Stdin = strings.NewReader(fmt.Sprintf(`{"hook_event_name":%q,"session_id":%q,"cwd":%q,%s}`, event, session, dir, fields))
	out, err := cmd.Output()
	if err != nil {
		f.t.Fatalf("%s %s of session %s: %v", adapter, event, session, err)
	}
	return string(out)
}

// startAs starts a session of the adapter in dir and returns its welcome,
// printed at the start or, when the start passed its deadline, at the first
// prompt.
func startAs(f *setupWorld, adapter, session, dir string) string {
	f.t.Helper()
	if out := fireHook(f, adapter, session, dir, "SessionStart", `"source":"startup"`); out != "" {
		return out
	}
	return fireHook(f, adapter, session, dir, "UserPromptSubmit", `"prompt_id":"first","turn_id":"first","prompt":"synthetic"`)
}
