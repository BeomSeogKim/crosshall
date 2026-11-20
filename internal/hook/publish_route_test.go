package hook

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/wire"
)

func TestClaudeHookKeepsRuntimeProfileSeparateAndBuildsRouteRequest(t *testing.T) {
	m, ok := loadManifest("claude")
	if !ok {
		t.Fatal("missing claude manifest")
	}
	spec, ok := m.event("SessionStart")
	if !ok {
		t.Fatal("missing SessionStart")
	}
	input := `{"hook_event_name":"SessionStart","session_id":"sess-1","source":"startup","permission_mode":"bypassPermissions"}`
	lookup := func(name string) (string, bool) {
		switch name {
		case "CLAUDE_CODE_SESSION_ID":
			return "sess-1", true
		case "CLAUDE_CODE_MESSAGING_SOCKET":
			return "/tmp/cc-socks/12345.sock", true
		case "CLAUDE_CODE_ENTRYPOINT":
			return "sdk-cli", true
		}
		return "", false
	}
	e, err := parseEvent(strings.NewReader(input), m, spec, lookup)
	if err != nil {
		t.Fatalf("parseEvent: %v", err)
	}
	req := makeRequest(time.UnixMilli(1000), m, spec, e, "", true, 1)
	var enrollArgs wire.HookEnrollArgs
	if err := json.Unmarshal(req.Args, &enrollArgs); err != nil {
		t.Fatal(err)
	}
	if enrollArgs.Profile.Mode != "print" {
		t.Fatalf("runtime profile mode = %q, want print", enrollArgs.Profile.Mode)
	}

	routeReq, ok := makeRouteRequest(time.UnixMilli(1000), m, spec, e, 1)
	if !ok || routeReq.Verb != wire.VerbHookRoute {
		t.Fatalf("makeRouteRequest: ok=%v verb=%q", ok, routeReq.Verb)
	}
	var routeArgs wire.HookRouteArgs
	if err := json.Unmarshal(routeReq.Args, &routeArgs); err != nil {
		t.Fatal(err)
	}
	if routeArgs.SocketPath != "/tmp/cc-socks/12345.sock" || routeArgs.PermissionMode != "bypassPermissions" {
		t.Fatalf("got %+v", routeArgs)
	}

	// Field-less Notification produces no route request.
	notifSpec, _ := m.event("Notification")
	notifInput := `{"hook_event_name":"Notification","session_id":"sess-1","notification_type":"permission_prompt"}`
	ne, err := parseEvent(strings.NewReader(notifInput), m, notifSpec, lookup)
	if err != nil {
		t.Fatalf("parseEvent notification: %v", err)
	}
	if _, ok := makeRouteRequest(time.UnixMilli(2000), m, notifSpec, ne, 1); ok {
		t.Fatal("field-less notification unexpectedly produced a route request")
	}
}

func TestCodexHookExtractsThreadAndSessionLaunch(t *testing.T) {
	m, ok := loadManifest("codex")
	if !ok {
		t.Fatal("missing codex manifest")
	}
	spec, ok := m.event("SessionStart")
	if !ok {
		t.Fatal("missing SessionStart")
	}
	input := `{"hook_event_name":"SessionStart","session_id":"thread-42","source":"startup"}`
	lookup := func(name string) (string, bool) {
		if name == "CODEX_THREAD_ID" {
			return "thread-42", true
		}
		return "", false
	}
	e, err := parseEvent(strings.NewReader(input), m, spec, lookup)
	if err != nil {
		t.Fatalf("parseEvent: %v", err)
	}
	req := makeRequest(time.UnixMilli(1000), m, spec, e, "", true, 1)
	var args wire.HookEnrollArgs
	if err := json.Unmarshal(req.Args, &args); err != nil {
		t.Fatal(err)
	}
	if args.LaunchVerified {
		t.Fatalf("got %+v", args)
	}
}
