package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/spool"
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
	req := makeRequest(time.UnixMilli(1000), m, spec, e, enrolment{}, true, 1)
	var enrollArgs wire.HookEnrollArgs
	if err := json.Unmarshal(req.Args, &enrollArgs); err != nil {
		t.Fatal(err)
	}
	if enrollArgs.Profile.Mode != "print" {
		t.Fatalf("runtime profile mode = %q, want print", enrollArgs.Profile.Mode)
	}

	routeReq, ok := makeRouteRequest(time.UnixMilli(1000), m, e, 1)
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
	if _, ok := makeRouteRequest(time.UnixMilli(2000), m, ne, 1); ok {
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
	req := makeRequest(time.UnixMilli(1000), m, spec, e, enrolment{}, true, 1)
	var args wire.HookEnrollArgs
	if err := json.Unmarshal(req.Args, &args); err != nil {
		t.Fatal(err)
	}
	if args.LaunchVerified {
		t.Fatalf("got %+v", args)
	}
}

type contendedRouteTransport struct {
	t      *testing.T
	stdout *bytes.Buffer
	reply  wire.HookResult
}

func (c contendedRouteTransport) HookLookup(context.Context, ident.Identity, string) (string, wire.Response, error) {
	return "", wire.Response{}, nil
}

func (c contendedRouteTransport) HookCall(ctx context.Context, _ ident.Identity, _ string, req wire.Request, _ bool) (spool.Reply, error) {
	if req.Verb == wire.VerbHookRoute {
		if !strings.Contains(c.stdout.String(), "[crosshall] pending: 42") {
			c.t.Fatalf("base output not written before optional hook.route: %q", c.stdout.String())
		}
		return spool.Reply{}, context.DeadlineExceeded
	}
	data, _ := json.Marshal(c.reply)
	return spool.Reply{Response: wire.Response{Status: core.ExitOK, Data: data}}, nil
}

