package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
)

// scanLatestFact is ReadLatestFact as it was before #366: one query over
// every observation of the latest launch. The bounded read must pick the
// same fact on any history.
func scanLatestFact(ctx context.Context, tx Tx, principal, slackMS int64) (LatestFact, bool, error) {
	var f LatestFact
	err := tx.QueryRowContext(ctx, `
		WITH latest AS (SELECT launch_ref, started_hook_at_ms, ended_hook_at_ms IS NULL AND fenced = 0 AS live
		                FROM adapter_launch WHERE principal_id = ?1
		                ORDER BY ended_hook_at_ms IS NOT NULL, started_hook_at_ms DESC, rowid DESC LIMIT 1),
		candidate AS (
			SELECT o.seq, o.kind, o.profile, o.hook_at_ms, o.fresh_until_ms, o.turn_ref, o.launch_ref,
			       CASE o.kind WHEN 'session_ended' THEN 4 WHEN 'turn_finishing' THEN 3
			                   WHEN 'approval_requested' THEN 2 ELSE 1 END AS stage,
			       COALESCE((SELECT t.opened_hook_at_ms FROM turn t WHERE t.principal_id = o.principal_id
			                 AND t.launch_ref = o.launch_ref AND t.turn_ref = o.turn_ref), o.hook_at_ms) AS position
			FROM observation o JOIN latest ON o.launch_ref = latest.launch_ref
			WHERE o.principal_id = ?1 AND o.kind NOT IN ('command', 'idle_prompt')
			  AND o.hook_at_ms <= o.observed_at_ms + ?2),
		staged AS (
			SELECT c.*, MAX(c.stage) OVER (PARTITION BY c.turn_ref) AS top FROM candidate c),
		pick AS (
			SELECT c.seq, c.kind, c.profile, c.hook_at_ms, c.fresh_until_ms FROM staged c
			WHERE c.position >= (SELECT started_hook_at_ms FROM latest) AND (c.turn_ref IS NULL OR c.stage = c.top)
			ORDER BY c.position DESC, c.stage DESC, c.hook_at_ms DESC, c.seq DESC
			LIMIT 1)
		SELECT p.seq, p.kind, (SELECT source FROM observation WHERE seq = p.seq), p.profile, COALESCE(json_extract(p.profile, '$.adapter'), ''), p.hook_at_ms, p.fresh_until_ms,
		       (SELECT live FROM latest),
		       EXISTS (SELECT 1 FROM observation k WHERE k.principal_id = ?1 AND k.kind = 'command' AND k.seq > p.seq)
		FROM pick p`, principal, slackMS).
		Scan(&f.Seq, &f.Fact, &f.Source, &f.Profile, &f.Adapter, &f.AtMS, &f.FreshUntilMS, &f.LaunchLive, &f.CommandSince)
	if errors.Is(err, sql.ErrNoRows) {
		return LatestFact{}, false, nil
	}
	return f, err == nil, err
}

