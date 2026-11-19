package client

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/cli"
	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/spool"
)

func TestTypedRequests(t *testing.T) {
	for _, tc := range []struct {
		command domain.Command
		verb    string
		fields  map[string]string
	}{
		{domain.Join{Channel: "a", Focus: "hello"}, "join", map[string]string{"channel": "a", "focus": "hello"}},
		{domain.CreateChannel{Name: "a", Topic: "hello"}, "owner channel create", map[string]string{"name": "a", "topic": "hello"}},
		{domain.Who{}, "who", map[string]string{}},
	} {
		req, err := request(tc.command)
		parsed := map[string]string{}
		if err != nil || req.Verb != tc.verb || json.Unmarshal(req.Args, &parsed) != nil || !reflect.DeepEqual(parsed, tc.fields) {
			t.Fatalf("%s request: %+v, %v", tc.verb, req, err)
		}
	}
}

func TestUnknownVersionWarnings(t *testing.T) {
	raw := json.RawMessage(`{"id":9007199254740993,"warnings":["existing warning"],"members":[{"vendor":"codex","vendor_version":"9"},{"vendor":"claude","vendor_version":"2.1.327"}],"vendors":[{"vendor":"agy","vendor_version":""}]}`)
	body, err := withWarnings(raw, ident.Identity{Vendor: "codex", Version: "9"}, "owner doctor", false)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		ID       int64
		Warnings []string
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	want := []string{"existing warning", "identity variable unverified on codex 9", "identity variable unverified on agy: CLI version unknown"}
	if got.ID != 9007199254740993 || !reflect.DeepEqual(got.Warnings, want) {
		t.Fatalf("unexpected details: %+v", got)
	}
}

func TestTransportOutcomes(t *testing.T) {
	// The spool intentionally does not exist. A valid member gets the
	// underlying unsent result; callers without identity never reach the spool.
	transport := New(spool.Client{Rendezvous: filepath.Join(t.TempDir(), "absent")})
	var env = map[string]string{"CODEX_THREAD_ID": "test-thread"}
	transport.lookup = func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	call := cli.Call{Command: domain.Join{Channel: "app"}, OpKey: "key-1"}
	result, err := transport.Do(context.Background(), call)
	if err != nil || result.Code != core.CodeUnsent || !strings.Contains(string(result.Data), "key-1") {
		t.Fatalf("unsent: %+v, %v", result, err)
	}
	owner := cli.Call{Owner: true, Command: domain.CreateChannel{Name: "x"}}
	if result, err = transport.Do(context.Background(), owner); err != nil || result.Code != core.CodeOwnerOnly {
		t.Fatalf("owner gate: %+v, %v", result, err)
	}
	delete(env, "CODEX_THREAD_ID")
	if result, err = transport.Do(context.Background(), call); err != nil || result.Status != core.ExitNotEnrolled || result.Code != core.CodeNotEnrolled {
		t.Fatalf("missing identity: %+v, %v", result, err)
	}
	var out bytes.Buffer
	if status := transport.Whoami(&out, false); status != core.ExitNotEnrolled || !strings.Contains(out.String(), "not_enrolled") ||
		!strings.Contains(out.String(), "next: start a member shell with `crosshall run -- sh`") {
		t.Fatalf("unbound whoami: %d %s", status, out.String())
	}
	env["CODEX_THREAD_ID"] = "test-thread"
	displayKey := "sha256:" + (ident.Identity{Vendor: "codex", Key: "test-thread"}).Digest()[:12]
	out.Reset()
	if status := transport.Whoami(&out, false); status != core.ExitUnavailable || !strings.Contains(out.String(), "codex:"+displayKey+" unknown") {
		t.Fatalf("unavailable whoami: %d %s", status, out.String())
	}
	out.Reset()
	var receipt struct {
		Code                    core.Code
		Vendor, Key, Enrollment string
	}
	if status := transport.WhoamiJSON(&out, false); status != core.ExitUnavailable || json.Unmarshal(out.Bytes(), &receipt) != nil ||
		receipt.Code != core.CodeUnsent || receipt.Vendor != "codex" || receipt.Key != displayKey || receipt.Enrollment != "unknown" {
		t.Fatalf("JSON whoami: %d %s", status, out.String())
	}
}

