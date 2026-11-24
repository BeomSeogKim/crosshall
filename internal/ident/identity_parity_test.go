package ident

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/core"
)

// The identity rules as they were written in ident before the manifests
// declared them (#324). The index must keep every outcome.
func legacyRuntimeVersion(vendor string, lookup func(string) (string, bool)) string {
	var version, prefix string
	switch vendor {
	case "codex":
		version, _ = lookup("CODEX_VERSION")
	case "claude":
		agent, _ := lookup("AI_AGENT")
		if encoded, ok := strings.CutPrefix(agent, "claude-code_"); ok {
			encoded, _, _ = strings.Cut(encoded, "_")
			version = strings.ReplaceAll(encoded, "-", ".")
		}
	case "agy":
		build, _ := lookup("ANTIGRAVITY_LS_VERSION")
		if number, ok := strings.CutPrefix(build, "cli-"); ok {
			version, prefix = number, "ls:cli-"
		}
	}
	if !validVersion(version) {
		return "unknown"
	}
	return prefix + version
}

func legacyUnknownCLI(id Identity) bool {
	return id.Vendor == "agy" && (id.Version == "" || id.Version == "unknown" || strings.HasPrefix(id.Version, "ls:"))
}

func legacyWarning(id Identity) string {
	verified := map[string][]string{"claude": {"2.1.327"}, "codex": {"0.170.0"}, "agy": {"1.5.11"}}
	known, ok := verified[id.Vendor]
	if !ok || slices.Contains(known, id.Version) {
		return ""
	}
	version := id.Version
	if legacyUnknownCLI(id) {
		msg := "identity variable unverified on agy: CLI version unknown"
		if number, ok := strings.CutPrefix(version, "ls:cli-"); ok && validVersion(number) {
			return fmt.Sprintf("%s (language server cli-%s)", msg, number)
		}
		return msg
	}
	if version == "" {
		version = "unknown"
	}
	return fmt.Sprintf("identity variable unverified on %s %s", id.Vendor, version)
}

func TestIdentityVersionsKeepTheirNormalization(t *testing.T) {
	values := map[string][]string{
		"AI_AGENT":               {"claude-code_2-1-327_harness", "claude-code_2-1-327_agent", "claude-code_2-1-3", "claude-code_", "claude-code__agent", "claude-code_x-1_agent", "claude-code_2-1-3+b_agent", "codex_1-2_agent", "2.1.327"},
		"CODEX_VERSION":          {"0.170.0", "1.2.3-rc.1+b", "v0.1", "0.1 beta", "-1"},
		"ANTIGRAVITY_LS_VERSION": {"cli-1.5.10", "cli-1.5.11+abc", "1.5.10", "cli-", "cli-x", "ls_1_5", "CLI-1.5.10"},
	}
	for _, vendor := range []string{"claude", "codex", "agy", "route", "other"} {
		for name, list := range values {
			for _, value := range append(list, "", "\x00") {
				for _, present := range []bool{true, false} {
					lookup := func(k string) (string, bool) {
						if k == name && present {
							return value, true
						}
						return "", false
					}
					got, want := runtimeVersion(vendor, lookup), legacyRuntimeVersion(vendor, lookup)
					if got != want {
						t.Errorf("%s with %s=%q (present %t): version %q, was %q", vendor, name, value, present, got, want)
					}
					id := Identity{Vendor: vendor, Version: got}
					if id.UnknownCLI() != legacyUnknownCLI(id) || id.Warning() != legacyWarning(id) {
						t.Errorf("%+v: unknown CLI %t warning %q, was %t %q", id, id.UnknownCLI(), id.Warning(), legacyUnknownCLI(id), legacyWarning(id))
					}
				}
			}
		}
	}
	// Versions read back from the store need not come from runtimeVersion.
	for _, vendor := range []string{"claude", "codex", "agy", "route"} {
		for _, version := range []string{"", "unknown", "ls:", "ls:cli-", "ls:cli-1.5.10", "ls:junk", "1.5.11", "2.1.327", "0.170.0", "9.9.9"} {
			id := Identity{Vendor: vendor, Version: version}
			if id.UnknownCLI() != legacyUnknownCLI(id) || id.Warning() != legacyWarning(id) {
				t.Errorf("%+v: unknown CLI %t warning %q, was %t %q", id, id.UnknownCLI(), id.Warning(), legacyUnknownCLI(id), legacyWarning(id))
			}
		}
	}
	if got := IndirectVersionVendors(); !slices.Equal(got, []string{"agy"}) {
		t.Errorf("indirect version vendors: %v", got)
	}
}

func TestOwnerPrefixRefusesEveryAgentMarker(t *testing.T) {
	for _, marker := range []string{"CLAUDE_CODE_SESSION_ID", "CODEX_THREAD_ID", "ANTIGRAVITY_CONVERSATION_ID", "CLAUDECODE", "AI_AGENT", "CODEX_SESSION_ID", "ANTIGRAVITY_AGENT", RouteVariable} {
		lookup := func(k string) (string, bool) { return "", k == marker }
		if AllowOwner(lookup, true) {
			t.Errorf("owner allowed with only %s set", marker)
		}
	}
	none := func(string) (string, bool) { return "", false }
	if !AllowOwner(none, true) || AllowOwner(none, false) {
		t.Error("owner prefix without agent context")
	}
}

func TestSimultaneousSessionVariablesAreAmbiguous(t *testing.T) {
	all := []string{"CLAUDE_CODE_SESSION_ID", "CODEX_THREAD_ID", "ANTIGRAVITY_CONVERSATION_ID"}
	for _, set := range [][]string{all, all[:2], all[1:], {all[0], all[2]}} {
		lookup := func(k string) (string, bool) { return "s", slices.Contains(set, k) }
		_, err := ResolveEnvironment(lookup)
		var envErr *EnvironmentError
		if !errors.As(err, &envErr) || envErr.Code != core.CodeBindingAmbiguous ||
			envErr.Message != "ambiguous session variables: "+strings.Join(set, ", ") {
			t.Errorf("%v: %v", set, err)
		}
	}
}
