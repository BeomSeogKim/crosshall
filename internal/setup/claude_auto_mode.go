package setup

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/BeomSeogKim/crosshall/internal/hook"
)

const claudeDefaultRules = `"$defaults"`

func claudeAutoModeEntries(executable string, includeDefaults bool) []Entry {
	var entries []Entry
	if includeDefaults {
		entries = append(entries, Entry{AutoMode: true, Value: json.RawMessage(claudeDefaultRules)})
	}
	guidance := fmt.Sprintf(
		"Allow direct shell invocations for local Crosshall team coordination using the installed executable at %q or the bare command \"crosshall\" on PATH. "+
			"The first argument must be exactly one of %s. "+
			"This exception also covers merging Crosshall stderr into stdout with 2>&1 and a single display-only pipe to head or tail that reads standard input with only an optional line count. "+
			"Evaluate other shell operations and destinations under the normal rules.",
		executable, strings.Join(hook.RoutineMemberVerbs(), ", "),
	)
	raw, _ := json.Marshal(guidance)
	return append(entries, Entry{AutoMode: true, Value: raw})
}
