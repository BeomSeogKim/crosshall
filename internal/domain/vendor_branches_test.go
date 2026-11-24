package domain

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/hook/manifests"
)

// vendorBranchesLeft counts the adapter-name decisions each function may
// still hold, keyed by file and function. An entry goes when its capability
// is read from the manifest instead.
var vendorBranchesLeft = map[string]int{
	"../setup/command_rule_plan.go checkCommandRuleRelocation": 1, // command rules (#327 part 3)
	"../setup/command_rule_plan.go preflightCommandRules":      3, // command rules (#327 part 3)
	"../setup/command_rule_syntax.go memberCommandRules":       3, // command rules (#327 part 3)
	"../setup/detect.go hookEntries":                           3, // hook entries and state pin (#327 part 2)
	"../setup/inspect.go inspectHookEntries":                   2, // hook recognition, trust status (#327 part 2)
	"../setup/inspect.go recognizableHook":                     3, // hook recognition (#327 part 2)
	"../setup/inspect.go recognizableHooks":                    1, // hook recognition (#327 part 2)
	"../setup/journal.go ownsDirectories":                      1, // owned config directories (#327 part 2)
	"../setup/plan.go checkHookRelocation":                     1, // hook relocation (#327 part 2)
	"../setup/plan.go planJSONInstall":                         4, // revision upgrades and relocation (#327 part 2)
	"../setup/plan.go planRemoval":                             1, // rule removal (#327 part 3)
	"../setup/plan.go preflightInstall":                        1, // hook layout (#327 part 2)
	"../setup/plan.go recordedHookExecutable":                  1, // hook relocation (#327 part 2)
	"../setup/run.go apply":                                    1, // command rule conflict (#327 part 3)
	"../setup/vendor_plan.go pendingClaudeRules":               1, // command rules (#327 part 3)
	"../setup/vendor_plan.go resumedLegacyCodex":               1, // legacy rules (#327 part 3)
}

// The core asks an adapter's manifest what it can do. A comparison or switch
// case against an adapter name, spelled as a literal or as a constant or
// variable that holds one, decides by vendor instead. Only the functions
// listed above may still hold one, exactly as many as listed.
func TestCoreDoesNotBranchOnAdapterNames(t *testing.T) {
	names := make(map[string]bool)
	for _, name := range manifests.Names() {
		names[name] = true
	}
	isNameLiteral := func(e ast.Expr) bool {
		lit, ok := e.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return false
		}
		value, err := strconv.Unquote(lit.Value)
		return err == nil && names[value]
	}
	found := make(map[string]int)
	fset := token.NewFileSet()
	for _, dir := range []string{".", "../wire", "../cli", "../hook", "../setup", "../telemetry"} {
		paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		var files []*ast.File
		var filePaths []string
		// Identifiers declared with an adapter name as their value.
		holders := make(map[string]bool)
		for _, path := range paths {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			files, filePaths = append(files, file), append(filePaths, path)
			ast.Inspect(file, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.ValueSpec:
					for i, value := range n.Values {
						if i < len(n.Names) && isNameLiteral(value) {
							holders[n.Names[i].Name] = true
						}
					}
				case *ast.AssignStmt:
					for i, value := range n.Rhs {
						if id, ok := n.Lhs[min(i, len(n.Lhs)-1)].(*ast.Ident); ok && isNameLiteral(value) {
							holders[id.Name] = true
						}
					}
				}
				return true
			})
		}
		isName := func(e ast.Expr) bool {
			id, ok := e.(*ast.Ident)
			return isNameLiteral(e) || ok && holders[id.Name]
		}
		for i, file := range files {
			for _, decl := range file.Decls {
				key := filePaths[i]
				if fn, ok := decl.(*ast.FuncDecl); ok {
					key += " " + fn.Name.Name
				}
				ast.Inspect(decl, func(n ast.Node) bool {
					var at []ast.Expr
					switch n := n.(type) {
					case *ast.BinaryExpr:
						if n.Op == token.EQL || n.Op == token.NEQ {
							at = []ast.Expr{n.X, n.Y}
						}
					case *ast.CaseClause:
						at = n.List
					}
					for _, e := range at {
						if isName(e) {
							found[key]++
							if vendorBranchesLeft[key] == 0 {
								t.Errorf("%s: decides by adapter name; read the capability from its manifest", fset.Position(e.Pos()))
							}
						}
					}
					return true
				})
			}
		}
	}
	for _, key := range slices.Sorted(maps.Keys(vendorBranchesLeft)) {
		if want := vendorBranchesLeft[key]; found[key] != want {
			t.Errorf("%s: %d adapter-name decisions, allowed exactly %d; update vendorBranchesLeft", key, found[key], want)
		}
	}
}
