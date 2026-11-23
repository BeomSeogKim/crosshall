//go:build e2e

package e2e

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTelemetrySettingsAndDurableOptOut(t *testing.T) {
	w := newWorld(t)
	owner := w.owner()
	modeFile := filepath.Join(w.state, "telemetry", "mode")
	if got := owner.must("owner", "telemetry"); got.obj["mode"] != "unset" || got.obj["endpoint"] != "" {
		t.Fatal(got.stdout)
	}
	if _, err := os.Stat(modeFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected mode file: %v", err)
	}
	checkMode := func(want string) {
		t.Helper()
		content, err := os.ReadFile(modeFile)
		if err != nil || !strings.HasPrefix(string(content), want+" ") {
			t.Fatalf("mode=%q, want %s: %v", content, want, err)
		}
		if info, err := os.Stat(modeFile); err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("mode file metadata: %v, %v", info, err)
		}
	}
	owner.must("owner", "telemetry", "off")
	checkMode("off")
	if _, err := os.Stat(filepath.Join(w.state, "store.sqlite3")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("telemetry off opened the dispatcher store: %v", err)
	}
	if got := owner.must("owner", "telemetry", "on"); got.obj["mode"] != "on" || got.obj["eligible_from_day"] == nil {
		t.Fatal(got.stdout)
	}
	checkMode("on")

	owner.must("owner", "channel", "create", "dev")
	member := w.member()
	member.extra = []string{"DO_NOT_TRACK=1"}
	member.must("join", "dev", "--as", "member")
	checkMode("on")

	managed := w.owner()
	managed.extra = []string{"DO_NOT_TRACK=1"}
	got := managed.must("owner", "telemetry")
	if got.obj["mode"] != "off" || got.obj["decision_source"] != "environment" ||
		got.obj["environment_opt_out"] != true || !strings.Contains(got.stderr, "turned off") {
		t.Fatalf("stdout=%s stderr=%s", got.stdout, got.stderr)
	}
	checkMode("off")
	if refused := managed.run("owner", "telemetry", "on"); refused.exit == 0 {
		t.Fatalf("enabled with an environment opt-out: %s", refused.stdout)
	}
	checkMode("off")
	owner.must("owner", "stop")
	owner.must("owner", "start")
	if got := owner.must("owner", "telemetry"); got.obj["mode"] != "off" {
		t.Fatal(got.stdout)
	}
}

func TestSentUsageReportCanBeReadByteForByte(t *testing.T) {
	w := newWorld(t)
	owner := w.owner()
	if got := owner.mustPlain("owner", "telemetry", "sent"); got != "No sent usage reports.\n" {
		t.Fatalf("empty archive: %q", got)
	}
	day := time.Now().UTC().AddDate(0, 0, -1).Format(time.DateOnly)
	dir := filepath.Join(w.state, "telemetry", "sent")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	const body = "{\n  \"counts\": { \"sample\": 1 }\n}\n"
	if err := os.WriteFile(filepath.Join(dir, day+".json"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	if got := owner.mustPlain("owner", "telemetry", "sent"); got != day+"\n" {
		t.Fatalf("archive days: %q", got)
	}
	if got := owner.mustPlain("owner", "telemetry", "sent", "--day", day); got != body {
		t.Fatalf("report bytes: %q", got)
	}
}
