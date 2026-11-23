package telemetry

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// The shared fixtures are dated 2026-11-24 and 2026-11-25; the receiver
// would see them on this day.
var receiverNow = time.Date(2026, 11, 26, 3, 0, 0, 0, time.UTC)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestValidFixturesHaveTheirCanonicalForm(t *testing.T) {
	for _, name := range []string{"valid-full", "valid-no-snapshot", "valid-hooks-missing"} {
		r, err := Parse(fixture(t, name+".json"), receiverNow)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got, err := r.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		if want := bytes.TrimSuffix(fixture(t, name+".canonical.json"), []byte("\n")); !bytes.Equal(got, want) {
			t.Errorf("%s: canonical form\n got %s\nwant %s", name, got, want)
		}
	}
}

func TestRetriesCarryTheFirstBatchToken(t *testing.T) {
	first, err := Parse(fixture(t, "valid-full.json"), receiverNow)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"retry-same.json", "retry-different-bytes.json"} {
		r, err := Parse(fixture(t, name), receiverNow)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if r.Batch != first.Batch {
			t.Errorf("%s: batch %s, want the first report's %s", name, r.Batch, first.Batch)
		}
	}
}

func TestBadFixturesAreRefused(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("testdata", "bad-*.json"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no bad fixtures: %v", err)
	}
	for _, path := range paths {
		err := Validate(fixture(t, filepath.Base(path)), receiverNow)
		if err == nil {
			t.Errorf("%s: accepted", path)
			continue
		}
		// bad-registry.json is answered 422; every other one 400.
		if unsupported := errors.Is(err, ErrUnsupported); unsupported != strings.HasSuffix(path, "bad-registry.json") {
			t.Errorf("%s: %v (unsupported %v)", path, err, unsupported)
		}
	}
}

// mutate decodes the canonical full fixture, applies f, and re-encodes it.
func mutate(t *testing.T, f func(map[string]any)) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(fixture(t, "valid-full.canonical.json"), &doc); err != nil {
		t.Fatal(err)
	}
	f(doc)
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestValidateRefusesEveryOtherShape(t *testing.T) {
	full := bytes.TrimSuffix(fixture(t, "valid-full.canonical.json"), []byte("\n"))
	sub := func(old, new string) []byte { return bytes.Replace(full, []byte(old), []byte(new), 1) }
	app := func(doc map[string]any) map[string]any { return doc["app"].(map[string]any) }
	for name, body := range map[string][]byte{
		"extra top member": mutate(t, func(d map[string]any) { d["install"] = "9f1c2e4a7b3d5f60812a4c6e8b0d2f41" }),
		"missing member":   mutate(t, func(d map[string]any) { delete(d, "sources") }),
		"switches no snap": mutate(t, func(d map[string]any) { d["sources"] = map[string]any{"hooks": "ok", "snapshot": "none"} }),
		"snap no switches": mutate(t, func(d map[string]any) { delete(d, "switches") }),
		"switch value":     mutate(t, func(d map[string]any) { d["switches"].(map[string]any)["publish_hall"] = "default" }),
		"switch extra":     mutate(t, func(d map[string]any) { d["switches"].(map[string]any)["auto_join"] = "on" }),
		"platform":         mutate(t, func(d map[string]any) { app(d)["platform"] = "windows-amd64" }),
		"dev version":      mutate(t, func(d map[string]any) { app(d)["version"] = "dev" }),
		"version suffix":   mutate(t, func(d map[string]any) { app(d)["version"] = "v0.2.0-rc.2+zqxmarker" }),
		"long version":     mutate(t, func(d map[string]any) { app(d)["version"] = "v100.2.0" }),
		"batch uppercase":  mutate(t, func(d map[string]any) { d["batch"] = strings.ToUpper(d["batch"].(string)) }),
		"empty counts":     mutate(t, func(d map[string]any) { d["counts"] = map[string]any{} }),
		"zero count":       mutate(t, func(d map[string]any) { d["counts"].(map[string]any)["msg.note"] = 0 }),
		"over the ladder":  mutate(t, func(d map[string]any) { d["counts"].(map[string]any)["msg.note"] = 2000 }),
		"null counts":      mutate(t, func(d map[string]any) { d["counts"] = nil }),
		"day today":        mutate(t, func(d map[string]any) { d["day"] = "2026-11-26" }),
		"day too old":      mutate(t, func(d map[string]any) { d["day"] = "2026-11-17" }),
		"not a date":       mutate(t, func(d map[string]any) { d["day"] = "2026-02-30" }),
		"v 2":              sub(`"v":1`, `"v":2`),
		"v string":         sub(`"v":1`, `"v":"1"`),
		"fractional count": sub(`"msg.ask":10`, `"msg.ask":10.0`),
		"exponent count":   sub(`"msg.ask":10`, `"msg.ask":1e1`),
		"negative count":   sub(`"msg.ask":10`, `"msg.ask":-10`),
		"quoted count":     sub(`"msg.ask":10`, `"msg.ask":"10"`),
		"duplicate top":    sub(`"day":"2026-11-25"`, `"day":"2026-11-25","day":"2026-11-24"`),
		"duplicate nested": sub(`"hooks":"ok"`, `"hooks":"ok","hooks":"missing"`),
		"trailing data":    append(slices.Clone(full), []byte("{}")...),
		"not an object":    []byte(`[1]`),
		"too large":        append(slices.Clone(full), bytes.Repeat([]byte(" "), MaxBody)...),
	} {
		if err := Validate(body, receiverNow); err == nil {
			t.Errorf("%s: accepted %s", name, body)
		}
	}
	if err := Validate(full, receiverNow); err != nil {
		t.Fatalf("the unmutated fixture: %v", err)
	}
}

