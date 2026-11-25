package hook

import (
	"slices"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/hook/manifests"
)

// Every native identity proof a manifest names has a handler in this
// build, and every declared dialect field is consistent: a list-shaped
// workspace needs a named field.
func TestManifestDialectsNameKnownHandlers(t *testing.T) {
	for _, adapter := range manifests.Names() {
		m, ok := loadManifest(adapter)
		if !ok {
			t.Fatalf("%s: manifest not readable", adapter)
		}
		if _, known := identityProofs[m.IdentityProof]; m.IdentityProof != "" && !known {
			t.Errorf("%s: unknown identity proof %q", adapter, m.IdentityProof)
		}
		if m.CWDList && m.CWDField == "" {
			t.Errorf("%s: cwd_list without cwd_field", adapter)
		}
		for _, problem := range turnDialectProblems(m) {
			t.Errorf("%s: %s", adapter, problem)
		}
	}
	if problems := turnDialectProblems(perCallManifest(t)); len(problems) > 0 {
		t.Errorf("per-call dialect: %v", problems)
	}
}

// turnDialectProblems lists turn declarations no callback could honor.
func turnDialectProblems(m manifest) []string {
	var problems []string
	if m.StopActive != nil && !slices.Contains(m.LiteralFields, m.StopActive.Field) {
		problems = append(problems, "stop_active names a field that is not a literal field")
	}
	opens := false
	for _, spec := range m.Events {
		opens = opens || spec.OpenTurn
	}
	for name, spec := range m.Events {
		if (spec.Output == "continue" || spec.Output == "block") && !spec.Stop {
			problems = append(problems, name+": a continuation output belongs to a Stop event")
		}
		if spec.TurnFromOpen && !opens {
			problems = append(problems, name+": turn_from_open with no open_turn event")
		}
		if spec.OpenTurn && spec.turnField(m) == "" {
			problems = append(problems, name+": open_turn without a turn field")
		}
		if (len(spec.StopRequire) > 0 || spec.TurnFromOpen) && !spec.Stop {
			problems = append(problems, name+": a Stop declaration on another event")
		}
	}
	return problems
}
