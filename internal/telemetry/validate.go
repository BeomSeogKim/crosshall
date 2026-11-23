package telemetry

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
)

// MaxBody is the largest body the receiver reads.
const MaxBody = 32 << 10

// ErrUnsupported marks a report whose v or registry number this code does
// not know; the receiver answers it with 422 instead of 400.
var ErrUnsupported = errors.New("unsupported report version or registry")

// Validate applies the receiver's rules to a report body.
func Validate(body []byte, now time.Time) error {
	_, err := Parse(body, now)
	return err
}

// Parse validates a report body and returns the report, whose Marshal is
// the canonical form the receiver stores. Every object must have exactly
// its registered members, none twice; every string must be a registry
// member; every count a registry key with a value on the ladder. now decides
// which days are acceptable: the 8 days before its UTC date.
func Parse(body []byte, now time.Time) (Report, error) {
	var r Report
	if len(body) > MaxBody {
		return r, fmt.Errorf("body is %d bytes, over %d", len(body), MaxBody)
	}
	top, err := object(body, "report")
	if err != nil {
		return r, err
	}
	if _, ok := top["registry"]; ok {
		if r.Registry, err = integer(top["registry"], "registry"); err != nil {
			return r, err
		}
		if r.Registry != registry.Registry {
			return r, fmt.Errorf("registry %d: %w", r.Registry, ErrUnsupported)
		}
	}
	if _, ok := top["v"]; ok {
		if r.V, err = integer(top["v"], "v"); err != nil {
			return r, err
		}
		if r.V != 1 {
			return r, fmt.Errorf("v %d: %w", r.V, ErrUnsupported)
		}
	}
	members := []string{"v", "registry", "batch", "day", "app", "sources", "counts"}
	if err := exactly(top, "", members, []string{"switches"}); err != nil {
		return r, err
	}
	if r.Batch, err = str(top["batch"], "batch"); err != nil {
		return r, err
	} else if !batchPattern.MatchString(r.Batch) {
		return r, fmt.Errorf("batch: %q", r.Batch)
	}
	if r.Day, err = validDay(top["day"], now); err != nil {
		return r, err
	}
	if r.App, err = validApp(top["app"]); err != nil {
		return r, err
	}
	if r.Sources, err = validSources(top["sources"]); err != nil {
		return r, err
	}
	_, hasSwitches := top["switches"]
	if hasSwitches != (r.Sources.Snapshot == "taken") {
		return r, errors.New("switches: present exactly when sources.snapshot is taken")
	}
	if hasSwitches {
		if r.Switches, err = validSwitches(top["switches"]); err != nil {
			return r, err
		}
	}
	r.Counts, err = validCounts(top["counts"], r.Sources)
	return r, err
}

func validDay(raw json.RawMessage, now time.Time) (string, error) {
	s, err := str(raw, "day")
	if err != nil {
		return "", err
	}
	day, err := time.Parse("2006-01-02", s)
	if err != nil {
		return "", fmt.Errorf("day: %q is not a date", s)
	}
	today := now.UTC().Truncate(24 * time.Hour)
	if !day.Before(today) || day.Before(today.AddDate(0, 0, -8)) {
		return "", fmt.Errorf("day: %s is not one of the 8 days before %s", s, today.Format("2006-01-02"))
	}
	return s, nil
}

func validApp(raw json.RawMessage) (App, error) {
	var app App
	obj, err := object(raw, "app")
	if err != nil {
		return app, err
	}
	if err := exactly(obj, "app.", []string{"version", "platform"}, nil); err != nil {
		return app, err
	}
	if app.Version, err = str(obj["version"], "app.version"); err != nil {
		return app, err
	} else if !releasePattern.MatchString(app.Version) {
		return app, fmt.Errorf("app.version: %q is not a release tag", app.Version)
	}
	app.Platform, err = member(obj["platform"], "app.platform", registry.Platforms)
	return app, err
}

