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
	enrollForRecovery(t, claimed, "startup", 1)
	enrollForRecovery(t, claimed, "resume", 2)
	enrollForRecovery(t, unclaimed, "startup", 1)
	if !rosterHas(t, writer, "claude1") || !rosterHas(t, writer, "claude2") {
		t.Fatal("both synthetic Claude sessions must be enrolled")
	}
	w.kill()

	// Model the crash boundary by staging one end in work and one in in.
	// Both packets have the normal 150 ms deadline and expire before recovery.
	workResponse, workExpiry := queueCleanEnd(t, claimed, "work", "claimed-end")
	inResponse, inExpiry := queueCleanEnd(t, unclaimed, "in", "unclaimed-end")
	time.Sleep(time.Until(time.UnixMilli(max(workExpiry, inExpiry))))
	// The restarted dispatcher publishes ready after its initial scan.
	w.owner().must("owner", "start")

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
		leaf, packet := filepath.Dir(filepath.Dir(response)), filepath.Base(response)
		for _, area := range []string{"in", "work", "out"} {
			path := filepath.Join(leaf, area, packet)
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("one-way observation left a packet at %s: %v", path, err)
			}
		}
	}
}

func enrollForRecovery(t *testing.T, c claudeSession, source string, starts int) {
	t.Helper()
	digest := (ident.Identity{Vendor: "claude", Key: c.key}).Digest()
	db := c.w.db()
	c.fireUntil("SessionStart", map[string]any{"source": source}, "the expected committed launches and a live binding", func(string) bool {
		var total int
		var running bool
		err := db.QueryRow(`SELECT COUNT(*), COALESCE(MAX(a.ended_hook_at_ms IS NULL), 0) FROM leaf l
			JOIN binding b ON b.principal_id = l.principal_id AND b.state = 'live'
			JOIN adapter_launch a ON a.principal_id = l.principal_id
			WHERE l.digest = ? AND a.verified = 1`, digest).Scan(&total, &running)
		if err != nil {
			t.Fatal(err)
		}
		return total >= starts && running
	})
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
		JOIN adapter_policy p ON p.adapter = l.vendor WHERE l.digest = ? AND a.ended_hook_at_ms IS NULL
		ORDER BY a.started_hook_at_ms DESC, a.rowid DESC LIMIT 1`, digest).
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