func TestHookRoutePreserves128ByteEventRefAndBaseOutputUnderContention(t *testing.T) {
	m, _ := loadManifest("claude")
	spec, _ := m.event("UserPromptSubmit")
	turnRef := strings.Repeat("x", 111) // 111 + ":UserPromptSubmit" (17) = 128 bytes
	input := `{"hook_event_name":"UserPromptSubmit","session_id":"sess-1","prompt_id":"` + turnRef + `","permission_mode":"default"}`
	lookup := func(name string) (string, bool) {
		switch name {
		case "CLAUDE_CODE_SESSION_ID":
			return "sess-1", true
		case "CLAUDE_CODE_ENTRYPOINT":
			return "cli", true
		}
		return "", false
	}
	e, err := parseEvent(strings.NewReader(input), m, spec, lookup)
	if err != nil {
		t.Fatalf("parseEvent: %v", err)
	}
	routeReq, ok := makeRouteRequest(time.UnixMilli(1000), m, e, 1)
	if !ok {
		t.Fatal("expected route request")
	}
	var routeArgs wire.HookRouteArgs
	if err := json.Unmarshal(routeReq.Args, &routeArgs); err != nil {
		t.Fatal(err)
	}
	// The prompt's reference carries its entry time; past 128 bytes it is
	// the same digest the prompt's own request carries.
	if want := wire.PromptEventRef(e.EventRef, 1000); routeArgs.EventRef != want || len(want) > 128 {
		t.Fatalf("EventRef = %q, want %q", routeArgs.EventRef, want)
	}
	if !strings.HasPrefix(routeReq.OpKey, "hook:route:") {
		t.Fatalf("OpKey = %q, want hook:route: prefix", routeReq.OpKey)
	}

	// Set up rendezvous + leaf + policy + ready marker so dispatchEvent runs base output + contended route.
	rvDir := t.TempDir()
	id := ident.Identity{Vendor: "claude", Key: "sess-1"}
	leafDir := filepath.Join(rvDir, "m", id.Digest()[:16])
	if err := os.MkdirAll(leafDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(leafDir, "gen"), []byte("gen-1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rvDir, "policy"), []byte("claude 1 0 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	readyJSON, _ := json.Marshal(map[string]any{"v": 1, "instance": "i1", "pid": os.Getpid()})
	if err := os.WriteFile(filepath.Join(rvDir, "ready"), readyJSON, 0600); err != nil {
		t.Fatal(err)
	}
	rv, err := os.OpenRoot(rvDir)
	if err != nil {
		t.Fatal(err)
	}
	defer rv.Close()

	var stdout bytes.Buffer
	outcome := dispatchEvent(t.Context(), time.Now(), rv, contendedRouteTransport{
		t:      t,
		stdout: &stdout,
		reply:  wire.HookResult{Epoch: 1, PendingLine: "[crosshall] pending: 42"},
	}, []string{"claude", "UserPromptSubmit"}, strings.NewReader(input), &stdout, lookup)
	if outcome != "rpc" {
		t.Fatalf("outcome = %q, want rpc", outcome)
	}
	if !strings.Contains(stdout.String(), "[crosshall] pending: 42") {
		t.Fatalf("base pending output dropped under route contention: %q", stdout.String())
	}
}

// #180: a turn's callbacks report the vendor's socket too, and a start
// reports even a missing one, marked as a start.
func TestClaudeTurnCallbacksReportTheSocketAndAStartReportsItsAbsence(t *testing.T) {
	m, ok := loadManifest("claude")
	if !ok {
		t.Fatal("missing claude manifest")
	}
	withSocket := func(name string) (string, bool) {
		switch name {
		case "CLAUDE_CODE_SESSION_ID":
			return "sess-1", true
		case "CLAUDE_CODE_MESSAGING_SOCKET":
			return "/tmp/cc-socks/12345.sock", true
		}
		return "", false
	}
	withoutSocket := func(name string) (string, bool) {
		if name == "CLAUDE_CODE_MESSAGING_SOCKET" {
			return "", false
		}
		return withSocket(name)
	}
	route := func(name, input string, lookup func(string) (string, bool)) (wire.HookRouteArgs, bool) {
		t.Helper()
		spec, _ := m.event(name)
		e, err := parseEvent(strings.NewReader(input), m, spec, lookup)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		req, ok := makeRouteRequest(time.UnixMilli(1000), m, e, 1)
		var args wire.HookRouteArgs
		if ok {
			if err := json.Unmarshal(req.Args, &args); err != nil {
				t.Fatal(err)
			}
		}
		return args, ok
	}
	for _, c := range []struct{ name, input string }{
		{"UserPromptSubmit", `{"hook_event_name":"UserPromptSubmit","session_id":"sess-1","prompt_id":"p1"}`},
		{"Stop", `{"hook_event_name":"Stop","session_id":"sess-1","prompt_id":"p1","stop_hook_active":false}`},
	} {
		if args, ok := route(c.name, c.input, withSocket); !ok || args.SocketPath != "/tmp/cc-socks/12345.sock" || args.Start {
			t.Fatalf("%s: route %v %+v, want the socket, not as a start", c.name, ok, args)
		}
	}
	start := `{"hook_event_name":"SessionStart","session_id":"sess-1","source":"startup"}`
	if args, ok := route("SessionStart", start, withoutSocket); !ok || args.SocketPath != "" || !args.Start {
		t.Fatalf("a start without the socket: route %v %+v, want a start report", ok, args)
	}
	if args, ok := route("SessionStart", start, withSocket); !ok || args.SocketPath == "" || !args.Start {
		t.Fatalf("a start with the socket: route %v %+v", ok, args)
	}
	notification := `{"hook_event_name":"Notification","session_id":"sess-1","notification_type":"permission_prompt"}`
	if _, ok := route("Notification", notification, withSocket); ok {
		t.Fatal("a notification reported a route")
	}
}

// #180: a prompt typed while its turn runs reports that turn's id; its
// route report carries the hook's entry time like its own request, so it
// is not taken for a repeat of the turn's first report.
func TestClaudePromptRouteReportsCarryTheEntryTime(t *testing.T) {
	m, ok := loadManifest("claude")
	if !ok {
		t.Fatal("missing claude manifest")
	}
	spec, _ := m.event("UserPromptSubmit")
	lookup := func(name string) (string, bool) {
		switch name {
		case "CLAUDE_CODE_SESSION_ID":
			return "sess-1", true
		case "CLAUDE_CODE_MESSAGING_SOCKET":
			return "/tmp/cc-socks/12345.sock", true
		}
		return "", false
	}
	e, err := parseEvent(strings.NewReader(`{"hook_event_name":"UserPromptSubmit","session_id":"sess-1","prompt_id":"p1"}`), m, spec, lookup)
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]bool{}
	for _, at := range []int64{1000, 2000, 2000} {
		req, ok := makeRouteRequest(time.UnixMilli(at), m, e, 1)
		var args wire.HookRouteArgs
		if !ok || json.Unmarshal(req.Args, &args) != nil {
			t.Fatalf("no route report at %d", at)
		}
		base := makeRequest(time.UnixMilli(at), m, spec, e, enrolment{}, true, 1)
		var event struct {
			EventRef string `json:"event_ref"`
		}
		if err := json.Unmarshal(base.Args, &event); err != nil || args.EventRef != event.EventRef {
			t.Fatalf("at %d the route report's ref %q, the prompt's %q", at, args.EventRef, event.EventRef)
		}
		keys[req.OpKey] = true
	}
	if len(keys) != 2 {
		t.Fatalf("route report keys %v, want one per hook entry time", keys)
	}
}