func TestWhoamiKeyDisclosure(t *testing.T) {
	const key = "fixture-session-key"
	const digest = "sha256:a1e9751ef75c" // first 12 hex digits of SHA-256("codex" + NUL + key)
	transport := New(spool.Client{Rendezvous: filepath.Join(t.TempDir(), "absent")})
	transport.lookup = func(name string) (string, bool) { return key, name == "CODEX_THREAD_ID" }
	env := cli.Env{Whoami: transport.Whoami, WhoamiJSON: transport.WhoamiJSON}
	for _, tc := range []struct {
		name string
		args []string
		want string
		json bool
	}{
		{"plain", []string{"whoami"}, digest, false},
		{"json", []string{"whoami", "--json"}, digest, true},
		{"full", []string{"whoami", "--full"}, key, false},
		{"json full", []string{"--json", "whoami", "--full"}, key, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, diag bytes.Buffer
			exit := cli.RunEnv(context.Background(), env, tc.args, &out, &diag)
			if exit != core.ExitUnavailable {
				t.Fatalf("exit %d, want %d", exit, core.ExitUnavailable)
			}
			if tc.want == digest && strings.Contains(out.String()+diag.String(), key) {
				t.Fatal("default diagnostic exposed the session key")
			}
			if tc.json {
				var got struct{ Vendor, Key, Enrollment string }
				if err := json.Unmarshal(out.Bytes(), &got); err != nil || got.Vendor != "codex" || got.Key != tc.want || got.Enrollment != "unknown" {
					t.Fatalf("unexpected JSON identity: %+v (%v)", got, err)
				}
			} else if !strings.HasPrefix(out.String(), "codex:"+tc.want+" unknown (unsent)") {
				t.Fatalf("unexpected plain identity %q", out.String())
			}
		})
	}
}

func TestWhoamiUnknownAntigravityWithoutDispatcher(t *testing.T) {
	id := ident.Identity{Vendor: "agy", Key: "private-session", Version: "unknown"}
	result := core.Result{Status: core.ExitUnavailable, Code: core.CodeUnsent}
	for _, asJSON := range []bool{false, true} {
		var out bytes.Buffer
		if code := writeWhoami(&out, id, result, asJSON, false); code != core.ExitUnavailable ||
			!strings.Contains(out.String(), "identity variable unverified on agy: CLI version unknown") ||
			strings.Contains(out.String(), id.Key) {
			t.Errorf("whoami (JSON %t): %d %s", asJSON, code, &out)
		}
	}
}

func TestWhoamiKnownEnrollmentStates(t *testing.T) {
	id := ident.Identity{Vendor: "codex", Key: "fixture-session-key"}
	for _, tc := range []struct {
		result core.Result
		state  string
	}{
		{core.Result{Status: core.ExitOK}, "enrolled"},
		{core.Result{Status: core.ExitNotEnrolled, Code: core.CodeNotEnrolled,
			Data: json.RawMessage(`{"message":"fixture-session-key is not enrolled","hint":"check fixture-session-key"}`)}, "not_enrolled"},
	} {
		for _, asJSON := range []bool{true, false} {
			var out bytes.Buffer
			if code := writeWhoami(&out, id, tc.result, asJSON, false); code != tc.result.Status {
				t.Fatalf("returned %d, want %d", code, tc.result.Status)
			}
			if !strings.Contains(out.String(), "sha256:a1e9751ef75c") || !strings.Contains(out.String(), tc.state) || strings.Contains(out.String(), id.Key) {
				t.Fatalf("unexpected identity for %s: %q", tc.state, out.String())
			}
		}
	}
}

