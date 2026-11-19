package main

import (
	"os"
	"testing"
)

// Neither a /dev/null redirect nor a pipe is a terminal, so a redirected
// setup cannot ask questions or be offered.
func TestRedirectedStreamsAreNotTerminals(t *testing.T) {
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	for name, f := range map[string]*os.File{"/dev/null": null, "pipe reader": r, "pipe writer": w} {
		if isTerminal(f) {
			t.Errorf("%s reported as a terminal", name)
		}
	}
}
