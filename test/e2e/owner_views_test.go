//go:build e2e

package e2e

import (
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestOwnerWhoKeepsClaudePromptPickupAcrossAnUpgrade(t *testing.T) {
	f := newSetupWorld(t)
	f.apply("", "setup", "claude", "--apply")
	repo := f.enrollmentRepo()
	const currentLaunch = `principal_id = (SELECT principal_id FROM principal_key WHERE vendor = 'claude' AND key = ?)
		AND json_extract(profile, '$.manifest_rev') = 'claude-2'`
	key := setupSessionPrefix + "claude"
	reader := f.db()
	counters, err := f.hookCounterRecords("claude", "SessionStart")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(fixtureEventTimeout)
	for attempt := 1; ; attempt++ {
		f.invokeHook(repo, "claude", "SessionStart", `"source":"startup"`)
		f.waitEnrollmentPackets("claude", "SessionStart", deadline)
		var launches int
		if err := reader.QueryRow(`SELECT COUNT(*) FROM adapter_launch WHERE `+currentLaunch, key).Scan(&launches); err != nil {
			t.Fatal(err)
		}
		if launches == 1 {
			break
		}
		after, err := f.hookCounterRecords("claude", "SessionStart")
		if err != nil {
			t.Fatal(err)
		}
		if launches != 0 || len(after) != len(counters)+1 || !strings.HasPrefix(after[len(counters)], "claude SessionStart deadline ") ||
			!time.Now().Before(deadline) {
			t.Fatalf("prepare the launch to upgrade after %d callbacks: %d matching rows; %s", attempt, launches, f.enrollmentEvidence("claude"))
		}
		counters = after
	}
	owner := f.owner()
	owner.must("owner", "stop")

	db, err := sql.Open("sqlite", "file:"+filepath.Join(f.state, "store.sqlite3")+"?mode=rw")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	result, err := db.Exec(`UPDATE adapter_launch SET profile = json_set(profile, '$.manifest_rev', 'claude-1') WHERE `+currentLaunch, key)
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := result.RowsAffected(); err != nil || changed != 1 {
		t.Fatalf("set the previous release's launch revision: %d rows, %v", changed, err)
	}
	owner.must("owner", "start")
	owner.must("owner", "publish", "hall", "off")
	checkReach := func(stage string) {
		t.Helper()
		out := owner.mustPlain("owner", "who", "--all")
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, "@claude1") && strings.Contains(line, "sees work at its next prompt") {
				return
			}
		}
		t.Fatalf("%s: owner who omitted Claude prompt pickup: %s", stage, out)
	}
	checkReach("before the next prompt")

	writer := f.member()
	writer.must("join", "setup-check", "--as", "writer")
	id := num(t, writer.must("ask", "@claude1", "Check this after the upgrade"), "id")
	got := f.invokeHook(repo, "claude", "UserPromptSubmit", `"prompt_id":"after-upgrade"`)
	if !strings.Contains(got, "[crosshall] pending: "+strconv.FormatInt(id, 10)) {
		t.Fatalf("the current hook did not show the pending request: %s", got)
	}
	checkReach("after the next prompt")
}

func TestOwnerViewsIncludeDepartedBindingsAndDoNotPickUpMessages(t *testing.T) {
	w, owner, members := setup(t)
	form, oldTax := members[0], members[1]
	owner.must("owner", "channel", "create", "inventory")
	owner.must("owner", "channel", "create", "empty")
	stock, reader, replacement := w.member(), w.member(), w.member()
	stock.must("join", "inventory", "--as", "stock")
	reader.must("join", "inventory", "--as", "reader")
	stock.must("note", "@reader", "inventory only")
	oldTax.must("leave")
	replacement.must("join", "checkout", "--as", "tax")
	ask := num(t, form.must("ask", "@tax", "checkout only"), "id")

	roster := owner.must("owner", "who", "--all")
	bindings := make(map[string]float64)
	for _, raw := range roster.obj["channels"].([]any) {
		ch := raw.(map[string]any)
		if ch["name"] == "empty" && ch["members"] == nil {
			t.Fatal("empty channel did not have a member array")
		}
		if ch["name"] != "checkout" {
			continue
		}
		for _, row := range ch["members"].([]any) {
			member := row.(map[string]any)
			if member["alias"] == "tax" {
				bindings[member["binding_state"].(string)] = member["binding_id"].(float64)
			}
		}
	}
	if len(roster.obj["channels"].([]any)) != 3 || bindings["live"] == 0 || bindings["departed"] == 0 || bindings["live"] == bindings["departed"] {
		t.Fatalf("unexpected roster %s", roster.stdout)
	}
	status := owner.must("owner", "status", "--channel", "checkout")
	ledger := status.obj["channels"].([]any)[0].(map[string]any)
	for _, raw := range ledger["bindings"].([]any) {
		binding := raw.(map[string]any)
		if binding["alias"] == "tax" && bindings[binding["state"].(string)] != binding["id"] {
			t.Fatalf("roster and ledger disagree on the binding: %v", binding)
		}
	}
	log := owner.must("owner", "log", "checkout")
	if log.obj["channel"] != "checkout" || !strings.Contains(log.stdout, "checkout only") || strings.Contains(log.stdout, "inventory only") {
		t.Fatalf("wrong history %s", log.stdout)
	}
	inbox := replacement.must("inbox")
	if !has(ids(inbox.obj["unread"]), ask) {
		t.Fatalf("owner log picked up #%d: %s", ask, inbox.stdout)
	}
	replacement.must("read", strconv.FormatInt(ask, 10))
	for _, args := range [][]string{{"who", "--all"}, {"log", "checkout"}, {"doctor"}} {
		if out := form.run(append([]string{"owner"}, args...)...); out.exit != 3 || out.obj["code"] != "owner_only" {
			t.Fatalf("member ran owner %v: exit %d %s", args, out.exit, out.stdout)
		}
	}
}

