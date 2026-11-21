package setup

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestMemberCommandRulesUseBothSpellingsAndOnlyRoutineVerbs(t *testing.T) {
	const executable = "/usr/local/bin/crosshall"
	verbs := []string{"ask", "inbox", "join", "leave", "log", "note", "read", "reply", "status", "wait", "waiting", "who"}
	for _, tc := range []struct {
		adapter string
		format  string
	}{
		{"claude", "Bash(%s %s:*)"},
		{"agy", "command(%s %s)"},
	} {
		t.Run(tc.adapter, func(t *testing.T) {
			want := make([]string, 0, 2*len(verbs))
			for _, program := range []string{executable, "crosshall"} {
				for _, verb := range verbs {
					want = append(want, fmt.Sprintf(tc.format, program, verb))
				}
			}
			got := renderedMemberRules(t, tc.adapter, executable)
			if !slices.Equal(got, want) {
				t.Errorf("rules = %q, want %q", got, want)
			}
		})
	}
}

func TestCodexMemberCommandRulesUseHostResolutionAndOnlyRoutineVerbs(t *testing.T) {
	const executable = "/usr/local/bin/crosshall"
	verbs := []string{"ask", "inbox", "join", "leave", "log", "note", "read", "reply", "status", "wait", "waiting", "who"}
	want := []string{`host_executable(name="crosshall", paths=["/usr/local/bin/crosshall"])`}
	for _, verb := range verbs {
		want = append(want, fmt.Sprintf(`prefix_rule(pattern=["crosshall", %q], decision="allow")`, verb))
	}
	got := renderedMemberRules(t, "codex", executable)
	if !slices.Equal(got, want) {
		t.Errorf("rules = %q, want %q", got, want)
	}
}

func TestMemberCommandRulesPreserveShellAndLiteralPathSpelling(t *testing.T) {
	const executable = `/opt/O'Brien's tools/crosshall`
	const shellPath = `'/opt/O'"'"'Brien'"'"'s tools/crosshall'`
	for _, tc := range []struct {
		adapter string
		want    string
	}{
		{"claude", "Bash(" + shellPath + " ask:*)"},
		{"agy", "command(" + shellPath + " ask)"},
		{"codex", `host_executable(name="crosshall", paths=["/opt/O'Brien's tools/crosshall"])`},
	} {
		t.Run(tc.adapter, func(t *testing.T) {
			if got := renderedMemberRules(t, tc.adapter, executable); len(got) == 0 || got[0] != tc.want {
				t.Errorf("rules = %q, want first rule %q", got, tc.want)
			}
		})
	}
}

func TestMemberCommandRulesRejectUnrepresentablePaths(t *testing.T) {
	for _, tc := range []struct {
		adapter    string
		executable string
	}{
		{"claude", "crosshall"},
		{"codex", "./crosshall"},
		{"codex", "/opt/team-crosshall"},
		{"agy", ""},
		{"claude", "/opt/tool*/crosshall"},
		{"agy", "/opt/tool\nname/crosshall"},
		{"claude", "/opt/tool\rname/crosshall"},
		{"agy", "/opt/tool\xff/crosshall"},
		{"codex", "/opt/tool\x00/crosshall"},
		{"unknown", "/opt/tool/crosshall"},
	} {
		if entries, text, err := memberCommandRules(tc.adapter, tc.executable); err == nil || entries != nil || text != nil {
			t.Errorf("%s %q: got entries %v, text %q, error %v", tc.adapter, tc.executable, entries, text, err)
		}
	}
}

func TestMemberCommandRulesKeepLiteralAsteriskWhereSupported(t *testing.T) {
	const executable = "/opt/tool*/crosshall"
	for _, tc := range []struct {
		adapter string
		want    string
	}{
		{"agy", "command('" + executable + "' ask)"},
		{"codex", `host_executable(name="crosshall", paths=["/opt/tool*/crosshall"])`},
	} {
		if got := renderedMemberRules(t, tc.adapter, executable); len(got) == 0 || got[0] != tc.want {
			t.Errorf("%s rules = %q, want first rule %q", tc.adapter, got, tc.want)
		}
	}
}

func renderedMemberRules(t *testing.T, adapter, executable string) []string {
	t.Helper()
	entries, text, err := memberCommandRules(adapter, executable)
	if err != nil {
		t.Fatal(err)
	}
	if adapter == "codex" {
		if entries != nil || !strings.HasSuffix(string(text), "\n") {
			t.Fatalf("Codex rules returned entries %v, text %q", entries, text)
		}
		return strings.Split(strings.TrimSuffix(string(text), "\n"), "\n")
	}
	if text != nil {
		t.Fatalf("%s rules returned file text %q", adapter, text)
	}
	rules := make([]string, 0, len(entries))
	for _, entry := range entries {
		var rule string
		if err := json.Unmarshal(entry.Value, &rule); entry.Event != "" || entry.Group != "" || err != nil {
			t.Fatalf("%s returned non-permission entry %+v, error %v", adapter, entry, err)
		}
		rules = append(rules, rule)
	}
	return rules
}
