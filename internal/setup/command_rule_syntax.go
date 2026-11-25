package setup

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/BeomSeogKim/crosshall/internal/hook"
)

func memberCommandRules(adapter, executable string) ([]Entry, []byte, error) {
	if !filepath.IsAbs(executable) || !utf8.ValidString(executable) || strings.ContainsRune(executable, 0) {
		return nil, nil, errors.New("member command permissions require a valid absolute executable path")
	}
	return installerFor(adapter).memberRules(adapter, executable)
}

// codexMemberRules is a Codex rules file that allows the routine member
// verbs of the executable, which must be named crosshall.
func codexMemberRules(executable string) ([]byte, error) {
	if filepath.Base(executable) != "crosshall" {
		return nil, errors.New("Codex member command permissions require an executable named crosshall")
	}
	var rules strings.Builder
	// An absolute prefix would take precedence over existing bare rules.
	fmt.Fprintf(&rules, "host_executable(name=\"crosshall\", paths=[%q])\n", executable)
	for _, verb := range hook.RoutineMemberVerbs() {
		fmt.Fprintf(&rules, "prefix_rule(pattern=[\"crosshall\", %q], decision=\"allow\")\n", verb)
	}
	return []byte(rules.String()), nil
}

// jsonMemberRules lists one rule per routine member verb, in ruleFormat, for
// the executable's shell path and for a bare crosshall.
func jsonMemberRules(adapter, executable, ruleFormat string) ([]Entry, error) {
	if strings.ContainsAny(executable, "\r\n") {
		return nil, fmt.Errorf("%s command permissions cannot represent an executable path containing a line break", adapter)
	}
	verbs := hook.RoutineMemberVerbs()
	shellPath := commandRuleShellPath(executable)
	entries := make([]Entry, 0, 2*len(verbs))
	for _, program := range []string{shellPath, "crosshall"} {
		for _, verb := range verbs {
			raw, _ := json.Marshal(fmt.Sprintf(ruleFormat, program, verb))
			entries = append(entries, Entry{Value: raw})
		}
	}
	return entries, nil
}

func commandRuleShellPath(executable string) string {
	needsQuoting := strings.IndexFunc(executable, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && !strings.ContainsRune("/._-:@%+=,", r)
	}) >= 0
	if needsQuoting {
		return shellQuote(executable)
	}
	return executable
}
