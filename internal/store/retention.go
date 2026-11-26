package store

import (
	"context"
	"encoding/json"
	"time"
)

// RetentionWindow is how long op and observation rows are kept beyond what
// their readers need (#256): an operation key replays for at least this
// long, and a hook fact of an ended launch is kept this long.
const RetentionWindow = 7 * 24 * time.Hour

// retentionChunk bounds the rows one write transaction deletes.
const retentionChunk = 2000

// RetentionResult counts the rows one pass deleted.
type RetentionResult struct {
	Ops          int64
	Observations int64
	Turns        int64
}

// retentionKeep holds what a pass keeps at any age, read once when it
// starts: rows and launches added during the pass are newer than its cut,
// so they are never candidates.
type retentionKeep struct {
	ops, observations string // JSON arrays of rowids and seqs
	running           string // JSON array of "<principal>:<launch>" keys
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
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(json_group_array(s), '[]') FROM (
		SELECT MAX(seq) AS s FROM observation GROUP BY principal_id
		UNION
		SELECT (SELECT x.seq FROM observation x WHERE x.principal_id = p.principal_id AND x.kind = 'command'
			ORDER BY x.observed_at_ms DESC, x.seq DESC LIMIT 1)
		FROM (SELECT DISTINCT principal_id FROM observation WHERE kind = 'command') p)`).Scan(&keep.observations); err != nil {
		return keep, err
	}
	// Launches still running for a member with a live binding: the turn
	// ledger re-reads every prompt and Stop of such a launch at each of its
	// hooks, so none of its facts may go. A session-scoped launch never
	// records an end, so a session_ended fact since its latest start ends it
	// too; a resume starts it again.
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(json_group_array(l.principal_id || ':' || l.launch_ref), '[]')
		FROM adapter_launch l WHERE l.ended_hook_at_ms IS NULL
			AND NOT EXISTS (SELECT 1 FROM observation e WHERE e.principal_id = l.principal_id
				AND e.launch_ref = l.launch_ref AND e.kind = 'session_ended' AND e.hook_at_ms >= l.started_hook_at_ms)
			AND EXISTS (SELECT 1 FROM binding b WHERE b.principal_id = l.principal_id AND b.state = 'live')`).Scan(&keep.running)
	return keep, err
}

// running holds for an observation of a launch in the pass's running set.
const running = `o.principal_id || ':' || COALESCE(o.launch_ref, '') IN (SELECT value FROM json_each(?5))`

// retentionSteps run in order on each daily pass. Each deletes rows older
// than its window, counted back from the start of the pass's UTC day, so a
// second pass on the same day deletes nothing.
var retentionSteps = []struct {
	name   string
	window time.Duration
	delete string // ?1 cut, ?2 kept op rowids, ?3 kept observation seqs, ?4 limit, ?5 running launches
}{
	{"ops", RetentionWindow, `DELETE FROM op WHERE rowid IN (SELECT rowid FROM op
		WHERE at_ms < ?1 AND rowid NOT IN (SELECT value FROM json_each(?2)) LIMIT ?4)`},
	{"link fences", RetentionWindow, `DELETE FROM link_fence WHERE rowid IN (SELECT rowid FROM link_fence
		WHERE at_ms < ?1 LIMIT ?4)`},
	{"device events", RetentionWindow, `DELETE FROM device_event WHERE rowid IN (SELECT rowid FROM device_event
		WHERE at_ms < ?1 ORDER BY at_ms LIMIT ?4)`},
	{"event streams", RetentionWindow, `DELETE FROM link_event_stream WHERE rowid IN (SELECT rowid FROM link_event_stream
		WHERE COALESCE(last_ack_at_ms, created_at_ms) < ?1 ORDER BY COALESCE(last_ack_at_ms, created_at_ms) LIMIT ?4)`},
	{"link input receipts", RetentionWindow, `DELETE FROM link_event_input WHERE rowid IN (SELECT rowid FROM link_event_input
		WHERE recorded_at_ms < ?1 ORDER BY recorded_at_ms LIMIT ?4)`},
	{"observations", RetentionWindow, `DELETE FROM observation WHERE seq IN (SELECT o.seq FROM observation o
		WHERE o.observed_at_ms < ?1 AND o.seq NOT IN (SELECT value FROM json_each(?3))
			AND (o.kind = 'command' OR NOT ` + running + `) LIMIT ?4)`},
	// The turns of a launch whose prompts and Stops are all gone, unless a
	// turn-end notice references them: the ledger can no longer rebuild
	// them, and a resumed launch rebuilds its turns from its new facts. A
	// running launch keeps its facts, so it keeps its turns.
	{"turns", RetentionWindow, `DELETE FROM turn WHERE rowid IN (SELECT t.rowid FROM turn t
		WHERE t.opened_hook_at_ms < ?1
			AND NOT EXISTS (SELECT 1 FROM observation o WHERE o.principal_id = t.principal_id
				AND o.launch_ref = t.launch_ref AND o.kind IN ('prompt_submitted', 'turn_finishing'))
			AND NOT EXISTS (SELECT 1 FROM turn_end_notice n WHERE n.principal_id = t.principal_id
				AND n.launch_ref = t.launch_ref AND n.turn_ref = t.turn_ref)
		LIMIT ?4)`},
}

