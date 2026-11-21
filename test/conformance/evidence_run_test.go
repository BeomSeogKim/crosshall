//go:build conformance

package conformance

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestEvidenceRunFixture is an evidence run with stand-in cases. It does
// nothing on its own: TestEvidenceNeedsEveryCase starts it again in a
// process of its own, where -run can filter its cases.
func TestEvidenceRunFixture(t *testing.T) {
	variant := os.Getenv("CROSSHALL_EVIDENCE_FIXTURE")
	if variant == "" {
		return
	}
	run := &evidenceRun{t: t}
	run.run("first", func(*testing.T) string { return "1.2.3" })
	run.run("second", func(t *testing.T) string {
		switch variant {
		case "skips":
			t.Skip("no vendor binary")
		case "other build":
			return "1.2.4"
		case "no build":
			return "unverified"
		}
		return "1.2.3"
	})
	if build, write := run.evidenceBuild(true); write {
		fmt.Printf("fixture: evidence written for %s\n", build)
	}
}

// A tested row stands on every case of its run. go test reports a case that
// -run filtered out, and one that skipped, as passed, so a run that was asked
// for evidence must fail and write nothing unless every case ran to its end
// on one release build.
func TestEvidenceNeedsEveryCase(t *testing.T) {
	const fixture = "^TestEvidenceRunFixture$"
	for _, tc := range []struct{ name, variant, filter, want string }{
		{"every case ran", "complete", fixture, "fixture: evidence written for 1.2.3"},
		{"a case was filtered out", "complete", fixture + "/^first$", "these cases did not run to their end: second"},
		{"a case skipped", "skips", fixture, "these cases did not run to their end: second"},
		{"the cases saw different builds", "other build", fixture, "the cases observed different builds, 1.2.3 and 1.2.4"},
		{"a case saw no release build", "no build", fixture, `observed the build "unverified"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run", tc.filter, "-test.count=1")
			cmd.Env = append(os.Environ(), "CROSSHALL_EVIDENCE_FIXTURE="+tc.variant)
			out, err := cmd.CombinedOutput()
			wrote := strings.Contains(string(out), "evidence written")
			if !strings.Contains(string(out), tc.want) || wrote != (tc.variant == "complete" && tc.filter == fixture) || wrote != (err == nil) {
				t.Fatalf("run ended with %v, want %q:\n%s", err, tc.want, out)
			}
		})
	}
}

// The same for the two real turn-end runs: filtered to one case, each still
// passes that case, and fails without writing a file.
func TestAFilteredTurnEndRunWritesNoEvidence(t *testing.T) {
	(&conformance{t: t}).requireClaude()
	for name, exporter := range map[string]string{"print": "TestClaudeCodeTurnEndConformance", "interactive": "TestClaudeCodeInteractiveTurnEndConformance"} {
		t.Run(name, func(t *testing.T) {
			dir, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run", "^"+exporter+"$/^no_evidence$", "-test.count=1", "-test.v")
			cmd.Env = append(os.Environ(), "CROSSHALL_CONFORMANCE_OUT="+dir)
			out, err := cmd.CombinedOutput()
			files, readErr := os.ReadDir(dir)
			if err == nil || readErr != nil || len(files) != 0 ||
				!strings.Contains(string(out), "--- PASS: "+exporter+"/no_evidence") || !strings.Contains(string(out), "no evidence is written: these cases did not run to their end") {
				t.Fatalf("filtered run ended with %v and %d files:\n%s", err, len(files), out)
			}
		})
	}
}

// Claude Code can run callbacks at the same time. The probe must record
// every one of them whole, also when a payload is larger than one write.
func TestConcurrentCallbacksAreAllRecorded(t *testing.T) {
	c := &conformance{t: t, root: t.TempDir()}
	probe := c.writeProbe()
	const callbacks = 24
	var wg sync.WaitGroup
	for i := range callbacks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.Command(probe)
			cmd.Env = []string{"PATH=/usr/bin:/bin", fmt.Sprintf("CLAUDE_PID=%d", i)}
			cmd.Stdin = strings.NewReader(fmt.Sprintf(`{"hook_event_name":"Stop","session_id":"s","prompt_id":"p%d","padding":"%s"}`, i, strings.Repeat("x", 70000)))
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("probe: %v\n%s", err, out)
			}
		}()
	}
	wg.Wait()
	seen := map[string]bool{}
	for _, e := range c.events() {
		seen[e.Process+" "+e.Prompt] = e.Name == "Stop" && e.Session == "s"
	}
	for i := range callbacks {
		if !seen[fmt.Sprintf("%d p%d", i, i)] {
			t.Errorf("callback %d was not recorded whole (%d records)", i, len(seen))
		}
	}
}
