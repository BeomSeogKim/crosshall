//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// A crosshall binary replaced without rerunning setup leaves the hooks of
// the earlier revision on disk. The owner is told at owner start, in doctor
// and in setup status, with the command that installs the current hooks,
// and no longer once setup has run.
func TestOwnerIsToldWhenInstalledHooksAreOlderThanTheBinary(t *testing.T) {
	f := newSetupWorld(t)
	f.apply("", "setup", "claude", "--apply")

	// Put the settings file and setup's record back to what claude-2 left:
	// its notification hook matched permission prompts only.
	journalPath := filepath.Join(f.state, "install-journal.json")
	journal, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	recorded := regexp.MustCompile(`"manifest_rev":"(claude-[0-9]+)"`).FindSubmatch(journal)
	if recorded == nil || string(recorded[1]) == "claude-2" {
		t.Fatalf("recorded Claude revision: %s", journal)
	}
	current := string(recorded[1])
	journal = bytes.Replace(journal, recorded[0], []byte(`"manifest_rev":"claude-2"`), 1)
	for path, content := range map[string][]byte{journalPath: journal, f.paths["claude"]: readSetupFile(t, f.paths["claude"]).bytes} {
		matcher := []byte(`"matcher":"permission_prompt|idle_prompt"`)
		if bytes.Count(content, matcher) != 1 {
			t.Fatalf("%s: expected one current notification matcher: %s", path, content)
		}
		if err := os.WriteFile(path, bytes.Replace(content, matcher, []byte(`"matcher":"permission_prompt"`), 1), 0600); err != nil {
			t.Fatal(err)
		}
	}

	text := func(args ...string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, f.executable, args...)
		cmd.Dir, cmd.Env = f.home, f.env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("crosshall %v: %v, %s", args, err, out)
		}
		return string(out)
	}
	sentence := "The Claude Code hooks on disk were installed for claude-2; this crosshall installs " + current +
		". Run crosshall setup claude."
	installedRevs := func() (doctor, status string, warnings []string) {
		t.Helper()
		var d struct {
			Adapters []struct {
				Adapter      string `json:"adapter"`
				InstalledRev string `json:"installed_rev"`
			} `json:"adapters"`
			Warnings []string `json:"warnings"`
		}
		if err := json.Unmarshal([]byte(f.apply("", "owner", "doctor").stdout), &d); err != nil {
			t.Fatal(err)
		}
		s := d
		if err := json.Unmarshal([]byte(f.apply("", "setup", "status").stdout), &s); err != nil {
			t.Fatal(err)
		}
		for _, a := range d.Adapters {
			if a.Adapter == "claude" {
				doctor = a.InstalledRev
			}
		}
		for _, a := range s.Adapters {
			if a.Adapter == "claude" {
				status = a.InstalledRev
			}
		}
		return doctor, status, d.Warnings
	}

	if got := text("owner", "start"); got != "dispatcher running\n"+sentence+"\n" {
		t.Errorf("owner start = %q", got)
	}
	if got := text("owner", "doctor"); strings.Count(got, sentence+"\n") != 1 || !strings.Contains(got, "claude adapter — partial, ") {
		t.Errorf("owner doctor = %s", got)
	}
	if got := text("setup", "status"); strings.Count(got, "\n  "+sentence+"\n") != 1 || !strings.Contains(got, "Claude Code — CLI found, hooks drift, ") {
		t.Errorf("setup status = %s", got)
	}
	if doctor, status, warnings := installedRevs(); doctor != "claude-2" || status != "claude-2" || !slices.Contains(warnings, sentence) {
		t.Errorf("JSON: doctor %q, setup status %q, warnings %q", doctor, status, warnings)
	}

	f.apply("", "setup", "claude", "--apply")
	if got := text("owner", "start"); got != "dispatcher running\n" {
		t.Errorf("owner start after setup = %q", got)
	}
	for _, args := range [][]string{{"owner", "doctor"}, {"setup", "status"}} {
		if got := text(args...); strings.Contains(got, "were installed for") {
			t.Errorf("%v after setup = %s", args, got)
		}
	}
	if doctor, status, warnings := installedRevs(); doctor != current || status != current || len(warnings) != 0 {
		t.Errorf("JSON after setup: doctor %q, setup status %q, warnings %q", doctor, status, warnings)
	}
}