// The bounded read picks the fact the full scan picks, on random histories
// that stress what decides the pick: several launches and resumes, ties in
// one millisecond, out-of-order delivery, hook clocks past the slack, turns
// opened after their own facts, facts no turn row places, and commands.
func TestTheBoundedLatestFactReadPicksWhatTheFullScanPicks(t *testing.T) {
	ctx := t.Context()
	const histories, slack = 3000, 5
	db, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rng := rand.New(rand.NewPCG(366, 1))
	kinds := []string{"prompt_submitted", "turn_finishing", "approval_requested", "session_ended", "idle_prompt", "command"}
	var b strings.Builder
	for p := 1; p <= histories; p++ {
		fmt.Fprintf(&b, "INSERT INTO principal (id, kind, vendor, created_at_ms) VALUES (%d, 'session', 'claude', 1);\n", p)
		launches := 1 + rng.IntN(3)
		for l := 0; l < launches; l++ {
			ended := "NULL"
			if rng.IntN(3) == 0 {
				ended = fmt.Sprint(rng.IntN(60))
			}
			fmt.Fprintf(&b, `INSERT INTO adapter_launch (principal_id, launch_ref, profile, verified, started_hook_at_ms, ended_hook_at_ms, fenced)
				VALUES (%d, 'l%d', '{"adapter":"claude","manifest_rev":"claude-3"}', 0, %d, %s, %d);`+"\n", p, l, rng.IntN(30), ended, rng.IntN(5)/4)
			for turn := 0; turn < 6; turn++ {
				if rng.IntN(2) == 0 {
					fmt.Fprintf(&b, "INSERT INTO turn (principal_id, launch_ref, turn_ref, opened_hook_at_ms) VALUES (%d, 'l%d', 't%d', %d);\n",
						p, l, turn, rng.IntN(60))
				}
			}
		}
		for i, n := 0, rng.IntN(25); i < n; i++ {
			kind := kinds[rng.IntN(len(kinds))]
			if kind == "command" {
				fmt.Fprintf(&b, "INSERT INTO observation (principal_id, kind, origin, source, observed_at_ms) VALUES (%d, 'command', 'unknown', 'cli', %d);\n", p, rng.IntN(60))
				continue
			}
			turn := "NULL"
			if rng.IntN(4) != 0 {
				turn = fmt.Sprintf("'t%d'", rng.IntN(7))
			}
			hook := rng.IntN(60)
			observed := hook + rng.IntN(2*slack+4) - slack - 3
			fresh := "NULL"
			if rng.IntN(2) == 0 {
				fresh = fmt.Sprint(hook + rng.IntN(30))
			}
			fmt.Fprintf(&b, `INSERT INTO observation (principal_id, kind, origin, source, observed_at_ms, fresh_until_ms, launch_ref, event_ref, turn_ref, hook_at_ms, profile)
				VALUES (%d, '%s', 'unknown', 'hook-%d', %d, %s, 'l%d', 'e%d', %s, %d, '{"adapter":"a%d"}');`+"\n",
				p, kind, i, observed, fresh, rng.IntN(launches), i, turn, hook, rng.IntN(3))
		}
	}
	if err := db.Update(ctx, func(tx Tx) error {
		_, err := tx.ExecContext(ctx, b.String())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	found := 0
	if err := db.View(ctx, func(tx Tx) error {
		for p := int64(1); p <= histories; p++ {
			want, wantFound, err := scanLatestFact(ctx, tx, p, slack)
			if err != nil {
				return err
			}
			got, gotFound, err := ReadLatestFact(ctx, tx, p, slack)
			if err != nil {
				return err
			}
			if got != want || gotFound != wantFound {
				return fmt.Errorf("principal %d: bounded read %+v (%t), full scan %+v (%t)", p, got, gotFound, want, wantFound)
			}
			if wantFound {
				found++
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if found < histories/2 {
		t.Fatalf("only %d of %d histories have a fact", found, histories)
	}
}

// Each query of the latest-fact read goes through an index from the newest
// rows down: none scans a launch's observations or turns.
func TestTheLatestFactQueriesReadFromTheNewestRows(t *testing.T) {
	ctx := t.Context()
	db, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	bounded := []string{
		"USING INDEX turn_launch_opened (principal_id=? AND launch_ref=? AND opened_hook_at_ms>?)",
		"USING INDEX turn_launch_opened (principal_id=? AND launch_ref=? AND opened_hook_at_ms=?)",
		"USING INDEX observation_launch_turn (principal_id=? AND launch_ref=? AND turn_ref=?)",
		"USING INDEX observation_launch_hook (principal_id=? AND launch_ref=? AND hook_at_ms>?)",
		"USING INDEX observation_launch_hook (principal_id=? AND launch_ref=? AND hook_at_ms=?)",
		"USING COVERING INDEX sqlite_autoindex_turn_1 (principal_id=? AND launch_ref=? AND turn_ref=?)",
		"USING INDEX observation_principal_sequence (principal_id=? AND seq>?)",
		"USING INTEGER PRIMARY KEY (rowid=?)",
	}
	for name, query := range map[string]string{"latestTurn": latestTurnQuery, "ownTime": ownTimeQuery, "pick": pickQuery} {
		if err := db.View(ctx, func(tx Tx) error {
			rows, err := tx.QueryContext(ctx, "EXPLAIN QUERY PLAN "+query, 1, "l", 5, 0)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var id, parent, unused int
				var detail string
				if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
					return err
				}
				table, _, _ := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(detail, "SEARCH "), "SCAN "), " ")
				if !strings.HasPrefix(detail, "SEARCH ") && !strings.HasPrefix(detail, "SCAN ") || table == "pick" || table == "tied" {
					continue
				}
				ok := false
				for _, b := range bounded {
					ok = ok || strings.HasPrefix(detail, "SEARCH ") && strings.HasSuffix(detail, b)
				}
				if !ok {
					t.Errorf("%s: %s", name, detail)
				}
			}
			return rows.Err()
		}); err != nil {
			t.Fatal(err)
		}
	}
}
