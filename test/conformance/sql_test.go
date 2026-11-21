//go:build conformance

package conformance

import (
	"database/sql"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/store"
)

// The suite reads the store with SQL of its own, and only a run with a vendor
// binary executes it. This check needs no vendor binary, so CI runs it: every
// statement written in the suite must compile against a freshly migrated
// store, which fails when a migration renames or drops what one reads.
func TestTheSuitesSQLCompilesAgainstTheSchema(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	migrated, err := store.Open(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrated.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(root, "store.sqlite3")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	files := token.NewFileSet()
	packages, err := parser.ParseDir(files, ".", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, pkg := range packages {
		for _, file := range pkg.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				expr, ok := n.(ast.Expr)
				if !ok || !startsStatement(expr) {
					return true
				}
				at := files.Position(expr.Pos())
				statement, ok := stringValue(expr)
				if !ok {
					t.Errorf("%s: a statement is built from something other than string literals and names bound to one, so it cannot be checked", at)
					return false
				}
				prepared, err := db.Prepare(statement)
				if err != nil {
					t.Errorf("%s: %v\n%s", at, err, statement)
					return false
				}
				prepared.Close()
				checked++
				return false
			})
		}
	}
	if checked < 20 {
		t.Fatalf("found %d statements; the suite has more, so the search is broken", checked)
	}
}

// startsStatement reports whether the expression is a string, or a
// concatenation that begins with one, that opens an SQL statement.
func startsStatement(expr ast.Expr) bool {
	for {
		sum, ok := expr.(*ast.BinaryExpr)
		if !ok || sum.Op != token.ADD {
			break
		}
		expr = sum.X
	}
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return false
	}
	text, err := strconv.Unquote(lit.Value)
	if err != nil {
		return false
	}
	for _, verb := range []string{"SELECT ", "INSERT ", "UPDATE ", "DELETE ", "WITH "} {
		if strings.HasPrefix(strings.TrimSpace(text), verb) {
			return true
		}
	}
	return false
}

// stringValue evaluates string literals, their concatenations, and names
// declared with one in the same file.
func stringValue(expr ast.Expr) (string, bool) {
	switch e := expr.(type) {
	case *ast.BasicLit:
		text, err := strconv.Unquote(e.Value)
		return text, e.Kind == token.STRING && err == nil
	case *ast.ParenExpr:
		return stringValue(e.X)
	case *ast.BinaryExpr:
		left, ok := stringValue(e.X)
		right, also := stringValue(e.Y)
		return left + right, e.Op == token.ADD && ok && also
	case *ast.Ident:
		if e.Obj == nil {
			return "", false
		}
		switch decl := e.Obj.Decl.(type) {
		case *ast.AssignStmt:
			for i, name := range decl.Lhs {
				if id, ok := name.(*ast.Ident); ok && id.Name == e.Name && len(decl.Rhs) == len(decl.Lhs) {
					return stringValue(decl.Rhs[i])
				}
			}
		case *ast.ValueSpec:
			for i, name := range decl.Names {
				if name.Name == e.Name && len(decl.Values) == len(decl.Names) {
					return stringValue(decl.Values[i])
				}
			}
		}
	}
	return "", false
}
