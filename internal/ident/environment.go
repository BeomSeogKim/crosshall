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

// HostVariable names the process crosshall run replaced itself with, as
// "<pid>:<start identity>", so that a route member's commands can name the
// process whose end ends the session.
const HostVariable = "CROSSHALL_HOST"

// Identity names a conversation or an explicitly scoped launch. Version is
// diagnostic metadata and never participates in the caller key.
type Identity struct {
	Vendor  string
	Key     string
	Version string
}

//go:generate go test -run ^TestIdentityIndexMatchesManifests$ . -update-index

// identityRow is one adapter's identity declaration in its hook manifest,
// compiled into identityIndex (identity_index.go) so that resolving an
// identity, which every hook does, decodes no manifest.
type identityRow struct {
	adapter    string
	sessionEnv string
	evidence   string
	verifiedOn []string
	// markers are variables the vendor's agents set besides the session
	// variable: an agent context crosshall cannot identify.
	markers []string
	cliName string
	version versionSource
}

// versionSource reads the vendor's version from env: after prefix, up to
// until, '-' read as '.' when dashesToDots. An indirect source is another
// component's build, recorded as "<indirect>:<prefix><version>" and never
// taken for the CLI's own version; indirectName names that component.
type versionSource struct {
	env          string
	prefix       string
	until        string
	dashesToDots bool
	indirect     string
	indirectName string
}

// genericAgentMarkers are set by agents of any vendor.
var genericAgentMarkers = []string{"AI_AGENT"}

func indexRow(vendor string) (identityRow, bool) {
	for _, row := range identityIndex {
		if row.adapter == vendor {
			return row, true
		}
	}
	return identityRow{}, false
}

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
	for _, row := range identityIndex {
		if value, ok := lookup(row.sessionEnv); ok {
			found = append(found, row.sessionEnv)
			caller = Identity{Vendor: row.adapter, Key: value}
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
	for _, row := range identityIndex {
		for _, name := range append([]string{row.sessionEnv}, row.markers...) {
			if _, present := lookup(name); present {
				return false
			}
		}
	}
	for _, marker := range genericAgentMarkers {
		if _, present := lookup(marker); present {
			return false
		}
	}
	return true
}

// Evidence reports the source of the identity variable and whether it was
// checked with this vendor CLI version.
func (id Identity) Evidence() (source string, verified bool) {
	row, ok := indexRow(id.Vendor)
	if !ok {
		return "", false
	}
	return row.evidence, slices.Contains(row.verifiedOn, id.Version)
}

// IndirectVersion reports that the vendor names its version only through
// another component's build, so no session can show its CLI version.
func (id Identity) IndirectVersion() bool {
	row, ok := indexRow(id.Vendor)
	return ok && row.version.indirect != ""
}

// UnknownCLI distinguishes an indirect build, or none, from a CLI version.
func (id Identity) UnknownCLI() bool {
	row, _ := indexRow(id.Vendor)
	return id.IndirectVersion() &&
		(id.Version == "" || id.Version == "unknown" || strings.HasPrefix(id.Version, row.version.indirect+":"))
}

// CLIName names the vendor's CLI in user-facing text.
func (id Identity) CLIName() string {
	row, _ := indexRow(id.Vendor)
	return row.cliName
}

// IndirectVersionVendors lists the vendors whose sessions record only an
// indirect build (IndirectVersion).
func IndirectVersionVendors() []string {
	var vendors []string
	for _, row := range identityIndex {
		if row.version.indirect != "" {
			vendors = append(vendors, row.adapter)
		}
	}
	return vendors
}

// Warning is shared by member views and owner diagnostics.
func (id Identity) Warning() string {
	source, verified := id.Evidence()
	if source == "" || verified {
		return ""
	}
	version := id.Version
	if id.UnknownCLI() {
		row, _ := indexRow(id.Vendor)
		msg := "identity variable unverified on " + id.Vendor + ": CLI version unknown"
		if number, ok := strings.CutPrefix(version, row.version.indirect+":"+row.version.prefix); ok && validVersion(number) {
			return fmt.Sprintf("%s (%s %s%s)", msg, row.version.indirectName, row.version.prefix, number)
		}
		return msg
	}
	if version == "" {
		version = "unknown"
	}
	return fmt.Sprintf("identity variable unverified on %s %s", id.Vendor, version)
}

func runtimeVersion(vendor string, lookup func(string) (string, bool)) string {
	row, ok := indexRow(vendor)
	if !ok || row.version.env == "" {
		return "unknown"
	}
	source := row.version
	version, _ := lookup(source.env)
	if source.prefix != "" {
		if version, ok = strings.CutPrefix(version, source.prefix); !ok {
			return "unknown"
		}
	}
	if source.until != "" {
		version, _, _ = strings.Cut(version, source.until)
	}
	if source.dashesToDots {
		version = strings.ReplaceAll(version, "-", ".")
	}
	if !validVersion(version) {
		return "unknown"
	}
	if source.indirect != "" {
		return source.indirect + ":" + source.prefix + version
	}
	return version
}

func validVersion(version string) bool {
	return version != "" && version[0] >= '0' && version[0] <= '9' &&
		strings.IndexFunc(version, func(r rune) bool {
			return !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' ||
				r >= 'A' && r <= 'Z' || r == '.' || r == '-' || r == '+')
		}) < 0
}
