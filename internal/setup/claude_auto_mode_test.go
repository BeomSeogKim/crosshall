package setup

import (
	"encoding/json"
	"testing"
)

func TestClaudeAutoModeEntries(t *testing.T) {
	const executable = `/opt/O'Brien's tools/crosshall`
	const guidance = `Allow direct shell invocations for local Crosshall team coordination using the installed executable at "/opt/O'Brien's tools/crosshall" or the bare command "crosshall" on PATH. The first argument must be exactly one of ask, inbox, join, leave, log, note, read, reply, status, wait, waiting, who. This exception also covers merging Crosshall stderr into stdout with 2>&1 and a single display-only pipe to head or tail that reads standard input with only an optional line count. Evaluate other shell operations and destinations under the normal rules.`
	for _, tc := range []struct {
		name            string
		includeDefaults bool
		want            []string
	}{
		{"new allow list", true, []string{"$defaults", guidance}},
		{"existing allow list", false, []string{guidance}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entries := claudeAutoModeEntries(executable, tc.includeDefaults)
			if len(entries) != len(tc.want) {
				t.Fatalf("got %d entries, want %d", len(entries), len(tc.want))
			}
			for i, entry := range entries {
				if !entry.AutoMode || entry.Event != "" || entry.Group != "" {
					t.Errorf("entry %d has incorrect routing: %+v", i, entry)
				}
				var value string
				if err := json.Unmarshal(entry.Value, &value); err != nil || value != tc.want[i] {
					t.Errorf("entry %d = %s, want %q, error %v", i, entry.Value, tc.want[i], err)
				}
			}
		})
	}
}
