package domain

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/hook/manifests"
)

// vendorBranchesLeft lists the files that still compare an adapter name;
// each entry goes when its capability is read from the manifest instead.
var vendorBranchesLeft = map[string]bool{
	"views.go": true, // join hint for an unknown Antigravity CLI version
}

// The core asks an adapter's manifest what it can do. A comparison or switch
// case against an adapter name decides by vendor instead, and only the
// files listed above may still hold one.
func TestCoreDoesNotBranchOnAdapterNames(t *testing.T) {
	names := make(map[string]bool)
	for _, name := range manifests.Names() {
		names[name] = true
	}
	isName := func(e ast.Expr) bool {
		lit, ok := e.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return false
		}
		value, err := strconv.Unquote(lit.Value)
		return err == nil && names[value]
	}
	fset := token.NewFileSet()
	for _, dir := range []string{".", "../wire"} {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range files {
			if strings.HasSuffix(path, "_test.go") || dir == "." && vendorBranchesLeft[filepath.Base(path)] {
				continue
			}
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			ast.Inspect(file, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.BinaryExpr:
					if (n.Op == token.EQL || n.Op == token.NEQ) && (isName(n.X) || isName(n.Y)) {
						t.Errorf("%s: compares an adapter name; read the capability from its manifest", fset.Position(n.Pos()))
					}
				case *ast.CaseClause:
					for _, e := range n.List {
						if isName(e) {
							t.Errorf("%s: switches on an adapter name; read the capability from its manifest", fset.Position(e.Pos()))
						}
					}
				}
				return true
			})
		}
	}
}
