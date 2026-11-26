//go:build e2e

package e2e

import (
	"bytes"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

// A wait on a linked device forwards each poll to the home as one short
// call (#400, 12l): the home sees the member waiting, the wait ends on the
// home's ask, and with three waits running an ask from the same device
// still answers in its normal time.
func TestSpokeWaitPollsTheHomeWithoutHoldingCommands(t *testing.T) {
	f := newLinkFixture(t, binary, binary)
	home, spoke := f.home.owner(), f.spoke.owner()
	home.must("owner", "start")
	spoke.must("owner", "start")
	home.must("owner", "channel", "create", "checkout")
	spoke.must("link", linkFixtureTarget, "--name", "fixture-spoke", "--yes")
	waitSpokeLink(t, spoke, "connected")
	writer := f.home.member()
	writer.must("join", "checkout", "--as", "writer")
	remote := f.spoke.member()
	remote.must("join", "checkout", "--as", "remote")
	var waiters []*participant
	for i := 1; i <= 3; i++ {
		w := f.spoke.member()
		w.must("join", "checkout", "--as", fmt.Sprintf("waiter%d", i))
		waiters = append(waiters, w)
	}

	askTime := func() time.Duration {
		t.Helper()
		start := time.Now()
		remote.must("ask", "@writer", "a question from the device")
		return time.Since(start)
	}
	median := func(n int) time.Duration {
		t.Helper()
		var d []time.Duration
		for range n {
			d = append(d, askTime())
		}
		slices.Sort(d)
		return d[n/2]
	}
	normal := median(5)

	type running struct {
		cmd *exec.Cmd
		out *bytes.Buffer
	}
	var waits []running
	for _, w := range waiters {
		cmd := exec.Command(f.spoke.binaryPath(), "--json", "wait", "--work-only", "--timeout", "60s")
		cmd.Env, cmd.Dir = w.env(), f.spoke.home
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
		waits = append(waits, running{cmd, &out})
	}
	waiting := func(alias string) bool {
		for _, m := range writer.must("who").obj["members"].([]any) {
			row, _ := m.(map[string]any)
			if row["alias"] == alias && row["wake"] == "wait" {
				return true
			}
		}
		return false
	}
	eventually(t, "the home sees the device's waits", func() bool {
		return waiting("waiter1") && waiting("waiter2") && waiting("waiter3")
	})
	// Same device, three waits polling: an ask answers in its normal time.
	busy := median(5)
	if limit := normal*3/2 + 200*time.Millisecond; busy > limit {
		t.Fatalf("an ask took %v with three waits running, %v without (limit %v)", busy, normal, limit)
	}
	t.Logf("ask median: %v alone, %v with three waits", normal, busy)
	// The home's ask ends the first wait; the others keep waiting.
	id := num(t, writer.must("ask", "@waiter1", "work for the device"), "id")
	done := make(chan error, 1)
	go func() { done <- waits[0].cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil || !strings.Contains(waits[0].out.String(), fmt.Sprint(id)) {
			t.Fatalf("the wait ended with %v: %s", err, waits[0].out.String())
		}
	case <-time.After(fixtureEventTimeout):
		t.Fatalf("the wait did not end on the home's ask %d: %s", id, waits[0].out.String())
	}
	if !waiting("waiter2") || !waiting("waiter3") {
		t.Fatal("the other waits stopped")
	}
}
