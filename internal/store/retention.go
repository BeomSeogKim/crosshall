package store

import (
	"context"
	"time"
)

// RetentionWindow is how long op and observation rows are kept beyond what
// their readers need (#256): an operation key replays for this long, and
// a hook fact of an ended launch is kept this long.
const RetentionWindow = 7 * 24 * time.Hour

// retentionChunk bounds the rows one write transaction deletes, so hooks
// and commands interleave with a long first pass.
const retentionChunk = 2000

// RetentionResult counts the rows one pass deleted.
type RetentionResult struct {
	Ops          int64
	Observations int64
	Turns        int64
}

// retentionKeep holds the rows kept at any age, read once per pass: rows
// added during the pass are newer than its cut, so they are never
// candidates.
type retentionKeep struct {
	ops, observations string // JSON arrays of rowids and seqs
}

func readRetentionKeep(ctx context.Context, tx Tx) (retentionKeep, error) {
	var keep retentionKeep
	// Each principal's latest hook op: the departure token reads its
	// MAX(rowid), and new rows take larger rowids than a kept one.
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(json_group_array(r), '[]') FROM (
		SELECT MAX(rowid) AS r FROM op WHERE scope = 'hook' GROUP BY principal_id)`).Scan(&keep.ops); err != nil {
		return keep, err
	}
	// Each principal's newest observation (the departure token's MAX(seq),
	// and presence's last fact of an ended launch) and newest command (the
	// last-command views).
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(json_group_array(s), '[]') FROM (
		SELECT MAX(seq) AS s FROM observation GROUP BY principal_id
		UNION
		SELECT (SELECT x.seq FROM observation x WHERE x.principal_id = p.principal_id AND x.kind = 'command'
			ORDER BY x.observed_at_ms DESC, x.seq DESC LIMIT 1)
		FROM (SELECT DISTINCT principal_id FROM observation WHERE kind = 'command') p)`).Scan(&keep.observations)
	return keep, err
}

// runningLaunch holds for a launch, named by the row alias's principal_id
// and launch_ref columns, that has not ended while its principal has a
// live binding: the turn ledger re-reads every prompt and Stop of such a
// launch at each of its hooks, so none of its facts may go. A
// session-scoped launch never records an end, so a session_ended fact
// since its latest start ends it too; a resume starts it again.
func runningLaunch(alias string) string {
	return `EXISTS (SELECT 1 FROM adapter_launch l WHERE l.principal_id = ` + alias + `.principal_id
			AND l.launch_ref = ` + alias + `.launch_ref AND l.ended_hook_at_ms IS NULL
			AND NOT EXISTS (SELECT 1 FROM observation e WHERE e.principal_id = l.principal_id
				AND e.launch_ref = l.launch_ref AND e.kind = 'session_ended' AND e.hook_at_ms >= l.started_hook_at_ms))
		AND EXISTS (SELECT 1 FROM binding b WHERE b.principal_id = ` + alias + `.principal_id AND b.state = 'live')`
}

var retentionSteps = []struct {
	name   string
	delete string // ?1 cut, ?2 kept op rowids, ?3 kept observation seqs, ?4 limit
}{
	{"ops", `DELETE FROM op WHERE rowid IN (SELECT rowid FROM op
		WHERE at_ms < ?1 AND rowid NOT IN (SELECT value FROM json_each(?2)) LIMIT ?4)`},
	{"observations", `DELETE FROM observation WHERE seq IN (SELECT o.seq FROM observation o
		WHERE o.observed_at_ms < ?1 AND o.seq NOT IN (SELECT value FROM json_each(?3))
			AND (o.kind = 'command' OR NOT (` + runningLaunch("o") + `)) LIMIT ?4)`},
	// The turns of a launch whose facts are all gone, unless a turn-end
	// notice references them: the ledger can no longer rebuild them, and a
	// resumed launch rebuilds its turns from its new facts.
	{"turns", `DELETE FROM turn WHERE rowid IN (SELECT t.rowid FROM turn t
		WHERE t.opened_hook_at_ms < ?1 AND NOT (` + runningLaunch("t") + `)
			AND NOT EXISTS (SELECT 1 FROM observation o WHERE o.principal_id = t.principal_id
				AND o.launch_ref = t.launch_ref AND o.kind IN ('prompt_submitted', 'turn_finishing'))
			AND NOT EXISTS (SELECT 1 FROM turn_end_notice n WHERE n.principal_id = t.principal_id
				AND n.launch_ref = t.launch_ref AND n.turn_ref = t.turn_ref)
		LIMIT ?4)`},
}

func retentionCut(now time.Time) int64 { return now.Add(-RetentionWindow).UnixMilli() }

func pruneChunk(ctx context.Context, tx Tx, step int, cut int64, keep retentionKeep, limit int) (int64, error) {
	res, err := tx.ExecContext(ctx, retentionSteps[step].delete, cut, keep.ops, keep.observations, limit)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (r *RetentionResult) add(step int, n int64) {
	switch retentionSteps[step].name {
	case "ops":
		r.Ops += n
	case "observations":
		r.Observations += n
	case "turns":
		r.Turns += n
	}
}

// PruneRetention deletes the op, observation and turn rows past
// RetentionWindow that no reader needs, in write transactions of at most
// retentionChunk rows each.
func (s *Store) PruneRetention(ctx context.Context, now time.Time) (RetentionResult, error) {
	var keep retentionKeep
	if err := s.View(ctx, func(tx Tx) (err error) {
		keep, err = readRetentionKeep(ctx, tx)
		return err
	}); err != nil {
		return RetentionResult{}, err
	}
	return pruneSteps(ctx, retentionCut(now), keep, retentionChunk, func(apply func(Tx) error) error {
		return s.Update(ctx, apply)
	})
}

// PruneRetentionTx runs the same pass inside one transaction.
func PruneRetentionTx(ctx context.Context, tx Tx, now time.Time) (RetentionResult, error) {
	keep, err := readRetentionKeep(ctx, tx)
	if err != nil {
		return RetentionResult{}, err
	}
	return pruneSteps(ctx, retentionCut(now), keep, retentionChunk, func(apply func(Tx) error) error { return apply(tx) })
}

func pruneSteps(ctx context.Context, cut int64, keep retentionKeep, limit int, update func(func(Tx) error) error) (RetentionResult, error) {
	var out RetentionResult
	for step := range retentionSteps {
		for {
			var n int64
			if err := update(func(tx Tx) (err error) {
				n, err = pruneChunk(ctx, tx, step, cut, keep, limit)
				return err
			}); err != nil {
				return out, err
			}
			out.add(step, n)
			if n < int64(limit) {
				break
			}
		}
	}
	return out, nil
}
