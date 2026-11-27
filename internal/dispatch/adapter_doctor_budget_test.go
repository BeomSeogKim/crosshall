package dispatch

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A start or a prompt has the start budget: one that took 300 ms did not
// miss its deadline, nor did a Stop that took 160 ms, which may bind a
// missed launch (#437); a SessionEnd that took 160 ms did, and so did a
// prompt that ran out of time after the ordinary budget, which an enrolled
// member's prompt keeps.
func TestDoctorCountsMissedDeadlinesByTheCallbacksBudget(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	if err := os.MkdirAll(filepath.Join(dir, "hooks"), 0700); err != nil {
		t.Fatal(err)
	}
	records := "claude SessionStart rpc 300\nclaude UserPromptSubmit rpc 390\nclaude Stop clean 160\nclaude SessionEnd clean 160\n" +
		"claude SessionStart deadline 410\nclaude UserPromptSubmit deadline 160\nclaude UserPromptSubmit deadline 145\n"
	if err := os.WriteFile(filepath.Join(dir, "hooks", now.UTC().Format("20060102")), []byte(records), 0600); err != nil {
		t.Fatal(err)
	}
	rv, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer rv.Close()
	adapters := []doctorAdapter{{Adapter: "claude"}}
	attachDoctorHooks(adapters, rv, now)
	if h := adapters[0].Hooks; h == nil || h.RecordedStarts != 7 || h.MissedDeadlines != 3 {
		t.Fatalf("hooks: %+v, want 3 of 7 missed (the SessionEnd, the 410 ms start, the 160 ms prompt deadline)", h)
	}
}
