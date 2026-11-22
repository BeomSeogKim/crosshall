package setuptest

import (
	"os"
	"strings"
	"testing"
)

// ReadmeBlock returns the text fence after a readme-check marker.
func ReadmeBlock(t testing.TB, label string) string {
	t.Helper()
	data, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	marker := "<!-- readme-check: " + label + " -->\n```text\n"
	if strings.Count(text, marker) != 1 {
		t.Fatalf("README must have one %q text block", label)
	}
	_, rest, _ := strings.Cut(text, marker)
	body, _, closed := strings.Cut(rest, "\n```\n")
	if !closed {
		t.Fatalf("README block %q has no closing fence", label)
	}
	return body + "\n"
}
