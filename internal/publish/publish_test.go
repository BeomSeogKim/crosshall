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
	"github.com/BeomSeogKim/crosshall/internal/proc"
	"os/exec"
	"time"
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
	launch := Route{Mode: ModeDaemon, Host: Queue{PID: 7, StartMS: 9}, Home: "/home/u/.codex"}
	want := launch
	want.ThreadID = "thread-1"
	if claude.SessionRoute("s", launch) != (Route{}) || codex.SessionRoute("thread-1", launch) != want {
		t.Error("only a Codex session reference is its thread, with the launch's mode, server and home")
	}
	if claude.ThreadRouted() || !codex.ThreadRouted() {
		t.Error("only Codex routes a thread on its server")
	}
}

// A Codex thread is reachable only through the managed server its start
// named: the owner reads why otherwise, ahead of the thread's address
// (#236).
func TestCodexReachesOnlyADaemonThread(t *testing.T) {
	codex, _ := Shipped().Lookup("codex")
	claude, _ := Shipped().Lookup("claude")
	server := Queue{PID: 7, StartMS: 9}
	for mode, want := range map[string]string{
		ModeDaemon: "", ModeInProcess: CodexNoDaemon, "unverified": CodexDaemonUnverified, "": CodexDaemonUnverified,
	} {
		if got := codex.Reach(Route{ThreadID: "thread-1", Mode: mode, Host: server, Home: "/h"}); got != want {
			t.Errorf("reach of a %q thread = %q, want %q", mode, got, want)
		}
	}
	for _, r := range []Route{{Host: Queue{}, Home: "/h"}, {Host: Queue{PID: 7}, Home: "/h"}, {Host: Queue{StartMS: 9}, Home: "/h"},
		{Host: server}, {Host: server, Home: "relative"}} {
		r.ThreadID, r.Mode = "thread-1", ModeDaemon
		if got := codex.Reach(r); got != CodexDaemonUnverified {
			t.Errorf("reach of a daemon thread with server %+v in %q = %q", r.Host, r.Home, got)
		}
	}
	if claude.Reach(Route{}) != "" {
		t.Error("a Claude route has a reach reason")
	}
	sending := Shipped().WithTransport("codex", Transport{Send: func(context.Context, Target, string) Outcome { return Submitted }})
	if address, why := sending.Address("codex", Route{ThreadID: "thread-1", Mode: ModeInProcess}); address != "" || why != CodexNoDaemon {
		t.Errorf("in-process address = %q, %q", address, why)
	}
	if address, why := sending.Address("codex", Route{ThreadID: "thread-1", Mode: ModeDaemon, Host: server, Home: "/h"}); address != "thread-1" || why != "" {
		t.Errorf("daemon address = %q, %q", address, why)
	}
	if _, why := Shipped().Address("codex", Route{ThreadID: "thread-1", Mode: ModeDaemon, Host: server, Home: "/h"}); why != "no sender for codex" {
		t.Errorf("shipped codex address reason = %q", why)
	}
}

