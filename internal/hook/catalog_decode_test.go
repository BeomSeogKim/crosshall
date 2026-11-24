package hook

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/hook/manifests"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

// A hook invocation selects its adapter from its arguments and decodes only
// that adapter's manifest, never the whole catalog.
func TestAHookDecodesOnlyItsOwnManifest(t *testing.T) {
	state := t.TempDir()
	if err := os.Chmod(state, 0700); err != nil {
		t.Fatal(err)
	}
	var policy strings.Builder
	for _, adapter := range manifests.Names() {
		policy.WriteString(adapter + " 1 0 1\n")
	}
	if err := os.WriteFile(filepath.Join(state, StatePolicyName), []byte(policy.String()), 0600); err != nil {
		t.Fatal(err)
	}
	locate := func() (Paths, error) {
		return Paths{State: state, Rendezvous: filepath.Join(state, "rendezvous"),
			Start: func(context.Context) (bool, error) { return false, errors.New("no dispatcher in this test") }}, nil
	}
	for _, adapter := range manifests.Names() {
		m, ok := loadManifest(adapter)
		if !ok {
			t.Fatalf("%s: manifest not readable", adapter)
		}
		for _, event := range m.Install.Events {
			var mu sync.Mutex
			var read []string
			restore := manifests.SwapReader(func(name string) ([]byte, error) {
				mu.Lock()
				read = append(read, name)
				mu.Unlock()
				return manifests.FS.ReadFile(name + ".json")
			})
			body := `{"` + m.EventField + `":"` + event + `","` + m.SessionField + `":"s"}`
			Run(time.Now(), []string{adapter, event}, strings.NewReader(body), io.Discard, locate)
			restore()
			mu.Lock()
			if len(read) == 0 {
				t.Errorf("%s %s: decoded no manifest", adapter, event)
			}
			for _, name := range read {
				if name != adapter {
					t.Errorf("%s %s: decoded the %s manifest too (%v)", adapter, event, name, read)
					break
				}
			}
			mu.Unlock()
		}
	}
}

// The catalog derives prompt_line and session_end from the same fact names
// the hooks report.
func TestCatalogFactNamesAreTheWireFacts(t *testing.T) {
	if wire.HookPromptSubmitted != "prompt_submitted" || wire.HookSessionEnded != "session_ended" {
		t.Fatalf("wire facts %q, %q no longer match the catalog's", wire.HookPromptSubmitted, wire.HookSessionEnded)
	}
}
