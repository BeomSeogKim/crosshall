package setup

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"testing"
)

func TestConfigEntriesPreserveUnrelatedBytes(t *testing.T) {
	before := []byte("{\n  \"last\": { \"z\": 1e2, \"a\": [true, null] },\n  \"hooks\": { \"Stop\": [ {\"hooks\": [ {\"type\": \"prompt\"} ] } ] },\n  \"permissions\": {\"deny\": [\"other\"], \"allow\": [\"Bash(existing)\"]},\n  \"first\": \"\\u0031\"\n}\n")
	hook := Entry{Event: "Stop", Value: json.RawMessage(`{"hooks":[{"type":"command","command":"/cross hall","args":["hook","claude","Stop"],"timeout":1}]}`)}
	allow := Entry{Value: json.RawMessage(`"Bash(crosshall who)"`)}
	updated, added, err := addEntries(before, []Entry{hook, allow})
	if err != nil || len(added) != 2 || !json.Valid(updated) {
		t.Fatalf("added %v: %v", added, err)
	}
	for _, raw := range []string{`{"hooks": [ {"type": "prompt"} ] }`, `{ "z": 1e2, "a": [true, null] }`, `"deny": ["other"]`, `"first": "\u0031"`} {
		if !bytes.Contains(updated, []byte(raw)) {
			t.Errorf("lost raw bytes %q in %s", raw, updated)
		}
	}
	counts, err := entryMatches(updated, []Entry{hook, allow})
	if err != nil || !slices.Equal(counts, []int{1, 1}) {
		t.Fatalf("installed counts %v: %v", counts, err)
	}
	idempotent, added, err := addEntries(updated, []Entry{hook, allow})
	if err != nil || len(added) != 0 || !bytes.Equal(updated, idempotent) {
		t.Fatalf("second setup changed config: %s, %v", idempotent, err)
	}
	restored, removed, remaining, err := removeEntries(updated, []Entry{hook, allow})
	if err != nil || len(removed) != 2 || len(remaining) != 0 || !bytes.Equal(restored, before) {
		t.Fatalf("uninstall changed unrelated bytes: %s, %v", restored, err)
	}
}

func TestConfigEntriesCreateMissingSections(t *testing.T) {
	entries := []Entry{
		{Event: "SessionStart", Value: json.RawMessage(`{"hooks":[{"type":"command","command":"crosshall"}]}`)},
		{Event: "Stop", Value: json.RawMessage(`{"hooks":[{"type":"command","command":"crosshall"}]}`)},
		{Value: json.RawMessage(`"Bash(crosshall who)"`)},
	}
	for _, raw := range [][]byte{nil, []byte(` {"hooks":{}, "permissions":{"deny":[]}} `)} {
		updated, added, err := addEntries(raw, entries)
		if err != nil || len(added) != len(entries) || !json.Valid(updated) {
			t.Fatalf("add to %s: %s, %v", raw, updated, err)
		}
		counts, err := entryMatches(updated, entries)
		if err != nil || !slices.Equal(counts, []int{1, 1, 1}) {
			t.Fatalf("installed counts %v in %s: %v", counts, updated, err)
		}
	}
}

func TestConfigEntriesDoNotClaimMixedGroups(t *testing.T) {
	entry := Entry{Event: "Stop", Value: json.RawMessage(`{"matcher":"x","hooks":[{"type":"command","command":"crosshall","timeout":1}]}`)}
	original := []byte(`{"hooks":{"Stop":[{"hooks":[{"type":"prompt"},{"timeout":1,"command":"crosshall","type":"command"}],"matcher":"x"}]}}`)
	updated, added, err := addEntries(original, []Entry{entry})
	if err != nil || len(added) != 0 || !bytes.Equal(updated, original) {
		t.Fatalf("setup claimed an existing handler: %s, %v", updated, err)
	}
	counts, err := entryMatches(original, []Entry{entry})
	if err != nil || !slices.Equal(counts, []int{1}) {
		t.Fatalf("installed counts %v: %v", counts, err)
	}
	updated, removed, left, err := removeEntries(original, []Entry{entry})
	if err != nil || len(removed) != 0 || len(left) != 1 || !bytes.Equal(updated, original) {
		t.Fatalf("uninstall changed a mixed group: %s, %v", updated, err)
	}
}

func TestConfigEntriesLeaveEditedAndAmbiguousHooks(t *testing.T) {
	value := `{"hooks":[{"type":"command","command":"crosshall","timeout":1}]}`
	edited := `{"hooks":[{"type":"command","command":"crosshall","timeout":2}]}`
	entry := Entry{Event: "Stop", Value: json.RawMessage(value)}
	for _, groups := range []string{edited, value + "," + value, value + "," + edited, ""} {
		raw := []byte(`{"hooks":{"Stop":[` + groups + `]}}`)
		updated, removed, left, err := removeEntries(raw, []Entry{entry})
		if err != nil || len(removed) != 0 || len(left) != 1 || !bytes.Equal(raw, updated) {
			t.Fatalf("uninstall changed contested groups %s: %s, %v", groups, updated, err)
		}
		if groups != "" {
			if _, _, err := addEntries(raw, []Entry{entry}); !errors.Is(err, errAmbiguousHook) {
				t.Fatalf("setup on contested groups returned %v", err)
			}
		}
	}
}

func TestConfigEntriesRejectInvalidJSONWithoutEchoingIt(t *testing.T) {
	entry := Entry{Event: "Stop", Value: json.RawMessage(`{"hooks":[{"type":"command","command":"crosshall"}]}`)}
	for _, input := range []string{
		`{"secret":"private","secret":false}`,
		`{"secret":{"a":1,"\u0061":2}}`,
		`{"secret":"private"} {}`,
		`{"hooks":{"Stop":[{"hooks":["private"]}]}}`,
		`{"hooks":{"Stop":false}}`,
		`{"hooks":null}`,
		`[]`,
	} {
		if _, _, err := addEntries([]byte(input), []Entry{entry}); !errors.Is(err, errInvalidConfig) {
			t.Fatalf("invalid configuration returned %v", err)
		}
	}
}