// TestRegistryMatchesTheBuilder checks that the keys the builder can compose
// from its dimensions are exactly the registry's 287, that every one passes
// validation with every ladder value, and that the ladder is what Round
// produces.
func TestRegistryMatchesTheBuilder(t *testing.T) {
	var composed []string
	add := func(prefix string, values ...string) {
		for _, v := range values {
			composed = append(composed, prefix+v)
		}
	}
	for _, a := range adapters {
		for _, c := range classes {
			add("hook."+a+"."+c+".", hookOutcomes...)
			add("hook_ms."+a+"."+c+".", hookMSBuckets...)
		}
		add("turn_end.claim.", a)
		add("snap.build_stopped.", a)
		add("snap.live_launch."+a+".", append(slices.Clone(families[a]), "other", "unknown")...)
	}
	add("msg.", messageKinds...)
	add("ask_pair.", settlements...)
	add("pickup_lag.", pickupBuckets...)
	add("notice.", "reserved", "retired_unobserved", "unobserved_10m", "late_observed")
	add("notice.resolved.", resolutions...)
	add("notice.observed.", observations...)
	add("receipt.rejected.", rejections...)
	for _, s := range suspensions {
		add("suspend."+s+".", "timed", "lasting")
	}
	add("send.wake.", "notice", "wait", "none")
	add("join.", "hook", "cli", "rejoined")
	add("", "budget.exhausted", "week.active_days", "week.unobserved_10m", "snap.unread_ask_pairs")
	add("snap.reach.", reachStates...)
	slices.Sort(composed)
	if !slices.Equal(composed, Keys()) || len(composed) != 287 {
		t.Fatalf("builder dimensions make %d keys, registry has %d:\n%v\n%v", len(composed), len(Keys()), composed, Keys())
	}
	if !slices.IsSorted(registry.Keys) {
		t.Error("registry.json keys are not in ascending byte order")
	}
	var doc map[string]any
	if err := json.Unmarshal(fixture(t, "valid-full.canonical.json"), &doc); err != nil {
		t.Fatal(err)
	}
	for _, key := range Keys() {
		for _, v := range registry.Values {
			doc["counts"] = map[string]int64{key: v}
			raw, _ := json.Marshal(doc)
			if err := Validate(raw, receiverNow); err != nil {
				t.Fatalf("%s=%d: %v", key, v, err)
			}
		}
	}
	for n, want := range map[int64]int64{-1: 0, 0: 0, 1: 1, 3: 2, 4: 2, 5: 5, 19: 10, 999: 500, 1000: 1000, 1 << 40: 1000} {
		if got := Round(n); got != want {
			t.Errorf("Round(%d) = %d, want %d", n, got, want)
		}
	}
}

func TestRegisteredHookEventsMatchTheManifests(t *testing.T) {
	for _, adapter := range adapters {
		raw, err := os.ReadFile(filepath.Join("..", "hook", "manifests", adapter+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var m struct {
			Events map[string]json.RawMessage `json:"events"`
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		// Every event class but "other" names a manifest event; a manifest
		// event without a class is counted as "other".
		for event := range maps.Keys(hookClasses) {
			if _, ok := m.Events[event]; !ok && (adapter != "agy" || event == "SessionStart") {
				t.Errorf("%s manifest has no %s event", adapter, event)
			}
		}
	}
}

func TestVersionMapping(t *testing.T) {
	for in, want := range map[string]string{"v0.2.0-rc.2": "v0.2.0-rc.2", "v1.0.0": "v1.0.0", "dev": "dev",
		"v0.2.0-rc.2+zqxmarker": "dev", "v0.2.0-beta": "dev", "": "dev", "v100.0.0": "dev"} {
		if got := AppVersion(in); got != want {
			t.Errorf("AppVersion(%q) = %q, want %q", in, got, want)
		}
	}
	// The strings identity resolution records (runtimeVersion) and a Claude
	// launch profile carries; "unverified" is an unverified launch's.
	for _, c := range []struct{ adapter, version, want string }{
		{"claude", "2.1.330", "2_1"}, {"claude", "2.2.0", "other"}, {"claude", "unverified", "unknown"},
		{"claude", "unknown", "unknown"}, {"claude", "", "unknown"}, {"claude", "2.1.330-zqxmarker", "other"},
		{"codex", "0.170.0", "0_170"}, {"codex", "0.171.0", "0_171"}, {"codex", "0.172.0", "other"}, {"codex", "unknown", "unknown"},
		{"agy", "ls:cli-1.5.13", "ls_1_5"}, {"agy", "ls:cli-1.6.0", "other"}, {"agy", "unknown", "unknown"},
		{"agy", "1.5.13", "other"}, {"agy", "ls:cli-zqxmarker", "other"},
	} {
		if got := family(c.adapter, c.version); got != c.want {
			t.Errorf("family(%s, %q) = %q, want %q", c.adapter, c.version, got, c.want)
		}
	}
}
