package publish

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestShippedAdaptersKeepTheirRouteRules(t *testing.T) {
	claude, _ := Lookup("claude")
	codex, _ := Lookup("codex")
	if ok, why := claude.Complete(Route{}); ok || why != "no socket" {
		t.Fatalf("claude without a socket: %t %q", ok, why)
	}
	if ok, why := codex.Complete(Route{SocketPath: "/tmp/cc-socks/1.sock"}); ok || why != "no thread" {
		t.Fatalf("codex without a thread: %t %q", ok, why)
	}
	if claude.ModeEligible("bypassPermissions") || claude.ModeEligible("bypass") || !claude.ModeEligible("default") {
		t.Fatal("claude bypass modes must be on pull")
	}
	if !codex.ModeEligible("bypassPermissions") {
		t.Fatal("codex has no mode rule")
	}
	for path, want := range map[string]bool{
		"/tmp/cc-socks/1.sock": true, "/tmp/cc-socks/../x.sock": false, "/tmp/other/1.sock": false,
		"/tmp/cc-socks/.sock": false, "/tmp/cc-socks/a b.sock": false, "relative.sock": false,
	} {
		if got := claude.AcceptsSocket(path); got != want {
			t.Errorf("claude accepts %q: %t, want %t", path, got, want)
		}
	}
	if claude.SessionThread("s") != "" || codex.SessionThread("thread-1") != "thread-1" {
		t.Error("only a Codex session reference is its thread")
	}
	if codex.AcceptsSocket("/tmp/cc-socks/1.sock") {
		t.Error("codex accepted a socket")
	}
	if _, ok := Lookup("agy"); ok {
		t.Error("an adapter without a decision is registered")
	}
}

func TestOnlyClaudeHasANoticeAndNothingSendsYet(t *testing.T) {
	if got := Shipped().Publishing(); !slices.Equal(got, []string{"claude"}) {
		t.Fatalf("publishing adapters %v", got)
	}
	if Shipped().Sends() {
		t.Fatal("a shipped adapter has a transport before its sender is built")
	}
	if line, ok := Shipped()["claude"].Notice([]int64{3, 7}); !ok || !strings.HasPrefix(line, "[crosshall] pending: 3 7. ") {
		t.Fatalf("claude notice %q", line)
	}
	send := func(context.Context, Target, string) Outcome { return Submitted }
	with := Shipped().WithTransport("claude", Transport{Send: send})
	if !with.Sends() || Shipped().Sends() {
		t.Fatal("WithTransport must change only its copy")
	}
	if ok, _ := with["claude"].Complete(Route{}); ok {
		t.Fatal("WithTransport changed the route rules")
	}
}

// The publish core names no vendor: what is vendor-specific comes through
// an Adapter (principle 7).
func TestThePublishCoreNamesNoVendor(t *testing.T) {
	for _, file := range []string{
		"../domain/publish_attempt.go", "../domain/publish_route.go", "../domain/publish_receipt.go",
		"../domain/publish_switch.go", "../dispatch/publisher.go", "../store/publish_status.go",
	} {
		data, err := os.ReadFile(filepath.FromSlash(file))
		if err != nil {
			t.Fatal(err)
		}
		for _, vendor := range []string{`"claude"`, `'claude'`, `"codex"`, `'codex'`, "notice.Claude("} {
			if strings.Contains(string(data), vendor) {
				t.Errorf("%s names %s", file, vendor)
			}
		}
	}
}