func validSources(raw json.RawMessage) (Sources, error) {
	var s Sources
	obj, err := object(raw, "sources")
	if err != nil {
		return s, err
	}
	if err := exactly(obj, "sources.", []string{"hooks", "snapshot"}, nil); err != nil {
		return s, err
	}
	if s.Hooks, err = member(obj["hooks"], "sources.hooks", registry.Sources["hooks"]); err != nil {
		return s, err
	}
	s.Snapshot, err = member(obj["snapshot"], "sources.snapshot", registry.Sources["snapshot"])
	return s, err
}

func validSwitches(raw json.RawMessage) (*Switches, error) {
	sw := &Switches{}
	obj, err := object(raw, "switches")
	if err != nil {
		return nil, err
	}
	if err := exactly(obj, "switches.", []string{"publish_hall", "turn_end_claude"}, nil); err != nil {
		return nil, err
	}
	if sw.PublishHall, err = member(obj["publish_hall"], "switches.publish_hall", registry.Switches["publish_hall"]); err != nil {
		return nil, err
	}
	sw.TurnEndClaude, err = member(obj["turn_end_claude"], "switches.turn_end_claude", registry.Switches["turn_end_claude"])
	return sw, err
}

func validCounts(raw json.RawMessage, sources Sources) (map[string]int64, error) {
	obj, err := object(raw, "counts")
	if err != nil {
		return nil, err
	}
	if len(obj) == 0 || len(obj) > len(registry.Keys) {
		return nil, fmt.Errorf("counts: %d members, want 1 to %d", len(obj), len(registry.Keys))
	}
	counts := make(map[string]int64, len(obj))
	for key, value := range obj {
		if !registry.has(key) {
			return nil, fmt.Errorf("counts: %q is not a registry key", key)
		}
		if sources.Hooks == "missing" && (strings.HasPrefix(key, "hook.") || strings.HasPrefix(key, "hook_ms.")) {
			return nil, fmt.Errorf("counts: %q while sources.hooks is missing", key)
		}
		if sources.Snapshot == "none" && strings.HasPrefix(key, "snap.") {
			return nil, fmt.Errorf("counts: %q while sources.snapshot is none", key)
		}
		n, err := integer(value, key)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(registry.Values, int64(n)) {
			return nil, fmt.Errorf("counts: %s is %d, not a ladder value", key, n)
		}
		counts[key] = int64(n)
	}
	return counts, nil
}

// object decodes one JSON object, refusing a member name that appears twice
// and anything after the object.
func object(raw json.RawMessage, name string) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, fmt.Errorf("%s: not an object", name)
	}
	out := make(map[string]json.RawMessage)
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		key := tok.(string)
		if _, dup := out[key]; dup {
			return nil, fmt.Errorf("%s: member %q appears twice", name, key)
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		out[key] = value
	}
	if _, err := dec.Token(); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("%s: data after the object", name)
	}
	return out, nil
}

func exactly(obj map[string]json.RawMessage, prefix string, required, optional []string) error {
	for _, key := range required {
		if _, ok := obj[key]; !ok {
			return fmt.Errorf("%s%s: missing", prefix, key)
		}
	}
	for key := range obj {
		if !slices.Contains(required, key) && !slices.Contains(optional, key) {
			return fmt.Errorf("%s%s: unknown member", prefix, key)
		}
	}
	return nil
}

func str(raw json.RawMessage, name string) (string, error) {
	var s string
	if !bytes.HasPrefix(raw, []byte(`"`)) || json.Unmarshal(raw, &s) != nil {
		return "", fmt.Errorf("%s: not a string", name)
	}
	return s, nil
}

func member(raw json.RawMessage, name string, allowed []string) (string, error) {
	s, err := str(raw, name)
	if err == nil && !slices.Contains(allowed, s) {
		err = fmt.Errorf("%s: %q is not allowed", name, s)
	}
	return s, err
}

// integer accepts only a plain JSON integer: no sign, fraction, exponent or
// leading zero.
func integer(raw json.RawMessage, name string) (int, error) {
	s := string(raw)
	if s == "" || len(s) > 9 || (len(s) > 1 && s[0] == '0') || strings.Trim(s, "0123456789") != "" {
		return 0, fmt.Errorf("%s: %s is not a plain integer", name, s)
	}
	var n int
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n, nil
}
