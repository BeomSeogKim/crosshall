// Package ident resolves local session identity and validates dispatcher ingress.
package ident

import (
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/BeomSeogKim/crosshall/internal/core"
)

const RouteVariable = "CROSSHALL_ROUTE"

// Identity names a conversation or an explicitly scoped launch. Version is
// diagnostic metadata and never participates in the caller key.
type Identity struct {
	Vendor  string
	Key     string
	Version string
}

type vendorVariable struct {
	name       string
	vendor     string
	evidence   string
	verifiedOn []string
}

var vendorVariables = []vendorVariable{
	{"CLAUDE_CODE_SESSION_ID", "claude", "documented", []string{"2.1.327"}},
	{"CODEX_THREAD_ID", "codex", "observed", []string{"0.170.0"}},
	{"ANTIGRAVITY_CONVERSATION_ID", "agy", "observed", []string{"1.5.11"}},
}

var otherAgentMarkers = []string{"CLAUDECODE", "AI_AGENT", "CODEX_SESSION_ID", "ANTIGRAVITY_AGENT"}

// EnvironmentError describes a local resolution failure. It includes variable
// names when useful but never the values of identity variables.
type EnvironmentError struct {
	Code    core.Code
	Message string
}

func (e *EnvironmentError) Error() string { return e.Message }

// ResolveEnvironment selects the vendor identity when exactly one vendor
// variable is present. A conflicting or malformed identity is an error.
func ResolveEnvironment(lookup func(string) (string, bool)) (Identity, error) {
	var found []string
	var caller Identity
	for _, row := range vendorVariables {
		if value, ok := lookup(row.name); ok {
			found = append(found, row.name)
			caller = Identity{Vendor: row.vendor, Key: value}
		}
	}
	switch len(found) {
	case 0:
		route, ok := lookup(RouteVariable)
		if !ok {
			return Identity{}, &EnvironmentError{core.CodeNotEnrolled, "no session identity"}
		}
		if !validRoute(route) {
			return Identity{}, &EnvironmentError{core.CodeNotEnrolled, "invalid CROSSHALL_ROUTE"}
		}
		return Identity{Vendor: "route", Key: strings.ToLower(route)}, nil
	case 1:
		if caller.Key == "" || strings.IndexFunc(caller.Key, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
			return Identity{}, &EnvironmentError{core.CodeNotEnrolled, "invalid session identity in " + found[0]}
		}
		caller.Version = runtimeVersion(caller.Vendor, lookup)
		return caller, nil
	default:
		return Identity{}, &EnvironmentError{core.CodeBindingAmbiguous, "ambiguous session variables: " + strings.Join(found, ", ")}
	}
}

func validRoute(s string) bool {
	if len(s) != 32 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// AllowOwner checks the explicit owner prefix and the presence of agent
// context before the client opens the private owner spool.
func AllowOwner(lookup func(string) (string, bool), ownerPrefix bool) bool {
	if !ownerPrefix {
		return false
	}
	if _, present := lookup(RouteVariable); present {
		return false
	}
	for _, row := range vendorVariables {
		if _, present := lookup(row.name); present {
			return false
		}
	}
	for _, marker := range otherAgentMarkers {
		if _, present := lookup(marker); present {
			return false
		}
	}
	return true
}

// Evidence reports the source of the identity variable and whether it was
// checked with this vendor CLI version.
func (id Identity) Evidence() (source string, verified bool) {
	for _, row := range vendorVariables {
		if id.Vendor == row.vendor {
			return row.evidence, slices.Contains(row.verifiedOn, id.Version)
		}
	}
	return "", false
}

// UnknownAntigravityCLI distinguishes language-server evidence from a CLI version.
func (id Identity) UnknownAntigravityCLI() bool {
	return id.Vendor == "agy" && (id.Version == "" || id.Version == "unknown" || strings.HasPrefix(id.Version, "ls:"))
}

// Warning is shared by member views and owner diagnostics.
func (id Identity) Warning() string {
	source, verified := id.Evidence()
	if source == "" || verified {
		return ""
	}
	version := id.Version
	if id.UnknownAntigravityCLI() {
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

func runtimeVersion(vendor string, lookup func(string) (string, bool)) string {
	var version string
	var prefix string
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
		// The language server and the CLI can have different build numbers.
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

func validVersion(version string) bool {
	return version != "" && version[0] >= '0' && version[0] <= '9' &&
		strings.IndexFunc(version, func(r rune) bool {
			return !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' ||
				r >= 'A' && r <= 'Z' || r == '.' || r == '-' || r == '+')
		}) < 0
}
