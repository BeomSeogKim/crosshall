//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDomainClockComesFromDispatcherStartup(t *testing.T) {
	w := newWorld(t)
	owner := w.owner()
	owner.extra = []string{"CROSSHALL_TEST_CLOCK_OFFSET=2h"}
	owner.must("owner", "start")
	owner.must("owner", "channel", "create", "sample")
	member := w.member()
	member.extra = []string{"CROSSHALL_TEST_CLOCK_OFFSET=12h"}
	member.must("join", "sample", "--as", "viewer")
	start := time.Now()
	status := member.must("status")
	requireGenerationTime(t, status.obj, start, 2*time.Hour)
}

func TestInvalidDevelopmentClockPreventsStartup(t *testing.T) {
	w := newWorld(t)
	owner := w.owner()
	owner.extra = []string{"CROSSHALL_TEST_CLOCK_OFFSET=later"}
	if result := owner.run("owner", "start"); result.exit == 0 {
		t.Fatal("dispatcher started with invalid clock offset")
	}
	log, err := os.ReadFile(filepath.Join(w.state, "dispatcher.log"))
	if err != nil || !strings.Contains(string(log), "CROSSHALL_TEST_CLOCK_OFFSET") {
		t.Fatalf("clock error missing from dispatcher log: %s (%v)", log, err)
	}
}

func TestTaggedBuildIgnoresTestClock(t *testing.T) {
	tagged := filepath.Join(t.TempDir(), "crosshall")
	build := exec.Command("go", "build", "-ldflags=-X main.version=v1.2.3", "-o", tagged, "../../cmd/crosshall")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build tagged binary: %s (%v)", out, err)
	}
	for _, offset := range []string{"24h", "invalid"} {
		t.Run(offset, func(t *testing.T) {
			w := newWorld(t)
			run := func(args ...string) map[string]any {
				t.Helper()
				cmd := exec.Command(tagged, append([]string{"--json"}, args...)...)
				cmd.Env = append(w.baseEnv(), "CROSSHALL_TEST_CLOCK_OFFSET="+offset)
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("%v: %s (%v)", args, out, err)
				}
				var result map[string]any
				if err := json.Unmarshal(out, &result); err != nil {
					t.Fatalf("%v returned invalid JSON: %s", args, out)
				}
				return result
			}
			run("owner", "start")
			start := time.Now()
			requireGenerationTime(t, run("owner", "status", "--all"), start, 0)
			doctor := run("owner", "doctor")
			checks, _ := doctor["checks"].([]any)
			for _, item := range checks {
				check, _ := item.(map[string]any)
				detail, _ := check["detail"].(string)
				if check["ok"] == true && strings.Contains(detail, "CROSSHALL_TEST_CLOCK_OFFSET") &&
					strings.Contains(detail, "ignore") {
					return
				}
			}
			t.Fatalf("doctor did not report that release builds ignore the test clock: %v", doctor)
		})
	}
}

func requireGenerationTime(t *testing.T, status map[string]any, start time.Time, offset time.Duration) {
	t.Helper()
	got, ok := status["generated_at_ms"].(float64)
	if !ok || int64(got) < start.Add(offset).UnixMilli() || int64(got) > time.Now().Add(offset).UnixMilli() {
		t.Fatalf("status timestamp outside the request interval for offset %s: %v", offset, status)
	}
}
