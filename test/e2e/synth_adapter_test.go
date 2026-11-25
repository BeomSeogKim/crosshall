//go:build e2e && synthadapter

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"strconv"
	"testing"
	"time"
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

// The synth fixture is a fresh adapter ID that only its bundle defines: a
// manifest, an identity row and an installer, each in a file that only the
// synthadapter tag compiles. Through the normal entry points it installs
// beside the owner's hooks, enrolls, is identified, gets a request at its
// next prompt, follows owner policy and uninstalls byte for byte.
func TestSynthAdapterWorksThroughTheNormalEntryPoints(t *testing.T) {
	f := newSynthWorld(t)
	original := readSetupFile(t, f.paths["synth"])
	if got := f.apply("", "setup", "synth", "--apply"); !strings.Contains(got.stdout, `"outcome":"installed"`) {
		t.Fatalf("setup: %s", got.stdout)
	}
	installed := readSetupFile(t, f.paths["synth"]).bytes
	if !bytes.Contains(installed, []byte(`"command":"synth-owner-hook"`)) || !bytes.Contains(installed, []byte(`"owner": true`)) {
		t.Fatalf("setup changed the owner's settings: %s", installed)
	}
	status := f.apply("", "setup", "status").stdout
	if !strings.Contains(status, `"adapter":"synth","name":"Synth Fixture"`) || !strings.Contains(status, `"install":"installed"`) {
		t.Fatalf("status: %s", status)
	}

	repo := f.enrollmentRepo()
	session := setupSessionPrefix + "synth"
	f.waitForWelcomeDelivery("synth", "SessionStart", time.Now().Add(fixtureEventTimeout), func(string) string {
		return f.invokeSynth(repo, session, "SessionStart", "")
	})
	f.waitEnrollmentPackets("synth", "SessionStart", time.Now().Add(fixtureEventTimeout))
	if evidence := f.enrollmentEvidence("synth"); !strings.Contains(evidence, "live binding") {
		t.Fatalf("synth session did not enroll: %s", evidence)
	}
	member := &participant{w: f.world, extra: []string{"SYNTH_SESSION_ID=" + session}}
	if whoami := member.must("whoami"); whoami.obj["vendor"] != "synth" || whoami.obj["enrollment"] != "enrolled" ||
		!strings.Contains(fmt.Sprint(whoami.obj["join_hint"]), "as @synth1.") {
		t.Fatalf("synth session identified as %v", whoami.obj)
	}

	writer := f.member()
	writer.must("join", "setup-check", "--as", "writer")
	id := strconv.FormatInt(num(t, writer.must("ask", "@synth1", "a request for the fixture"), "id"), 10)
	want := pendingContext([]string{id})
	deadline := time.Now().Add(fixtureEventTimeout)
	for turn := 1; ; turn++ {
		got := hostContext(t, f.invokeSynth(repo, session, "UserPromptSubmit", ""))
		f.waitEnrollmentPackets("synth", "UserPromptSubmit", time.Now().Add(fixtureEventTimeout))
		if got == want {
			break
		}
		if got != "" || time.Now().After(deadline) {
			t.Fatalf("prompt %d offered %q, want %q", turn, got, want)
		}
	}
	member.must("read")
	member.must("reply", id, "done")
	if got := writer.must("read").stdout; !strings.Contains(got, "done") {
		t.Fatalf("the writer did not get the reply: %s", got)
	}

	f.owner().must("owner", "adapter", "set", "synth", "--enabled", "off")
	writer.must("ask", "@synth1", "while policy is off")
	if got := f.invokeSynth(repo, session, "UserPromptSubmit", ""); got != "" && got != "{}" {
		t.Fatalf("a disabled adapter's prompt offered %q", got)
	}
	if doctor := f.apply("", "owner", "doctor").stdout; !strings.Contains(doctor, "synth") {
		t.Fatalf("doctor does not list the fixture: %s", doctor)
	}

	f.apply("", "uninstall", "synth", "--apply")
	assertSetupFile(t, f.paths["synth"], original, false)
}
