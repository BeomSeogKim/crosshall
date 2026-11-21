package setup

import (
	"bytes"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"text/scanner"
)

func existingCodexHostRule(path string, skipManaged bool) (*retainedFile, error) {
	location, err := openConfigLocation(path)
	if err != nil {
		return nil, err
	}
	defer location.dir.Close()
	if location.missing != "" {
		return nil, nil
	}
	files, err := fs.ReadDir(location.dir.FS(), ".")
	if err != nil {
		return nil, err
	}
	for _, file := range files {
		name := file.Name()
		if file.IsDir() || filepath.Ext(name) != ".rules" || skipManaged && name == filepath.Base(path) {
			continue
		}
		existing := filepath.Join(filepath.Dir(path), name)
		snapshot, err := readConfigAt(location.dir, name)
		if err != nil {
			return &retainedFile{"codex", existing, fmt.Sprintf("could not inspect existing rules; skipped Codex command rules: %v", err)}, nil
		}
		if mayDeclareCrosshallHost(snapshot.Bytes) {
			return &retainedFile{"codex", existing, "existing host_executable may bind crosshall; skipped Codex command rules"}, nil
		}
	}
	return nil, nil
}

// Custom Starlark can compute the executable name or call an alias. Only
// a direct declaration with a literal different name proves it is unrelated.
func mayDeclareCrosshallHost(raw []byte) bool {
	var lex scanner.Scanner
	lex.Init(bytes.NewReader(raw))
	lex.Mode = scanner.ScanIdents | scanner.ScanStrings | scanner.ScanChars
	lex.Error = func(*scanner.Scanner, string) {}
	next := func() rune {
		for {
			token := lex.Scan()
			if token != '#' {
				return token
			}
			for ch := lex.Peek(); ch != '\n' && ch != scanner.EOF; ch = lex.Peek() {
				lex.Next()
			}
		}
	}
	for token := next(); token != scanner.EOF; token = next() {
		if token == scanner.Ident && lex.TokenText() == "host_executable" &&
			(next() != '(' || !otherCodexHostName(&lex, next)) {
			return true
		}
	}
	return false
}

func otherCodexHostName(lex *scanner.Scanner, next func() rune) bool {
	depth, start, first := 0, true, true
	for token := next(); token != scanner.EOF; token = next() {
		if depth == 0 && start {
			named := token == scanner.Ident && lex.TokenText() == "name"
			if named {
				if next() != '=' {
					return false
				}
				token = next()
			}
			if (named || first) && (token == scanner.String || token == scanner.Char) {
				raw := lex.TokenText()
				if len(raw) < 2 {
					return false
				}
				name := raw[1 : len(raw)-1]
				end := next()
				return (end == ',' || end == ')') && name != "" &&
					!strings.ContainsAny(name, "\\'\"\r\n") && !strings.EqualFold(name, "crosshall")
			}
			if named {
				return false
			}
			start, first = false, false
		}
		switch token {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			if depth == 0 {
				return false
			}
			depth--
		case ',':
			start = depth == 0
		}
	}
	return false
}