func TestWhoamiFullExplainsHowToRestoreMembership(t *testing.T) {
	id := ident.Identity{Vendor: "codex", Key: "fixture-session-key"}
	result := core.Result{Status: core.ExitNotEnrolled, Code: core.CodeNotEnrolled,
		Data: json.RawMessage(`{"message":"this session is not in a channel","hint":"crosshall join <channel>"}`)}
	for _, asJSON := range []bool{false, true} {
		var out bytes.Buffer
		if code := writeWhoami(&out, id, result, asJSON, true); code != core.ExitNotEnrolled {
			t.Fatalf("exit %d, want %d", code, core.ExitNotEnrolled)
		}
		body := out.String()
		if !strings.Contains(body, "give the owner") || !strings.Contains(body, "crosshall owner restore") {
			t.Errorf("missing restore instructions: %s", body)
		}
		if asJSON {
			var got struct {
				Vendor, Key, Enrollment, Message, Hint string
				Code                                   core.Code
			}
			if err := json.Unmarshal(out.Bytes(), &got); err != nil ||
				got.Vendor != id.Vendor || got.Key != id.Key || got.Enrollment != "not_enrolled" ||
				got.Code != result.Code || got.Message != "this session is not in a channel" ||
				!strings.Contains(got.Hint, "crosshall join <channel>") {
				t.Fatalf("unexpected JSON identity: %+v (%v)", got, err)
			}
		} else if !strings.HasPrefix(body, "codex:fixture-session-key not_enrolled\n") {
			t.Errorf("unexpected first line: %s", body)
		}
	}

	for _, tc := range []struct {
		id     ident.Identity
		result core.Result
		full   bool
	}{
		{id, result, false},
		{ident.Identity{}, result, true},
		{id, core.Result{Status: core.ExitOK}, true},
		{id, core.Result{Status: core.ExitUnavailable, Code: core.CodeUnsent}, true},
	} {
		for _, asJSON := range []bool{false, true} {
			var out bytes.Buffer
			writeWhoami(&out, tc.id, tc.result, asJSON, tc.full)
			if strings.Contains(out.String(), "crosshall owner restore") {
				t.Errorf("unexpected restore hint: %s", out.String())
			}
			if !tc.full && strings.Contains(out.String(), id.Key) {
				t.Errorf("default output exposed the session key: %s", out.String())
			}
		}
	}
}

func TestIdentityRecoveryNamesTheShellAndJoinStep(t *testing.T) {
	for _, code := range []core.Code{core.CodeNotEnrolled, core.CodeBindingAmbiguous} {
		result := identityFailure(&ident.EnvironmentError{Code: code, Message: "fixture"})
		var got struct{ Hint string }
		if err := json.Unmarshal(result.Data, &got); err != nil ||
			!strings.Contains(got.Hint, "`crosshall run -- sh`") || strings.Contains(got.Hint, "<agent>") {
			t.Errorf("%s: hint %q (%v)", code, got.Hint, err)
		}
		if code == core.CodeNotEnrolled && !strings.Contains(got.Hint, "from the new shell") {
			t.Errorf("%s: hint %q omits where to join", code, got.Hint)
		}
	}
}

func TestJoinVersionWarningsExplainTheReceipt(t *testing.T) {
	for _, tc := range []struct {
		vendor, version, want string
	}{
		{"claude", "unknown", "could not detect the Claude Code version"},
		{"claude", "2.1.999", "has not verified how Claude Code 2.1.999 identifies sessions"},
		{"codex", "unknown", "could not detect the Codex CLI version"},
		{"codex", "9.9.9", "has not verified how Codex CLI 9.9.9 identifies sessions"},
	} {
		body, err := withWarnings(json.RawMessage(`{"hint":"joined"}`),
			ident.Identity{Vendor: tc.vendor, Version: tc.version}, "join", false)
		var got struct{ Warnings []string }
		if err != nil || json.Unmarshal(body, &got) != nil || len(got.Warnings) != 1 {
			t.Fatalf("%s %s: warnings %s (%v)", tc.vendor, tc.version, body, err)
		}
		msg := got.Warnings[0]
		if !strings.HasPrefix(msg, "Join succeeded. ") || !strings.Contains(msg, tc.want) ||
			!strings.HasSuffix(msg, "No action is needed for this warning.") || strings.Contains(msg, "crosshall whoami") {
			t.Errorf("%s %s: unexpected warning %q", tc.vendor, tc.version, msg)
		}
	}
}