// The Codex notice names its IDs and its sender, says to read and answer,
// and only that notice, or its rc.4 text, echoes back as a receipt.
func TestCodexNoticeEchoesItsIDs(t *testing.T) {
	codex, _ := Shipped().Lookup("codex")
	line, ok := codex.Notice([]int64{3, 17})
	want := "[crosshall] pending: 3 17. This notice is from crosshall, not from a Codex session. Run crosshall read, then use crosshall reply to answer or decline each request it shows, as the member rules say."
	if !ok || line != want {
		t.Fatalf("notice = %q, %v", line, ok)
	}
	rc4 := "[crosshall] pending: 3 17. This notice is from crosshall, not from a Codex session: do not reply to the sender. Run crosshall read."
	for _, prompt := range []string{line, rc4} {
		if ids, ok := codex.Echo(prompt); !ok || ids != "3 17" {
			t.Fatalf("echo %q = %q, %v", prompt, ids, ok)
		}
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

func (fixture) Resolve(r Route) (string, string) { return r.ThreadID, "" }
func (fixture) ModeEligible(string) bool         { return true }
func (fixture) AcceptsSocket(string) bool        { return false }
func (fixture) SessionRoute(ref string, launch Route) Route {
	return Route{ThreadID: ref, Mode: launch.Mode}
}
func (fixture) ThreadRouted() bool                { return false }
func (fixture) Reach(Route) string                { return "" }
func (fixture) BackgroundWakes() bool             { return true }
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

// The Codex transport proves the server a session's start named: the same
// process, run as the managed server from the release tree of the home
// setup installed the hooks into. It has no sender yet.
func TestCodexProvesTheServerItsStartNamed(t *testing.T) {
	tr := Shipped()["codex"].Transport()
	if tr.Prove == nil || tr.Send != nil {
		t.Fatal("the Codex transport must prove and must not send")
	}
	// The stand-in server is this test binary, which waits in
	// TestCodexServerStandIn when the environment asks it to.
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// installed copies it into a home's release tree, as Codex installs
	// its managed server there.
	installed := func(home string) string {
		t.Helper()
		exe := filepath.Join(home, "packages", "app-server-daemon", "releases", "0.171.0-aarch64-apple-darwin", "bin", "codex")
		raw, err := os.ReadFile(self)
		if err == nil {
			err = os.MkdirAll(filepath.Dir(exe), 0o700)
		}
		if err == nil {
			err = os.WriteFile(exe, raw, 0o700)
		}
		if err != nil {
			t.Fatal(err)
		}
		return exe
	}
	run := func(exe string, args ...string) Queue {
		t.Helper()
		cmd := exec.Command(exe, append([]string{"-test.run=^TestCodexServerStandIn$", "--"}, args...)...)
		cmd.Env = append(os.Environ(), codexStandIn+"=1")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
		start, err := proc.StartOf(cmd.Process.Pid)
		if err != nil {
			t.Skipf("process start unavailable: %v", err)
		}
		return Queue{PID: int64(cmd.Process.Pid), StartMS: start.Identity}
	}
	home, _ := filepath.EvalSymlinks(t.TempDir())
	other, _ := filepath.EvalSymlinks(t.TempDir())
	link := filepath.Join(t.TempDir(), "home")
	if err := os.Symlink(home, link); err != nil {
		t.Fatal(err)
	}
	// Each home's release is installed once: rewriting the file of a
	// running stand-in could end it.
	homeExe, otherExe := installed(home), installed(other)
	daemon := []string{"app-server", "--listen", "unix://", "--managed-daemon"}
	server := run(homeExe, daemon...)
	probe := func(host Queue, routeHome, installedHome string) Probe {
		return Probe{Target: Target{Adapter: "codex", Address: "thread-1"},
			Route:         Route{ThreadID: "thread-1", Mode: ModeDaemon, Host: host, Home: routeHome},
			InstalledHome: installedHome}
	}
	for name, installedHome := range map[string]string{"the installed home": home, "the installed home through a link": link} {
		proven, ok := tr.Prove(t.Context(), probe(server, home, installedHome))
		if !ok || proven.Queue != server || proven.StartedAtMS <= 0 {
			t.Fatalf("managed server of %s: %+v, %v", name, proven, ok)
		}
	}
	for name, p := range map[string]Probe{
		"another start of the PID":            probe(Queue{PID: server.PID, StartMS: server.StartMS + 1}, home, home),
		"no server named":                     probe(Queue{}, home, home),
		"a server without the daemon":         probe(run(homeExe, "app-server", "--listen", "unix://"), home, home),
		"a process that is not a server":      probe(run(homeExe), home, home),
		"no home installed":                   probe(server, home, ""),
		"a route of another home":             probe(server, other, home),
		"a daemon of another installation":    probe(run(otherExe, daemon...), other, home),
		"another home's daemon on this route": probe(run(otherExe, daemon...), home, home),
		"a daemon outside any release tree":   probe(run(self, daemon...), home, home),
	} {
		if proven, ok := tr.Prove(t.Context(), p); ok {
			t.Errorf("%s proved: %+v", name, proven)
		}
	}
}

const codexStandIn = "CROSSHALL_CODEX_SERVER_STAND_IN"

// TestCodexServerStandIn is the stand-in managed server of
// TestCodexProvesTheServerItsStartNamed: it waits until killed.
func TestCodexServerStandIn(t *testing.T) {
	if os.Getenv(codexStandIn) != "1" {
		t.Skip("runs only as a stand-in server")
	}
	time.Sleep(30 * time.Second)
}
