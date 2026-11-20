package hook

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/publish"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

func TestAntigravitySessionStartAcceptsOnlySupportedPayload(t *testing.T) {
	const conversation = "ec33ebf9-0cba-4100-8142-c61503f6c587"
	home := t.TempDir()
	workspace := filepath.Join(home, "workspace")
	transcript := func(surface string) string {
		return filepath.Join(home, ".gemini", surface, "brain", conversation, ".system_generated", "logs", "transcript.jsonl")
	}
	m, ok := loadManifest("agy")
	if !ok {
		t.Fatal("Antigravity manifest unavailable")
	}
	spec, ok := m.event("SessionStart")
	if !ok {
		t.Fatal("SessionStart unavailable")
	}
	if len(m.Events) != 1 || spec.Stop {
		t.Fatal("Antigravity acquired another hook capability")
	}
	if _, sends := publish.Lookup("agy"); sends {
		t.Fatal("Antigravity acquired a publish adapter")
	}
	for _, test := range []struct {
		name    string
		field   string
		value   any
		envID   string
		wantErr bool
	}{
		{name: "documented fields"},
		{name: "named callback", field: "hookEventName", value: "SessionStart", envID: conversation},
		{name: "captured CLI transcript", field: "transcriptPath", value: filepath.Join(filepath.Dir(transcript("antigravity-cli")), "transcript_full.jsonl"), envID: conversation},
		{name: "other conversation", envID: "different", wantErr: true},
		{name: "wrong callback", field: "hookEventName", value: "Stop", wantErr: true},
		{name: "non UUID", field: "conversationId", value: "../x", wantErr: true},
		{name: "app surface", field: "transcriptPath", value: transcript("antigravity"), wantErr: true},
		{name: "IDE surface", field: "transcriptPath", value: transcript("antigravity-ide"), wantErr: true},
		{name: "missing workspace", field: "workspacePaths", wantErr: true},
		{name: "relative workspace", field: "workspacePaths", value: []string{"workspace"}, wantErr: true},
		{name: "multiple workspaces", field: "workspacePaths", value: []string{workspace, home}, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := map[string]any{
				"conversationId": conversation,
				"workspacePaths": []string{workspace},
				"transcriptPath": transcript("antigravity-cli"),
				"lastUserInput":  "private prompt",
			}
			if test.field != "" {
				if test.value == nil {
					delete(input, test.field)
				} else {
					input[test.field] = test.value
				}
			}
			payload, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
			lookup := func(key string) (string, bool) {
				switch key {
				case "HOME":
					return home, true
				case "ANTIGRAVITY_CONVERSATION_ID":
					return test.envID, test.envID != ""
				case "ANTIGRAVITY_LS_VERSION":
					return "cli-1.5.10", true
				default:
					return "", false
				}
			}
			got, err := parseEvent(strings.NewReader(string(payload)), m, spec, lookup)
			if (err != nil) != test.wantErr {
				t.Fatalf("parse result: %+v, error=%v", got, err)
			}
			if test.wantErr {
				return
			}
			if got.Identity.Vendor != "agy" || got.Identity.Key != conversation || got.CWD != workspace || got.Source != "startup" ||
				got.VendorVersion != "unverified" || got.LaunchRef != "" || got.receipt != "" {
				t.Fatalf("unexpected startup event: %+v", got)
			}
			req := makeRequest(time.Now(), m, spec, got, workspace, true, 1)
			if req.Verb != wire.VerbHookEnroll || strings.Contains(string(req.Args), "private prompt") ||
				strings.Contains(string(req.Args), transcript("antigravity-cli")) {
				t.Fatalf("unexpected enrollment request: %+v", req)
			}
		})
	}
}

func TestAntigravitySessionStartWritesInjectionOrNoOpJSON(t *testing.T) {
	m, _ := loadManifest("agy")
	spec, _ := m.event("SessionStart")
	out, err := present(spec, wire.HookResult{Enrollment: "joined", Hint: "member rules"})
	if err != nil || string(out) != "{\"injectSteps\":[{\"systemMessage\":{\"systemMessage\":\"member rules\"}}]}\n" {
		t.Fatalf("startup output: %q, %v", out, err)
	}
	var noOp strings.Builder
	Run(time.Now(), []string{"agy", "SessionStart"}, strings.NewReader("{}"), &noOp, func() (Paths, error) {
		return Paths{}, errors.New("no dispatcher")
	})
	if noOp.String() != "{}\n" {
		t.Fatalf("no-op output: %q", noOp.String())
	}
}
