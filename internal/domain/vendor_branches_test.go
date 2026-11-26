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
	// The approved telemetry key TurnEndClaude reports Claude Code's
	// turn-end switch; telemetry vocabulary decides nothing in the core.
	"../telemetry/build.go snapshot": 1,
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
	isSQLName := func(e ast.Expr) bool {
		lit, ok := e.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return false
		}
		value, err := strconv.Unquote(lit.Value)
		return err == nil && sqlNamesAdapter(value, names)
	}
	for _, dir := range []string{".", "../wire", "../cli", "../hook", "../setup", "../telemetry", "../dispatch", "../publish", "../ident", "../store", "../link"} {
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
					case *ast.BasicLit:
						if isSQLName(n) {
							found[key]++
							if vendorBranchesLeft[key] == 0 {
								t.Errorf("%s: SQL decides by adapter name; read the capability from its manifest", fset.Position(n.Pos()))
							}
						}
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

// sqlNamesAdapter reports SQL text that names an adapter as a value: quoted
// as a string ('claude'), or double-quoted ("claude"), which SQLite reads as
// a string when no column has that name.
func sqlNamesAdapter(sql string, names map[string]bool) bool {
	for name := range names {
		if strings.Contains(sql, "'"+name+"'") || strings.Contains(sql, `"`+name+`"`) {
			return true
		}
	}
	return false
}

func TestSQLNamingAnAdapterIsFound(t *testing.T) {
	names := map[string]bool{"codex": true}
	for sql, want := range map[string]bool{
		"SELECT 1 FROM leaf WHERE vendor = 'codex'":    true,
		`SELECT 1 FROM leaf WHERE vendor = "codex"`:    true,
		"SELECT 1 FROM leaf WHERE vendor = ?":          false,
		"SELECT 1 FROM leaf WHERE vendor = 'codexcli'": false,
		`SELECT "codex_route" FROM leaf`:               false,
	} {
		if got := sqlNamesAdapter(sql, names); got != want {
			t.Errorf("%s: %t, want %t", sql, got, want)
		}
	}
}
