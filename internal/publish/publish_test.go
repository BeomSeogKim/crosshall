package publish

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/hook/manifests"
	"github.com/BeomSeogKim/crosshall/internal/notice"
)

func TestShippedAdaptersKeepTheirRouteRules(t *testing.T) {
	claude, _ := Lookup("claude")
	codex, _ := Lookup("codex")
	const sock = "/tmp/cc-socks/1.sock"
	for _, c := range []struct {
		adapter      Adapter
		route        Route
		address, why string
	}{
		{claude, Route{}, "", "no socket"},
		{claude, Route{SocketPath: sock, Mode: "bypassPermissions"}, "", "bypass mode"},
		{claude, Route{SocketPath: sock, Mode: "bypass"}, "", "bypass mode"},
		{claude, Route{SocketPath: sock, Mode: "default"}, sock, ""},
		{codex, Route{SocketPath: sock}, "", "no thread"},
		{codex, Route{ThreadID: "thread-1", Mode: "bypassPermissions"}, "thread-1", ""},
	} {
		if address, why := c.adapter.Resolve(c.route); address != c.address || why != c.why {
			t.Errorf("%T resolves %+v to %q %q, want %q %q", c.adapter, c.route, address, why, c.address, c.why)
		}
	}
	for path, want := range map[string]bool{
		sock: true, "/tmp/cc-socks/../x.sock": false, "/tmp/other/1.sock": false,
		"/tmp/cc-socks/.sock": false, "/tmp/cc-socks/a b.sock": false, "relative.sock": false,
	} {
		if got := claude.AcceptsSocket(path); got != want {
			t.Errorf("claude accepts %q: %t, want %t", path, got, want)
		}
	}
	if codex.AcceptsSocket(sock) {
		t.Error("codex accepted a socket")
	}
	if claude.SessionRoute("s", "interactive") != (Route{}) ||
		codex.SessionRoute("thread-1", ModeDaemon) != (Route{ThreadID: "thread-1", Mode: ModeDaemon}) {
		t.Error("only a Codex session reference is its thread, with the launch's mode")
	}
}

// A Codex thread is reachable only through the managed server: the owner
// reads why otherwise, ahead of the thread's address (#236).
func TestCodexReachesOnlyADaemonThread(t *testing.T) {
	codex, _ := Shipped().Lookup("codex")
	claude, _ := Shipped().Lookup("claude")
	for mode, want := range map[string]string{
		ModeDaemon: "", ModeInProcess: CodexNoDaemon, "unverified": CodexDaemonUnverified, "": CodexDaemonUnverified,
	} {
		if got := codex.Reach(Route{ThreadID: "thread-1", Mode: mode}); got != want {
			t.Errorf("reach of a %q thread = %q, want %q", mode, got, want)
		}
	}
	if claude.Reach(Route{}) != "" {
		t.Error("a Claude route has a reach reason")
	}
	sending := Shipped().WithTransport("codex", Transport{Send: func(context.Context, Target, string) Outcome { return Submitted }})
	if address, why := sending.Address("codex", Route{ThreadID: "thread-1", Mode: ModeInProcess}); address != "" || why != CodexNoDaemon {
		t.Errorf("in-process address = %q, %q", address, why)
	}
	if address, why := sending.Address("codex", Route{ThreadID: "thread-1", Mode: ModeDaemon}); address != "thread-1" || why != "" {
		t.Errorf("daemon address = %q, %q", address, why)
	}
	if _, why := Shipped().Address("codex", Route{ThreadID: "thread-1", Mode: ModeDaemon}); why != "no sender for codex" {
		t.Errorf("shipped codex address reason = %q", why)
	}
}

// The Codex notice names its IDs and its sender, and only that notice
// echoes back as a receipt.
func TestCodexNoticeEchoesItsIDs(t *testing.T) {
	codex, _ := Shipped().Lookup("codex")
	line, ok := codex.Notice([]int64{3, 17})
	want := "[crosshall] pending: 3 17. This notice is from crosshall, not from a Codex session: do not reply to the sender. Run crosshall read."
	if !ok || line != want {
		t.Fatalf("notice = %q, %v", line, ok)
	}
	if ids, ok := codex.Echo(line); !ok || ids != "3 17" {
		t.Fatalf("echo = %q, %v", ids, ok)
	}
	claude, _ := Shipped().Lookup("claude")
	claudeLine, _ := claude.Notice([]int64{3})
	for _, prompt := range []string{claudeLine, "[crosshall] pending: 3", "run crosshall read"} {
		if _, ok := codex.Echo(prompt); ok {
			t.Errorf("codex echoed %q", prompt)
		}
	}
	if Shipped().Sends("codex") {
		t.Error("codex sends without a transport")
	}
}

