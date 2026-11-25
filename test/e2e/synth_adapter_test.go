//go:build e2e && synthadapter

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func init() { binaryTags += ",synthadapter" }

const synthOriginal = "{\n  \"owner\": true,\n  \"hooks\": {\"SessionStart\": [{\"hooks\":[{\"type\":\"command\",\"command\":\"synth-owner-hook\"}]}]}\n}\n"

// newSynthWorld adds the synth fixture vendor, built in only under the
// synthadapter tag, to a setup world: its CLI probe and its home.
func newSynthWorld(t *testing.T) *setupWorld {
	t.Helper()
	f := newSetupWorld(t)
	home := filepath.Join(f.home, "synth test config")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	f.env = append(f.env, "SYNTH_HOME="+home)
	f.paths["synth"] = filepath.Join(home, "hooks.json")
	if err := os.WriteFile(f.paths["synth"], []byte(synthOriginal), 0600); err != nil {
		t.Fatal(err)
	}
	probe := "#!/bin/sh\n[ \"$1\" = --version ] || exit 9\nprintf 'synth 1.0.0\\n'\n"
	if err := os.WriteFile(filepath.Join(filepath.Dir(f.executable), "synth"), []byte(probe), 0700); err != nil {
		t.Fatal(err)
	}
	return f
}

// invokeSynth runs the installed synth hook for event with the fixture's
// own payload fields.
func (f *setupWorld) invokeSynth(repo, session, event, fields string) string {
	f.t.Helper()
	hook := setupHookFor(f.t, readSetupFile(f.t, f.paths["synth"]), event)
	if hook.Command != f.executable || !slices.Equal(hook.Args, []string{"hook", "synth", event}) || hook.Timeout != 1 {
		f.t.Fatalf("synth %s hook: %+v", event, hook)
	}
	cmd := exec.CommandContext(context.Background(), hook.Command, hook.Args...)
	payload := fmt.Sprintf(`{"event":%q,"session":%q,"cwd":%q%s}`, event, session, repo, fields)
	var stdout, stderr bytes.Buffer
	cmd.Dir, cmd.Env, cmd.Stdin, cmd.Stdout, cmd.Stderr = repo, f.env, strings.NewReader(payload), &stdout, &stderr
	if err := cmd.Run(); err != nil || stderr.Len() != 0 {
		f.t.Fatalf("invoke synth %s: %v, stderr %q", event, err, stderr.String())
	}
	return stdout.String()
}

func TestSynthAdapterInstallsThroughTheNormalSetup(t *testing.T) {
	f := newSynthWorld(t)
	out := f.apply("", "setup", "synth", "--apply")
	var installed struct{ Hooks map[string]json.RawMessage }
	if err := json.Unmarshal(readSetupFile(t, f.paths["synth"]).bytes, &installed); err != nil {
		t.Fatal(err)
	}
	t.Logf("apply: %s\nfile: %s", out.stdout, readSetupFile(t, f.paths["synth"]).bytes)
}
