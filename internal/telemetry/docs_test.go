package telemetry

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// Every registry key has a row in docs/telemetry.md, and every row names
// registry keys only. A row writes alternatives in braces.
func TestEveryKeyIsDocumented(t *testing.T) {
	doc, err := os.ReadFile("../../docs/telemetry.md")
	if err != nil {
		t.Fatal(err)
	}
	_, table, ok := strings.Cut(string(doc), "### Counts\n")
	if !ok {
		t.Fatal("docs/telemetry.md has no Counts section")
	}
	table, _, _ = strings.Cut(table, "\n## ")
	row := regexp.MustCompile("(?m)^\\| `([^`]+)` \\|")
	var documented []string
	for _, m := range row.FindAllStringSubmatch(table, -1) {
		documented = append(documented, expand(m[1])...)
	}
	slices.Sort(documented)
	if want := Keys(); !slices.Equal(documented, want) {
		for _, k := range want {
			if !slices.Contains(documented, k) {
				t.Errorf("undocumented key %s", k)
			}
		}
		for _, k := range documented {
			if !slices.Contains(want, k) {
				t.Errorf("documented key %s is not in the registry", k)
			}
		}
		t.Fatalf("%d documented keys, %d in the registry", len(documented), len(want))
	}
}

// expand turns "a.{b,c}.d" into "a.b.d" and "a.c.d".
func expand(pattern string) []string {
	open := strings.IndexByte(pattern, '{')
	if open < 0 {
		return []string{pattern}
	}
	closing := open + strings.IndexByte(pattern[open:], '}')
	var out []string
	for _, alt := range strings.Split(pattern[open+1:closing], ",") {
		out = append(out, expand(pattern[:open]+alt+pattern[closing+1:])...)
	}
	return out
}