func TestClaudeRecognizesOnlyItsOwnNotice(t *testing.T) {
	claude, _ := Lookup("claude")
	line, ok := claude.Notice([]int64{3, 7})
	if !ok || !strings.HasPrefix(line, "[crosshall] pending: 3 7. ") {
		t.Fatalf("claude notice %q", line)
	}
	if ids, ok := claude.Echo(line); !ok || notice.Receipt(ids) != "publish:3 7" {
		t.Fatalf("echo of its own notice: %q %t", ids, ok)
	}
	for _, prompt := range []string{"[crosshall] pending: 3 7", "pending: 3 7. x", "hello"} {
		if _, ok := claude.Echo(prompt); ok {
			t.Errorf("claude recognized %q", prompt)
		}
	}
	if ids, _ := claude.Echo(strings.Replace(line, "3 7", "7 3", 1)); notice.Receipt(ids) != "" {
		t.Error("the shared grammar accepted IDs out of order")
	}
	codex, _ := Lookup("codex")
	if codexLine, _ := codex.Notice([]int64{3, 7}); codexLine == line {
		t.Error("codex and claude notices read the same")
	} else if _, ok := claude.Echo(codexLine); ok {
		t.Error("claude recognized the codex notice")
	}
}

func TestOnlyClaudeSendsInTheShippedBuild(t *testing.T) {
	if got := Shipped().Senders(); !slices.Equal(got, []string{"claude"}) {
		t.Fatalf("shipped senders %v", got)
	}
	if _, why := Shipped().Address("codex", Route{ThreadID: "t"}); why != "no sender for codex" {
		t.Fatalf("codex route: %q", why)
	}
	if tr := Shipped()["claude"].Transport(); tr.Prove == nil || tr.Retired == nil {
		t.Fatal("the Claude sender cannot prove or retire its queue")
	}
}

// fixture is an adapter this build does not ship.
type fixture struct{ transport Transport }

func (fixture) Resolve(r Route) (string, string)    { return r.ThreadID, "" }
func (fixture) ModeEligible(string) bool            { return true }
func (fixture) AcceptsSocket(string) bool           { return false }
func (fixture) SessionRoute(ref, mode string) Route { return Route{ThreadID: ref, Mode: mode} }
func (fixture) Reach(Route) string                  { return "" }
func (fixture) Notice(ids []int64) (string, bool)   { return "fixture " + notice.IDList(ids), true }
func (fixture) Echo(prompt string) (string, bool)   { return strings.CutPrefix(prompt, "fixture ") }
func (f fixture) Transport() Transport              { return f.transport }

func TestTheRegistryAnswersFromOneAdapterAtATime(t *testing.T) {
	send := func(context.Context, Target, string) Outcome { return Submitted }
	r := Shipped().WithTransport("claude", Transport{}).With("fixture", fixture{Transport{Send: send}})
	if got := r.Senders(); !slices.Equal(got, []string{"fixture"}) {
		t.Fatalf("senders %v: one adapter's sender must not count for another", got)
	}
	if _, why := r.Address("claude", Route{SocketPath: "/tmp/cc-socks/1.sock"}); why != "no sender for claude" {
		t.Fatalf("claude in a registry where only the fixture sends: %q", why)
	}
	if address, why := r.Address("fixture", Route{ThreadID: "t"}); address != "t" || why != "" {
		t.Fatalf("fixture route: %q %q", address, why)
	}
	if r.Sends("fixture") == Shipped().Sends("fixture") {
		t.Fatal("With changed the shipped registry")
	}
	restore := Swap(r)
	if !Adapters().Sends("fixture") {
		t.Fatal("Swap did not take effect")
	}
	restore()
	if Adapters().Sends("fixture") {
		t.Fatal("restore did not take effect")
	}
}

func TestEveryPublishAdapterHasAHookManifest(t *testing.T) {
	for name := range Shipped() {
		if !manifests.Has(name) {
			t.Errorf("publish adapter %s has no hook manifest", name)
		}
	}
}

// The publish core and the adapter checks name no vendor: what is
// vendor-specific comes through an Adapter or a hook manifest (principle 7).
func TestThePublishCoreNamesNoVendor(t *testing.T) {
	for _, file := range []string{
		"../domain/publish_attempt.go", "../domain/publish_route.go", "../domain/publish_receipt.go",
		"../domain/publish_switch.go", "../domain/adapter_policy.go", "../dispatch/publisher.go",
		"../dispatch/server.go", "../dispatch/adapter_requests.go", "../store/publish_status.go",
		"../hook/manifest.go", "../notice/publish.go",
	} {
		data, err := os.ReadFile(filepath.FromSlash(file))
		if err != nil {
			t.Fatal(err)
		}
		for _, vendor := range []string{`"claude"`, `'claude'`, `"codex"`, `'codex'`, "Claude(", "ClaudeReceipt"} {
			if strings.Contains(string(data), vendor) {
				t.Errorf("%s names %s", file, vendor)
			}
		}
	}
}
