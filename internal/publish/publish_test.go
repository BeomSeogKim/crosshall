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
	if claude.SessionThread("s") != "" || codex.SessionThread("thread-1") != "thread-1" {
		t.Error("only a Codex session reference is its thread")
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
	if _, ok := codex.Notice([]int64{1}); ok {
		t.Error("codex has a notice before trusted capture is shown")
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

func (fixture) Resolve(r Route) (string, string)  { return r.ThreadID, "" }
func (fixture) ModeEligible(string) bool          { return true }
func (fixture) AcceptsSocket(string) bool         { return false }
func (fixture) SessionThread(ref string) string   { return ref }
func (fixture) Notice(ids []int64) (string, bool) { return "fixture " + notice.IDList(ids), true }
func (fixture) Echo(prompt string) (string, bool) { return strings.CutPrefix(prompt, "fixture ") }
func (f fixture) Transport() Transport            { return f.transport }

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
