//go:build e2e

package e2e

import "testing"

// A channel created with the default pool lets each member's first join open
// an exchange without an owner grant; the owner's status shows what is left.
func TestJoinPoolOpensTheFirstExchangeEndToEnd(t *testing.T) {
	w := newWorld(t)
	owner := w.owner()
	owner.must("owner", "start")
	if created := owner.must("owner", "channel", "create", "demo"); num(t, created, "join_pool") != 8 {
		t.Fatalf("channel create: %s", created.stdout)
	}
	writer, reviewer := w.member(), w.member()
	writer.must("join", "demo", "--as", "writer")
	reviewer.must("join", "demo", "--as", "reviewer")
	if ask := writer.run("ask", "@reviewer", "review?"); ask.exit != 0 {
		t.Fatalf("first ask without a grant: exit %d %s%s", ask.exit, ask.stdout, ask.stderr)
	}
	status := owner.must("owner", "status", "--channel", "demo")
	channels, _ := status.obj["channels"].([]any)
	if len(channels) != 1 || channels[0].(map[string]any)["join_pool"] != float64(6) {
		t.Fatalf("owner status: %s", status.stdout)
	}
	owner.must("owner", "budget", "grant", "demo", "--pool", "2")
	if status := owner.must("owner", "status", "--channel", "demo"); status.obj["channels"].([]any)[0].(map[string]any)["join_pool"] != float64(8) {
		t.Fatalf("after a pool grant: %s", status.stdout)
	}
}