func TestRepositoryDefaultJoinAndReplay(t *testing.T) {
	w := newWorld(t)
	owner := w.owner()
	owner.must("owner", "channel", "create", "checkout")
	owner.must("owner", "channel", "create", "inventory")
	marker := filepath.Join(w.home, ".git")
	if err := os.Mkdir(marker, 0700); err != nil {
		t.Fatal(err)
	}
	head := filepath.Join(marker, "HEAD")
	headRef := "ref: refs/heads/main\n"
	if err := os.WriteFile(head, []byte(headRef), 0600); err != nil {
		t.Fatal(err)
	}
	member := w.member()
	if out := member.run("join", "--as", "form"); out.exit != 3 || out.obj["code"] != "not_found" {
		t.Fatalf("opted-out repository joined: exit %d %s", out.exit, out.stdout)
	}
	owner.must("owner", "channel", "default", "checkout", "--repo", w.home)
	join := []string{"--key", "original-join", "join", "--as", "form"}
	first := member.must(join...)
	if first.obj["channel"] != "checkout" {
		t.Fatalf("did not join the configured channel: %s", first.stdout)
	}
	owner.must("owner", "channel", "default", "inventory")
	if replay := member.must(join...); replay.stdout != first.stdout {
		t.Fatalf("retry was redirected: first %s replay %s", first.stdout, replay.stdout)
	}
	if out := member.must("join"); out.obj["channel"] != "checkout" {
		t.Fatalf("changed default displaced the selected channel: %s", out.stdout)
	}
	if out := w.member().must("join"); out.obj["channel"] != "inventory" {
		t.Fatalf("new member missed the changed default: %s", out.stdout)
	}
	if out := w.member().must("join", "checkout", "--as", "tax"); out.obj["channel"] != "checkout" {
		t.Fatalf("default overrode the explicit channel: %s", out.stdout)
	}
	if entries, err := os.ReadDir(marker); err != nil || len(entries) != 1 || entries[0].Name() != "HEAD" {
		t.Fatalf("default modified the repository metadata: %v %v", entries, err)
	}
	if data, err := os.ReadFile(head); err != nil || string(data) != headRef {
		t.Fatalf("default modified the repository's HEAD: %q %v", data, err)
	}
}

func TestOwnerDoctorDoesNotStartDispatcher(t *testing.T) {
	w := newWorld(t)
	owner := w.owner()
	fresh := owner.run("owner", "doctor")
	if fresh.exit != 1 || fresh.obj["ok"] != false {
		t.Fatalf("fresh doctor: exit %d %s", fresh.exit, fresh.stdout)
	}
	if _, err := os.Stat(w.state); !os.IsNotExist(err) {
		t.Fatalf("doctor initialized state: %v", err)
	}
	owner.must("owner", "start")
	healthy := owner.must("owner", "doctor")
	checks := healthy.obj["checks"].([]any)
	if len(checks) != 5 {
		t.Fatalf("unexpected diagnostics %s", healthy.stdout)
	}
	for _, raw := range checks {
		if check := raw.(map[string]any); check["ok"] != true {
			t.Fatalf("healthy check failed: %v", check)
		}
	}
	w.kill()
	stopped := owner.run("owner", "doctor")
	if stopped.exit != 1 || stopped.obj["ok"] != false || len(w.dispatcherPIDs()) != 0 {
		t.Fatalf("doctor restarted dispatcher or missed the failure: exit %d %s", stopped.exit, stopped.stdout)
	}
}
