package spool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

func TestHookCallAdmitsOneWayHookRoute(t *testing.T) {
	rv := t.TempDir()
	if err := os.Chmod(rv, 0700); err != nil {
		t.Fatal(err)
	}
	id := ident.Identity{Vendor: "claude", Key: "sess-1", Version: "2.1.327"}
	leafDir := filepath.Join(rv, "m", id.Digest()[:16])
	for _, sub := range []string{"in", "out"} {
		if err := os.MkdirAll(filepath.Join(leafDir, sub), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(leafDir, "gen"), []byte("gen-1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	client := Client{Rendezvous: rv}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	args, _ := json.Marshal(wire.HookRouteArgs{
		Adapter: "claude", LaunchRef: "pid-1", EventRef: "ev-1", HookAtMS: 1000, Epoch: 1, SocketPath: "/tmp/cc-socks/1.sock",
	})
	req := wire.Request{OpKey: "hook:route:claude:pid-1:ev-1", Verb: wire.VerbHookRoute, Args: args}

	if _, err := client.HookCall(ctx, id, "gen-1", req, false); err == nil {
		t.Fatal("HookCall accepted non-one-way hook.route")
	}
	reply, err := client.HookCall(ctx, id, "gen-1", req, true)
	if err != nil {
		t.Fatalf("HookCall one-way hook.route: %v", err)
	}
	if reply.OpKey != req.OpKey {
		t.Fatalf("opKey = %q, want %q", reply.OpKey, req.OpKey)
	}
}