// retentionCut is the oldest time a step keeps on the pass at now.
func retentionCut(now time.Time, window time.Duration) int64 {
	y, m, d := now.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Add(-window).UnixMilli()
}

// RetentionPass is one daily pass in progress: the dispatcher runs it a
// chunk at a time, so a hook waits for one chunk at most.
type RetentionPass struct {
	now    time.Time
	keep   retentionKeep
	step   int
	limit  int
	Result RetentionResult
}

// StartRetention reads what the pass at now keeps, in a read transaction.
func (s *Store) StartRetention(ctx context.Context, now time.Time) (*RetentionPass, error) {
	p := &RetentionPass{now: now, limit: retentionChunk}
	err := s.View(ctx, func(tx Tx) (err error) {
		p.keep, err = readRetentionKeep(ctx, tx)
		return err
	})
	return p, err
}

// RetentionChunk deletes at most one chunk of the pass's current step in
// one write transaction, and reports whether the pass is done.
func (s *Store) RetentionChunk(ctx context.Context, p *RetentionPass) (bool, error) {
	err := s.Update(ctx, func(tx Tx) error { return p.chunk(ctx, tx) })
	return p.step == len(retentionSteps), err
}

func (p *RetentionPass) chunk(ctx context.Context, tx Tx) error {
	step := retentionSteps[p.step]
	var n int64
	if step.name == "ops" || step.name == "link fences" {
		var err error
		if n, err = p.pruneFences(ctx, tx, step.name, step.delete, retentionCut(p.now, step.window)); err != nil {
			return err
		}
	} else {
		res, err := tx.ExecContext(ctx, step.delete, retentionCut(p.now, step.window), p.keep.ops, p.keep.observations,
			p.limit, p.keep.running)
		if err != nil {
			return err
		}
		if n, err = res.RowsAffected(); err != nil {
			return err
		}
	}
	switch step.name {
	case "ops":
		p.Result.Ops += n
	case "observations":
		p.Result.Observations += n
	case "turns":
		p.Result.Turns += n
	}
	// A launch that lost facts or turns has its turns reassigned from all
	// its facts again: every launch this pass may delete from, in the same
	// transaction as the delete.
	if n > 0 && step.name != "ops" && step.name != "link fences" {
		if _, err := tx.ExecContext(ctx, `UPDATE adapter_launch SET reconciled_seq = NULL, reconciled_at_ms = NULL
			WHERE reconciled_seq IS NOT NULL AND principal_id || ':' || launch_ref NOT IN (SELECT value FROM json_each(?))`,
			p.keep.running); err != nil {
			return err
		}
	}
	if n < int64(p.limit) {
		p.step++
	}
	return nil
}

// pruneFences runs a step that may delete a linked device's request
// fences (#400): the link-scoped op rows a receipt lookup wrote, or
// link_fence rows. In the same transaction it raises the admission
// generation of exactly the devices whose fences it deleted, so a request
// held on a link accepted before a fence is never admitted after the
// fence is gone, whatever the wall clock does. Other devices' links stay.
func (p *RetentionPass) pruneFences(ctx context.Context, tx Tx, name, del string, cut int64) (int64, error) {
	returning, devices := " RETURNING scope, principal_id", `SELECT device_id FROM principal
		WHERE id IN (SELECT value FROM json_each(?)) AND device_id IS NOT NULL`
	if name == "link fences" {
		returning, devices = " RETURNING 'link', device_id", `SELECT value FROM json_each(?)`
	}
	rows, err := tx.QueryContext(ctx, del+returning, cut, p.keep.ops, p.keep.observations, p.limit, p.keep.running)
	if err != nil {
		return 0, err
	}
	var n int64
	fenced := []any{}
	for rows.Next() {
		var scope string
		var owner any
		if err := rows.Scan(&scope, &owner); err != nil {
			rows.Close()
			return 0, err
		}
		n++
		if scope == "link" {
			fenced = append(fenced, owner)
		}
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if err := rows.Err(); err != nil || len(fenced) == 0 {
		return n, err
	}
	raw, err := json.Marshal(fenced)
	if err != nil {
		return 0, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE device SET admission_gen = admission_gen + 1 WHERE device_id IN (`+devices+`)`, string(raw))
	return n, err
}

// PruneRetentionTx runs a whole pass inside one transaction.
func PruneRetentionTx(ctx context.Context, tx Tx, now time.Time) (RetentionResult, error) {
	p := &RetentionPass{now: now, limit: retentionChunk}
	var err error
	if p.keep, err = readRetentionKeep(ctx, tx); err != nil {
		return RetentionResult{}, err
	}
	for p.step < len(retentionSteps) {
		if err := p.chunk(ctx, tx); err != nil {
			return p.Result, err
		}
	}
	return p.Result, nil
}
