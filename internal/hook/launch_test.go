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
	ref, got, err := bindLaunch(leaf, gen, e, spec, time.UnixMilli(at))
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
	if ref, verified, err := bindLaunch(leaf, "g1", e, spec, time.UnixMilli(14)); ref != "session" || verified || err != nil {
		t.Fatalf("session-only adapter: %q, %t, %v", ref, verified, err)
	}
	e, spec = launchCallback("316", "startup")
	e.Identity.Vendor = "fixture"
	if ref, verified, err := bindLaunch(leaf, "g1", e, spec, time.UnixMilli(15)); ref == "session" || !verified || err != nil {
		t.Fatalf("an adapter with a launch PID: %q, %t, %v", ref, verified, err)
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
			if _, _, err := bindLaunch(leaf, "g1", start, spec, base.Add(time.Duration(i)*time.Second)); err != nil {
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
	if ref, verified, err := bindLaunch(leaf, "g1", end, spec, time.UnixMilli(after)); ref != "session" || verified || err != nil {
		t.Fatalf("contended start left prior launch eligible for departure: %q, %t, %v", ref, verified, err)
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
