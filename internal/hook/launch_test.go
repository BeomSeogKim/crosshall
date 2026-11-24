package hook

import (
	"crypto/sha256"
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

func launchFixture(t *testing.T) *os.Root {
	t.Helper()
	leaf := hookFileRoot(t)
	hookFile(t, leaf, "gen", "g1\n")
	return leaf
}

func launchCallback(pid, source string) (event, eventSpec) {
	name := "Stop"
	if source != "" {
		name = "SessionStart"
	}
	return event{Identity: ident.Identity{Vendor: "claude"}, LaunchRef: pid, Source: source, name: name},
		eventSpec{Name: name, Enroll: source != ""}
}

func checkLaunch(t *testing.T, leaf *os.Root, gen, pid, source string, at int64, verified bool) string {
	t.Helper()
	e, spec := launchCallback(pid, source)
	b, err := bindLaunch(leaf, gen, e, spec, time.UnixMilli(at))
	ref, got := b.ref, b.verified
	if err != nil || got != verified || (ref == "session") == verified || ref == "" {
		t.Fatalf("%s/%s at %d: ref=%q verified=%t err=%v", pid, source, at, ref, got, err)
	}
	return ref
}

func TestClaudeLaunchScopesStartsAndReusesBindings(t *testing.T) {
	leaf := launchFixture(t)
	a := checkLaunch(t, leaf, "g1", "0314", "startup", 100, true)
	b := checkLaunch(t, leaf, "g1", "315", "startup", 101, true)
	if a == b || !strings.HasPrefix(a, "pid-314-") || a == "pid-314" {
		t.Fatalf("launches %q %q", a, b)
	}
	for _, source := range []string{"", "compact"} {
		if got := checkLaunch(t, leaf, "g1", "314", source, 102, true); got != a {
			t.Fatalf("%s picked another launch: %q", source, got)
		}
	}
	updated := checkLaunch(t, leaf, "g1", "314", "resume", 103, true)
	if updated == a || updated != checkLaunch(t, leaf, "g1", "314", "resume", 103, true) {
		t.Fatal("resume must create its own replay-stable reference")
	}
	checkLaunch(t, leaf, "g1", "314", "clear", 102, false)
	checkLaunch(t, leaf, "g1", "314", "", 101, false)
	if got := checkLaunch(t, leaf, "g1", "314", "", 104, true); got != updated {
		t.Fatal("older start changed current launch")
	}
	if got := checkLaunch(t, leaf, "g1", "315", "", 104, true); got != b {
		t.Fatal("another PID changed launch")
	}
	hookFile(t, leaf, "gen", "g2\n")
	checkLaunch(t, leaf, "g2", "314", "", 105, false)
}

func TestClaudeLaunchAmbiguityAndSessionOnlyCallbacks(t *testing.T) {
	leaf := launchFixture(t)
	checkLaunch(t, leaf, "g1", "314", "compact", 9, false)
	checkLaunch(t, leaf, "g1", "314", "startup", 10, true)
	checkLaunch(t, leaf, "g1", "314", "clear", 10, false)
	checkLaunch(t, leaf, "g1", "314", "", 11, false)
	checkLaunch(t, leaf, "g1", "314", "resume", 12, true)
	checkLaunch(t, leaf, "g1", "", "startup", 13, false)
	// Capture is the manifest's, not the vendor's: an adapter without a
	// launch_env reports no launch PID and stays session-scoped, and any
	// adapter that reports one binds like Claude Code.
	if codex, _ := loadManifest("codex"); codex.LaunchEnv != "" {
		t.Fatalf("codex captures launches through %s", codex.LaunchEnv)
	}
	e, spec := launchCallback("", "startup")
	e.Identity.Vendor = "codex"
	if b, err := bindLaunch(leaf, "g1", e, spec, time.UnixMilli(14)); b.ref != "session" || b.verified || err != nil {
		t.Fatalf("session-only adapter: %+v, %v", b, err)
	}
	e, spec = launchCallback("316", "startup")
	e.Identity.Vendor = "fixture"
	if b, err := bindLaunch(leaf, "g1", e, spec, time.UnixMilli(15)); b.ref == "session" || !b.verified || err != nil {
		t.Fatalf("an adapter with a launch PID: %+v, %v", b, err)
	}
}

func TestConcurrentClaudeStartsRetainNewest(t *testing.T) {
	leaf := launchFixture(t)
	start, spec := launchCallback("313", "startup")
	var wg sync.WaitGroup
	ready := make(chan struct{})
	// All starts predate delivery, including any lock timeout recorded by the OS.
	base := time.Unix(100, 0)
	lastAt := base.Add(12 * time.Second)
	for i := int64(1); i <= 12; i++ {
		wg.Go(func() {
			<-ready
			if _, err := bindLaunch(leaf, "g1", start, spec, base.Add(time.Duration(i)*time.Second)); err != nil {
				t.Errorf("start: %v", err)
			}
		})
	}
	close(ready)
	wg.Wait()
	guard := fmt.Sprintf("launch-313-%x.lock", sha256.Sum256([]byte("g1")))
	marker, err := leaf.ReadFile(guard)
	if err != nil {
		t.Fatal(err)
	}
	if string(marker) == "?" {
		// A start missed the lock. No older PID binding is safe to verify.
		checkLaunch(t, leaf, "g1", "313", "", lastAt.UnixMilli()+1, false)
		return
	}
	last := fmt.Sprintf("pid-313-%x", sha256.Sum256([]byte(start.reference(lastAt))))
	if got := checkLaunch(t, leaf, "g1", "313", "", lastAt.UnixMilli()+1, true); got != last {
		t.Fatalf("latest start lost: %s, %s", last, got)
	}
}

func TestContendedStartCannotVerifyPrecedingPID(t *testing.T) {
	leaf := launchFixture(t)
	now := time.Now().UnixMilli()
	checkLaunch(t, leaf, "g1", "314", "startup", now-100, true)
	guard := fmt.Sprintf("launch-314-%x.lock", sha256.Sum256([]byte("g1")))
	f, err := leaf.Open(guard)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	checkLaunch(t, leaf, "g1", "314", "resume", now, false)
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	after := time.Now().UnixMilli() + 1
	end, spec := launchCallback("314", "")
	end.name, end.Reason, spec.Name = "SessionEnd", "other", "SessionEnd"
	if b, err := bindLaunch(leaf, "g1", end, spec, time.UnixMilli(after)); b.ref != "session" || b.verified || err != nil {
		t.Fatalf("contended start left prior launch eligible for departure: %+v, %v", b, err)
	}
	checkLaunch(t, leaf, "g1", "314", "startup", after, true)
	checkLaunch(t, leaf, "g1", "314", "", after+1, true)
}

// The dispatcher keys hook operations as hook:<adapter>:<launch>:<event> and
// refuses a launch reference containing ':', which would make keys ambiguous.
func TestClaudeLaunchReferenceHasNoKeySeparator(t *testing.T) {
	leaf := launchFixture(t)
	if ref := checkLaunch(t, leaf, "g1", "314", "startup", 100, true); strings.Contains(ref, ":") || len(ref) > 128 {
		t.Fatalf("launch reference %q", ref)
	}
}

func promptCallback(pid string) (event, eventSpec) {
	return event{Identity: ident.Identity{Vendor: "claude"}, LaunchRef: pid, Fact: wire.HookPromptSubmitted, EventRef: "turn-1:UserPromptSubmit",
			name: "UserPromptSubmit"},
		eventSpec{Name: "UserPromptSubmit", Fact: wire.HookPromptSubmitted}
}

// hosted makes the hook run under pid (true) or outside it (false) for the
// test's duration.
func hosted(t *testing.T, under bool) {
	t.Helper()
	previous := hostedBy
	hostedBy = func(int) bool { return under }
	t.Cleanup(func() { hostedBy = previous })
}

func launchRecord(t *testing.T, leaf *os.Root, gen, pid string) string {
	t.Helper()
	raw, err := leaf.ReadFile(fmt.Sprintf("launch-%s-%x", pid, sha256.Sum256([]byte(gen))))
	if err != nil {
		return ""
	}
	return string(raw)
}

// A start whose hook missed its deadline leaves no launch record; the
// session's first prompt writes it, under the proof a hand join uses, and
// every later callback of the process reads the same launch.
func TestPromptWritesTheLaunchRecordAMissedStartLeftOut(t *testing.T) {
	hosted(t, true)
	leaf := launchFixture(t)
	e, spec := promptCallback("314")
	b, err := bindLaunch(leaf, "g1", e, spec, time.UnixMilli(200))
	want := fmt.Sprintf("pid-314-%x", sha256.Sum256([]byte("late:"+e.reference(time.UnixMilli(200)))))
	if err != nil || b.ref != want || !b.verified || !b.late || b.atMS != 200 {
		t.Fatalf("late prompt: %+v, %v; want %s", b, err, want)
	}
	if got := checkLaunch(t, leaf, "g1", "314", "", 201, true); got != want {
		t.Fatalf("the next callback reads %q, want the late launch", got)
	}
	again, err := bindLaunch(leaf, "g1", e, spec, time.UnixMilli(202))
	if err != nil || again.ref != want || again.late || again.atMS != 200 {
		t.Fatalf("a later prompt of the launch: %+v, %v", again, err)
	}
	// A later start of the session replaces it; a callback timed before the
	// late record cannot use it.
	checkLaunch(t, leaf, "g1", "314", "", 150, false)
	resumed := checkLaunch(t, leaf, "g1", "314", "resume", 300, true)
	if resumed == want || checkLaunch(t, leaf, "g1", "314", "", 301, true) != resumed {
		t.Fatal("a later start did not replace the late launch")
	}
}

// A record written by a start that lost only its request is read by the
// prompt as any record, with its time for the dispatcher.
func TestPromptReportsTheRecordTimeOfAStartThatLostItsRequest(t *testing.T) {
	hosted(t, true)
	leaf := launchFixture(t)
	start := checkLaunch(t, leaf, "g1", "314", "startup", 100, true)
	e, spec := promptCallback("314")
	b, err := bindLaunch(leaf, "g1", e, spec, time.UnixMilli(200))
	if err != nil || b.ref != start || !b.verified || b.late || b.atMS != 100 {
		t.Fatalf("prompt after a start: %+v, %v", b, err)
	}
}

func TestOnlyAHostedPromptWritesALateLaunchRecord(t *testing.T) {
	for _, name := range []string{"Stop", "SessionEnd", "Notification", "compact"} {
		t.Run(name, func(t *testing.T) {
			hosted(t, true)
			leaf := launchFixture(t)
			e, spec := launchCallback("314", "")
			switch name {
			case "compact":
				e, spec = launchCallback("314", "compact")
			case "SessionEnd":
				e.name, e.Reason, spec.Name, e.Fact, spec.Fact = name, "other", name, wire.HookSessionEnded, wire.HookSessionEnded
			case "Notification":
				e.name, spec.Name, e.Fact, spec.Fact = name, name, wire.HookApprovalRequested, wire.HookApprovalRequested
			}
			checkLaunch(t, leaf, "g1", "314", e.Source, 200, false)
			if b, err := bindLaunch(leaf, "g1", e, spec, time.UnixMilli(200)); b.ref != "session" || b.verified || err != nil {
				t.Fatalf("%s: %+v, %v", name, b, err)
			}
			if record := launchRecord(t, leaf, "g1", "314"); record != "" {
				t.Fatalf("%s wrote a launch record: %q", name, record)
			}
		})
	}
	t.Run("not hosted", func(t *testing.T) {
		hosted(t, false)
		leaf := launchFixture(t)
		e, spec := promptCallback("314")
		if b, err := bindLaunch(leaf, "g1", e, spec, time.UnixMilli(200)); b.ref != "session" || b.verified || err != nil {
			t.Fatalf("a prompt from a session nested under the process: %+v, %v", b, err)
		}
		if record := launchRecord(t, leaf, "g1", "314"); record != "" {
			t.Fatalf("wrote %q", record)
		}
	})
}

// A start that could not take the lock left its mark: no later callback can
// tell which conversation the process runs, so a prompt does not guess. A
// start that holds the lock right now wins.
func TestPromptLeavesTheRecordToAStartItCannotRuleOut(t *testing.T) {
	hosted(t, true)
	guardName := fmt.Sprintf("launch-314-%x.lock", sha256.Sum256([]byte("g1")))
	t.Run("missed-lock mark", func(t *testing.T) {
		leaf := launchFixture(t)
		hookFile(t, leaf, guardName, "?")
		e, spec := promptCallback("314")
		if b, err := bindLaunch(leaf, "g1", e, spec, time.UnixMilli(time.Now().UnixMilli()+1000)); b.ref != "session" || err != nil {
			t.Fatalf("prompt after a missed lock: %+v, %v", b, err)
		}
		if record := launchRecord(t, leaf, "g1", "314"); record != "" {
			t.Fatalf("wrote %q", record)
		}
	})
	t.Run("start holds the lock", func(t *testing.T) {
		leaf := launchFixture(t)
		hookFile(t, leaf, guardName, "")
		f, err := leaf.Open(guardName)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			t.Fatal(err)
		}
		e, spec := promptCallback("314")
		if b, err := bindLaunch(leaf, "g1", e, spec, time.UnixMilli(200)); b.ref != "session" || err != nil {
			t.Fatalf("prompt while a start runs: %+v, %v", b, err)
		}
		// Also when the start takes the lock after the prompt's shared read.
		if b, err := lateLaunch(leaf, strings.TrimSuffix(guardName, ".lock"), 314, e, time.UnixMilli(200)); b.ref != "session" || err != nil {
			t.Fatalf("late write while a start runs: %+v, %v", b, err)
		}
		if record := launchRecord(t, leaf, "g1", "314"); record != "" {
			t.Fatalf("wrote %q", record)
		}
	})
}

// A start can write its record between the prompt's shared read and its
// exclusive lock; the prompt then leaves that record alone.
func TestLatePromptKeepsARecordAStartWroteMeanwhile(t *testing.T) {
	hosted(t, true)
	leaf := launchFixture(t)
	start := checkLaunch(t, leaf, "g1", "314", "startup", 100, true)
	before := launchRecord(t, leaf, "g1", "314")
	e, _ := promptCallback("314")
	name := fmt.Sprintf("launch-314-%x", sha256.Sum256([]byte("g1")))
	if b, err := lateLaunch(leaf, name, 314, e, time.UnixMilli(200)); b.ref != "session" || err != nil {
		t.Fatalf("late write over a start's record: %+v, %v", b, err)
	}
	if launchRecord(t, leaf, "g1", "314") != before || checkLaunch(t, leaf, "g1", "314", "", 201, true) != start {
		t.Fatal("the start's record changed")
	}
}
