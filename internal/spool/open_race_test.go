package spool

import (
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

// replaceDuringOpen opens a private root holding name, then runs replace at
// the gate between the entry check and the open, as a writer that renames
// a new file over name at that moment would (#467).
func replaceDuringOpen(t *testing.T, name string, replace func(root *os.Root, call int)) (*os.Root, *int) {
	t.Helper()
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	if err := PublishServerFile(root, name, map[string]int{"v": 0}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	openRaceGate = func(gated string) {
		if gated == name {
			calls++
			replace(root, calls)
		}
	}
	t.Cleanup(func() { openRaceGate = nil })
	return root, &calls
}

func TestOpenServerFileRetriesAnAtomicReplace(t *testing.T) {
	root, calls := replaceDuringOpen(t, "link-status.json", func(root *os.Root, call int) {
		if call == 1 {
			if err := PublishServerFile(root, "link-status.json", map[string]int{"v": 1}); err != nil {
				t.Error(err)
			}
		}
	})
	file, err := OpenServerFile(root, "link-status.json", os.O_RDONLY)
	if err != nil {
		t.Fatalf("open across a rename: %v", err)
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil || string(data) != `{"v":1}` || *calls != 2 {
		t.Fatalf("read %q, %v after %d gated opens; want the replacement after 2", data, err, *calls)
	}
}

func TestOpenServerFileStopsAfterBoundedReplaces(t *testing.T) {
	root, calls := replaceDuringOpen(t, "link-status.json", func(root *os.Root, call int) {
		if err := PublishServerFile(root, "link-status.json", map[string]int{"v": call}); err != nil {
			t.Error(err)
		}
	})
	_, err := OpenServerFile(root, "link-status.json", os.O_RDONLY)
	if !errors.Is(err, errReplaced) || *calls != replacedOpenAttempts {
		t.Fatalf("got %v after %d gated opens; want the replace error after %d", err, *calls, replacedOpenAttempts)
	}
	if !strings.Contains(err.Error(), `"link-status.json" changed while opening`) {
		t.Fatalf("message changed: %v", err)
	}
}

func TestOpenServerFileRefusesANonPrivateReplacement(t *testing.T) {
	root, calls := replaceDuringOpen(t, "link-status.json", func(root *os.Root, call int) {
		if err := root.WriteFile("open.part", []byte(`{"v":1}`), 0600); err != nil {
			t.Error(err)
		}
		if err := root.Chmod("open.part", 0644); err != nil {
			t.Error(err)
		}
		if err := root.Rename("open.part", "link-status.json"); err != nil {
			t.Error(err)
		}
	})
	file, err := OpenServerFile(root, "link-status.json", os.O_RDONLY)
	if err == nil {
		file.Close()
		t.Fatal("opened a 0644 replacement")
	}
	if errors.Is(err, errReplaced) || *calls != 1 {
		t.Fatalf("got %v after %d gated opens; want a refusal without a retry", err, *calls)
	}
}

func TestOpenServerFileDoesNotRetryAWriteOpen(t *testing.T) {
	root, calls := replaceDuringOpen(t, "state.json", func(root *os.Root, call int) {
		if err := PublishServerFile(root, "state.json", map[string]int{"v": call}); err != nil {
			t.Error(err)
		}
	})
	_, err := OpenServerFile(root, "state.json", os.O_RDWR)
	if !errors.Is(err, errReplaced) || *calls != 1 {
		t.Fatalf("got %v after %d gated opens; want the replace error after 1", err, *calls)
	}
}
