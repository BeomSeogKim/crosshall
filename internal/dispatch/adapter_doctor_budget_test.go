package dispatch

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A start or a prompt has the start budget: one that took 300 ms did not
// miss its deadline; a Stop that took 160 ms did.
func TestDoctorCountsMissedDeadlinesByTheCallbacksBudget(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	if err := os.MkdirAll(filepath.Join(dir, "hooks"), 0700); err != nil {
		t.Fatal(err)
	}
	records := "claude SessionStart rpc 300\nclaude UserPromptSubmit rpc 390\nclaude Stop clean 160\nclaude SessionStart deadline 410\n"
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
	if h := adapters[0].Hooks; h == nil || h.RecordedStarts != 4 || h.MissedDeadlines != 2 {
		t.Fatalf("hooks: %+v, want 2 of 4 missed (the Stop and the 410 ms start)", h)
	}
}
