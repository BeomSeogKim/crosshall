package domain

import (
	"database/sql"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/publish"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

// A submitted notice on a thread lane is withdrawn when its turn would be
// stale: its route ended (it would run at the next resume) or nothing in
// it is pending any more (read by hand, offered in a prompt, or gone). A
// notice still due, or one on a process lane, is never listed.
func TestCodexWithdrawCandidates(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change string // applied after the notice was submitted
		reason string // empty: not a candidate
	}{
		{name: "still due"},
		{name: "read by hand", change: `UPDATE recipient SET picked_up_at_ms = 1 WHERE message_id = ?2`, reason: WithdrawNothingToRead},
		{name: "route ended", change: `UPDATE publish_route SET state = 'ended', reason = 'session ended' WHERE binding_id = ?1`, reason: WithdrawSessionEnded},
		{name: "route on another thread", change: `UPDATE publish_route SET thread_id = 'next-thread' WHERE binding_id = ?1`, reason: WithdrawSessionEnded},
		{name: "resumed in the same home", change: `UPDATE publish_route SET generation = generation + 2, host_pid = 77 WHERE binding_id = ?1`},
		{name: "already observed", change: `UPDATE publish_attempt SET observed_at_ms = 1, observation = 'own_turn' WHERE binding_id = ?1`},
		{name: "process lane", change: `UPDATE publish_attempt SET lane = 'process:42:1' WHERE binding_id = ?1;
			UPDATE recipient SET picked_up_at_ms = 1 WHERE message_id = ?2`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, writer := adapterChannel(t)
			f.policy("codex", true, 1)
			reader := f.newSession("codex")
			wantOK(t, f.join(reader, Join{Channel: "checkout", Alias: "reader"}, "reader-join"))
			f.hook(reader, codexStart("startup", publish.ModeDaemon, f.now.UnixMilli()))
			principal := f.principalOf(reader)
			bid := bindingIDOf(t, f, principal)
			ask := f.turnEndAsk(writer, "reader", "codex-withdraw")
			attempt := codexAttempt(t, f, bid, principal, []int64{ask})
			if tc.change != "" {
				if _, err := f.db.Exec(tc.change, bid, ask); err != nil {
					t.Fatal(err)
				}
			}
			var got []WithdrawCandidate
			f.inTx(func(tx store.Tx) (err error) {
				got, err = WithdrawCandidates(t.Context(), tx)
				return err
			})
			if tc.reason == "" {
				if len(got) != 0 {
					t.Fatalf("candidates %+v, want none", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("candidates %+v, want attempt %d", got, attempt)
			}
			c := got[0]
			var reservedAt int64
			if err := f.db.QueryRow(`SELECT reserved_at_ms FROM publish_attempt WHERE id = ?`, attempt).Scan(&reservedAt); err != nil {
				t.Fatal(err)
			}
			if c.ID != attempt || c.Reason != tc.reason || c.Target.Adapter != "codex" || c.Target.Address != reader.key ||
				c.LaneHome != codexHome || c.Target.Attempt != AttemptIdentity(attempt, reservedAt) || c.Target.Home != "" {
				t.Fatalf("candidate %+v, want attempt %d on %s in %s for %q", c, attempt, reader.key, codexHome, tc.reason)
			}
		})
	}
}

// A withdrawal the transport proved frees the lane without a pickup, once,
// with its reason; the asks stay unread.
func TestWithdrawPublishAttemptSettlesOnce(t *testing.T) {
	f, writer := adapterChannel(t)
	f.policy("codex", true, 1)
	reader := f.newSession("codex")
	wantOK(t, f.join(reader, Join{Channel: "checkout", Alias: "reader"}, "reader-join"))
	f.hook(reader, codexStart("startup", publish.ModeDaemon, f.now.UnixMilli()))
	principal := f.principalOf(reader)
	bid := bindingIDOf(t, f, principal)
	ask := f.turnEndAsk(writer, "reader", "codex-withdrawn")
	attempt := codexAttempt(t, f, bid, principal, []int64{ask})
	for range 2 {
		f.inTx(func(tx store.Tx) error {
			return WithdrawPublishAttempt(t.Context(), tx, attempt, WithdrawSessionEnded, f.now)
		})
	}
	var retired sql.NullInt64
	var observation sql.NullString
	if err := f.db.QueryRow(`SELECT retired_at_ms, observation FROM publish_attempt WHERE id = ?`, attempt).Scan(&retired, &observation); err != nil {
		t.Fatal(err)
	}
	var events int
	_ = f.db.QueryRow(`SELECT COUNT(*) FROM event WHERE kind = 'publish_attempt_settled'
		AND json_extract(detail, '$.reason') = 'withdrawn: session ended'`).Scan(&events)
	var picked sql.NullInt64
	_ = f.db.QueryRow(`SELECT picked_up_at_ms FROM recipient WHERE message_id = ?`, ask).Scan(&picked)
	if !retired.Valid || observation.Valid || events != 1 || picked.Valid {
		t.Fatalf("retired %v observation %v events %d picked %v", retired, observation, events, picked)
	}
}
