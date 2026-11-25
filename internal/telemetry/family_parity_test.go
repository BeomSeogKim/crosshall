package telemetry

import (
	"strings"
	"testing"
)

// legacyFamily is family as written before the indirect prefix came from
// ident's index (#327); the registry stays the final allow-list.
func legacyFamily(adapter, version string) string {
	if version == "" || version == "unverified" || version == "unknown" {
		return "unknown"
	}
	prefix := ""
	if adapter == "agy" {
		rest, ok := strings.CutPrefix(version, "ls:cli-")
		if !ok {
			return "other"
		}
		prefix, version = "ls_", rest
	}
	if m := familyPattern.FindStringSubmatch(version); m != nil {
		if f := prefix + m[1] + "_" + m[2]; registry.has("snap.live_launch." + adapter + "." + f) {
			return f
		}
	}
	return "other"
}

func TestLaunchFamiliesKeepTheirKeys(t *testing.T) {
	versions := []string{"", "unverified", "unknown", "2.1.331", "2.1", "2.2.0", "0.170.0", "0.171.3", "0.172.0",
		"ls:cli-1.5.10", "ls:cli-1.6.0", "ls:cli-", "ls:cli-x", "ls:1.5.10", "cli-1.5.10", "1.5.10", "ls:cli-1.5.10.2.3",
		"v2.1.0", "2.1.0-rc1", "99999.1"}
	registered := map[string]string{"claude": "2.1.331", "codex": "0.170.0", "agy": "ls:cli-1.5.10"}
	for _, adapter := range []string{"claude", "codex", "agy", "zed", ""} {
		for _, version := range versions {
			if got, want := family(adapter, version), legacyFamily(adapter, version); got != want {
				t.Errorf("family(%q, %q) = %q, was %q", adapter, version, got, want)
			}
		}
		if v, ok := registered[adapter]; ok && family(adapter, v) == "other" {
			t.Errorf("%s %s: registered family not found", adapter, v)
		}
	}
	if family("zed", "1.2.3") != "other" || family("zed", "") != "unknown" {
		t.Error("an unregistered adapter got a family")
	}
}
