//go:build e2e

package e2e

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

func TestM2OneWayObservationRecovery(t *testing.T) {
	w, writer, claimed := m2World(t)
	unclaimed := claimed
	unclaimed.key = "synthetic-unclaimed-session"
	claimed.fire("SessionStart", map[string]any{"source": "startup"})
	unclaimed.fire("SessionStart", map[string]any{"source": "startup"})
	if !rosterHas(t, writer, "claude1") || !rosterHas(t, writer, "claude2") {
		t.Fatal("both synthetic Claude sessions must be enrolled")
	}
	w.kill()

	// Model the crash boundary by staging one end in work and one in in.
	// Both packets have the normal 150 ms deadline and expire before recovery.
	workResponse, workExpiry := queueCleanEnd(t, claimed, "work", "claimed-end")
	inResponse, inExpiry := queueCleanEnd(t, unclaimed, "in", "unclaimed-end")
	time.Sleep(time.Until(time.UnixMilli(max(workExpiry, inExpiry))))
	w.owner().must("owner", "start")
	claimed.settle()
	unclaimed.settle()

	if rosterHas(t, writer, "claude1") {
		t.Error("the recovered clean end did not depart its member")
	}
	if !rosterHas(t, writer, "claude2") {
		t.Error("the unclaimed expired end departed its member")
	}
	var workCount, inCount int
	if err := w.db().QueryRow(`SELECT
		(SELECT COUNT(*) FROM observation WHERE event_ref = 'claimed-end'),
		(SELECT COUNT(*) FROM observation WHERE event_ref = 'unclaimed-end')`).Scan(&workCount, &inCount); err != nil {
		t.Fatal(err)
	}
	if workCount != 1 || inCount != 0 {
		t.Errorf("recorded %d recovered and %d unclaimed observations, want 1 and 0", workCount, inCount)
	}
	for _, response := range []string{workResponse, inResponse} {
		if _, err := os.Stat(response); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("one-way observation produced a response at %s: %v", response, err)
		}
	}
}

func queueCleanEnd(t *testing.T, c claudeSession, area, eventRef string) (string, int64) {
	t.Helper()
	digest := (ident.Identity{Vendor: "claude", Key: c.key}).Digest()
	dir := filepath.Join(c.w.rendezvous(), "m", digest[:16])
	var generation string
	var args wire.HookEventArgs
	var profile string
	err := c.w.db().QueryRow(`SELECT l.generation, a.launch_ref, a.profile, a.verified, p.epoch
		FROM leaf l JOIN adapter_launch a ON a.principal_id = l.principal_id
		JOIN adapter_policy p ON p.adapter = l.vendor WHERE l.digest = ?`, digest).
		Scan(&generation, &args.LaunchRef, &profile, &args.LaunchVerified, &args.Epoch)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(profile), &args.Profile); err != nil {
		t.Fatal(err)
	}
	if !args.LaunchVerified {
		t.Fatal("clean end requires a verified launch")
	}
	now := time.Now()
	args.EventRef, args.Fact, args.HookAtMS = eventRef, wire.HookSessionEnded, now.UnixMilli()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	expires := now.Add(150 * time.Millisecond).UnixMilli()
	reply := false
	request := wire.Request{
		Version: wire.Version, Attempt: wire.NewID(), Verb: wire.VerbHookEvent, Args: raw, Reply: &reply,
		OpKey: "hook:claude:" + args.LaunchRef + ":" + eventRef, Generation: generation, KeyDigest: digest,
		IssuedAtMS: now.UnixMilli(), ExpiresAtMS: expires,
	}
	packet, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	name := request.Attempt + ".json"
	if err := os.WriteFile(filepath.Join(dir, area, name), packet, 0600); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "out", name), expires
}
