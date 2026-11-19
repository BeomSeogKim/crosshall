package domain

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/core"
)

// hookInternal results are read by the hook process, never shown to a member
// as text, so they carry no next step.
var hookInternal = map[string]bool{"hookUsage": true, "hookEvent": true, "hookEnroll": true}

// Every refusal and usage failure a member or the owner can read names a step
// that can succeed: no Refuse or Fail call in the domain has an empty hint.
func TestEveryRefusalNamesANextStep(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var missing []string
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || hookInternal[fn.Name.Name] {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				ident, ok := call.Fun.(*ast.Ident)
				if !ok {
					return true
				}
				at, known := map[string]int{"Refuse": 2, "Fail": 3, "waitFailure": 3}[ident.Name]
				if !known || len(call.Args) <= at {
					return true
				}
				if lit, ok := call.Args[at].(*ast.BasicLit); ok && lit.Value == `""` {
					missing = append(missing, fset.Position(call.Pos()).String())
				}
				return true
			})
		}
	}
	for _, at := range missing {
		t.Errorf("%s: refusal without a next step", at)
	}
}

// The join hint and the specification say the same thing: each rule's bold
// sentence in PROTOCOL.md section 3 appears in the hint verbatim, with the
// second sentences that carry meaning.
func TestHintCarriesTheSpecificationRules(t *testing.T) {
	raw, err := os.ReadFile("../../PROTOCOL.md")
	if err != nil {
		t.Fatal(err)
	}
	section := string(raw)
	start, end := strings.Index(section, "## 3. Member rules"), strings.Index(section, "## 4.")
	if start < 0 || end < start {
		t.Fatal("PROTOCOL.md has no section 3")
	}
	rules := regexp.MustCompile(`(?m)^\d\. \*\*(.+?)\*\*`).FindAllStringSubmatch(section[start:end], -1)
	if len(rules) != 6 {
		t.Fatalf("section 3 has %d rules, want 6", len(rules))
	}
	hint := Hint("tax", "checkout")
	for _, rule := range rules {
		if !strings.Contains(hint, rule[1]) {
			t.Errorf("hint lacks rule %q", rule[1])
		}
	}
	for _, also := range []string{"Later arrivals belong to the next turn.", "nowhere else", "If the deadline passed while your session was stopped, respond on your next turn.", "Omit notes sent solely as acknowledgments or thanks.", "Never loop on `inbox`."} {
		if !strings.Contains(hint, also) {
			t.Errorf("hint lacks %q", also)
		}
	}
}

// A refusal's next step must be one its recipient can take: a member is not
// pointed at a command that refuses it or at an owner-only command.
func TestRefusalStepsAreUsableForTheirRecipient(t *testing.T) {
	t.Run("one channel after leaving", func(t *testing.T) {
		f := newFixture(t)
		f.owner(CreateChannel{Name: "checkout"}, "")
		f.owner(CreateChannel{Name: "inventory"}, "")
		a := f.newSession("claude")
		wantOK(t, f.join(a, Join{Channel: "checkout", Alias: "form"}, ""))
		wantOK(t, f.run(a, Leave{}, ""))
		r := f.join(a, Join{Channel: "inventory"}, "")
		wantCode(t, r, core.ExitRefused, core.CodeOneChannel)
		hint := decode[Refusal](t, r).Hint
		if hint == "crosshall who" || !strings.Contains(hint, "crosshall join checkout") || !strings.Contains(hint, "new session") {
			t.Fatalf("hint %q", hint)
		}
	})
	t.Run("archived channel on ask", func(t *testing.T) {
		f, a, _, _ := channel3(t)
		f.roots(a, 1)
		if _, err := f.db.Exec(`UPDATE channel SET state = 'archived' WHERE name = 'checkout'`); err != nil {
			t.Fatal(err)
		}
		r := f.ask(a, Ask{To: []string{"tax"}, Body: "q"}, "")
		wantCode(t, r, core.ExitRefused, core.CodeArchived)
		if hint := decode[Refusal](t, r).Hint; strings.Contains(hint, "crosshall leave") || !strings.Contains(hint, "new session") {
			t.Fatalf("hint %q", hint)
		}
	})
	t.Run("archived channel on a repository join replay", func(t *testing.T) {
		f := newFixture(t)
		f.owner(CreateChannel{Name: "checkout"}, "")
		repo := t.TempDir()
		wantOK(t, f.owner(ChannelDefault{Channel: "checkout", RepoRoot: repo}, "default"))
		a := f.newSession("claude")
		wantOK(t, f.join(a, Join{RepoRoot: repo, Alias: "form"}, "repo-join"))
		if _, err := f.db.Exec(`UPDATE channel SET state = 'archived' WHERE name = 'checkout'`); err != nil {
			t.Fatal(err)
		}
		r := f.join(a, Join{RepoRoot: repo, Alias: "form"}, "repo-join")
		wantCode(t, r, core.ExitRefused, core.CodeArchived)
		hint := decode[Refusal](t, r).Hint
		if strings.HasPrefix(hint, "crosshall owner") || !strings.Contains(hint, "owner") || !strings.Contains(hint, "new session") {
			t.Fatalf("hint %q", hint)
		}
	})
}
