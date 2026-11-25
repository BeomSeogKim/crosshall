package domain

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"strconv"

	"github.com/BeomSeogKim/crosshall/internal/store"
)

// typedAheadPrefix marks a turn opened by a prompt that reported the
// running turn's id. Claude Code does so for a prompt typed while a turn
// runs, and that prompt's Stop carries an id no prompt had (#163).
const typedAheadPrefix = "typed-ahead:"

func placeholder(ref string, at int64) string {
	return typedAheadPrefix + ref + "@" + strconv.FormatInt(at, 10)
}

type turnFact struct {
	kind  string
	ref   string
	at    int64
	event string
}

// assignedTurn is a turn of a launch as its facts in hook order make it.
type assignedTurn struct {
	name        string
	opened      int64
	finishing   int64 // 0 while no Stop finished it
	placeholder bool
}

// assignTurns applies a launch's prompts and Stops in hook-time order, so
// the turns depend on what the hooks recorded, not on the order their
// callbacks were delivered in. It returns the turns in the order they
// opened, and for each prompt event the index of the turn it opened, or -1
// for a prompt that repeats one already handled.
//
//   - A prompt with the id of the latest turn, still running, was typed
//     ahead: Claude Code reports the running turn's id for it. It opens a
//     placeholder. A later prompt with that id is merged into it and
//     opens nothing.
//   - A prompt with a new id while a placeholder is the only turn running
//     since the latest turn with an id of its own: the id is the
//     placeholder's, typed ahead in a chain. The placeholder takes it, and
//     the prompt opens the next placeholder.
//   - Any other prompt with a new id opens its turn; any other repeat
//     opens nothing.
//   - A Stop finishes its id's turn (the earliest Stop counts). A Stop with
//     an id no turn has adopts the only running placeholder, if exactly one
//     runs since the latest turn with an id of its own; otherwise nothing.
func assignTurns(facts []turnFact) ([]assignedTurn, map[string]int) {
	return assignTurnsFrom(nil, facts)
}

// assignTurnsFrom applies the facts to the turns already open before them.
func assignTurnsFrom(seed []assignedTurn, facts []turnFact) ([]assignedTurn, map[string]int) {
	turns := slices.Clone(seed)
	byName := map[string]int{}
	for i, t := range turns {
		byName[t.name] = i
	}
	opened := map[string]int{}
	checkedAtIdle := 0
	sole := func() int {
		start := 0
		for i := len(turns) - 1; i >= 0; i-- {
			if !turns[i].placeholder {
				start = i
				break
			}
		}
		found := -1
		for i := start; i < len(turns); i++ {
			if turns[i].finishing != 0 {
				continue
			}
			if found >= 0 || !turns[i].placeholder {
				return -1
			}
			found = i
		}
		return found
	}
	open := func(t assignedTurn) int {
		turns = append(turns, t)
		byName[t.name] = len(turns) - 1
		return len(turns) - 1
	}
	rename := func(i int, ref string) {
		delete(byName, turns[i].name)
		turns[i].name, turns[i].placeholder = ref, false
		byName[ref] = i
	}
	for _, f := range facts {
		i, known := byName[f.ref]
		switch {
		case f.kind == FactIdlePrompt:
			for j := checkedAtIdle; j < len(turns); j++ {
				if turns[j].placeholder && turns[j].finishing == 0 {
					turns[j].finishing = f.at
				}
			}
			checkedAtIdle = len(turns)
		case f.kind == FactPromptSubmitted && known:
			opened[f.event] = -1
			if i == len(turns)-1 && turns[i].finishing == 0 {
				opened[f.event] = open(assignedTurn{name: placeholder(f.ref, f.at), opened: f.at, placeholder: true})
			}
		case f.kind == FactPromptSubmitted:
			if j := sole(); j >= 0 {
				rename(j, f.ref)
				opened[f.event] = open(assignedTurn{name: placeholder(f.ref, f.at), opened: f.at, placeholder: true})
			} else {
				opened[f.event] = open(assignedTurn{name: f.ref, opened: f.at})
			}
		case known:
			if turns[i].finishing == 0 {
				turns[i].finishing = f.at
			}
		default:
			if j := sole(); j >= 0 {
				rename(j, f.ref)
				turns[j].finishing = f.at
			}
		}
	}
	return turns, opened
}

// turnFact selects the observations a launch's turns are folded from.
const turnFactKind = `(kind = 'idle_prompt' OR (kind IN ('prompt_submitted', 'turn_finishing') AND turn_ref IS NOT NULL))`

// launchFacts reads a launch's recorded turn activity from hook time from
// on, in hook-time order. Within one millisecond, a Stop comes before the
// prompts (it ended the turn they follow), except after a prompt with its
// own id: a Stop never precedes its turn's first prompt. Remaining ties go
// by event, not by delivery. A fact stamped beyond the dispatcher's clock
// slack is left out until the clock reaches it.
func launchFacts(ctx context.Context, tx store.Tx, principal int64, launch string, from, nowMS int64) ([]turnFact, error) {
	rows, err := tx.QueryContext(ctx, launchFactsQuery, principal, launch, from, nowMS+futureSlackMS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var facts []turnFact
	for rows.Next() {
		var f turnFact
		if err := rows.Scan(&f.kind, &f.ref, &f.at, &f.event); err != nil {
			return nil, err
		}
		facts = append(facts, f)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	orderTies(facts)
	return facts, nil
}

// orderTies orders the facts of each millisecond: idle before tied activity, Stops of other turns,
// then prompts, then Stops of a turn prompted in that millisecond, keeping
// the event order within each.
func orderTies(facts []turnFact) {
	rank := func(f turnFact, prompted map[string]bool) int {
		switch {
		case f.kind == FactIdlePrompt:
			return -1
		case f.kind == FactPromptSubmitted:
			return 1
		case prompted[f.ref]:
			return 2
		}
		return 0
	}
	for start := 0; start < len(facts); {
		end := start + 1
		for end < len(facts) && facts[end].at == facts[start].at {
			end++
		}
		if end-start > 1 {
			prompted := map[string]bool{}
			for _, f := range facts[start:end] {
				if f.kind == FactPromptSubmitted {
					prompted[f.ref] = true
				}
			}
			slices.SortStableFunc(facts[start:end], func(a, b turnFact) int { return rank(a, prompted) - rank(b, prompted) })
		}
		start = end
	}
}

// reconcileTurns makes the launch's turn rows the hook-order assignment of
// its recorded facts, and returns the turn the prompt event opened ("" for
// a repeat) and whether that turn is the latest one opened. Rows are
// matched by name. A turn that gave a turn-end notice keeps its name: it is
// updated in place, or, when the assignment no longer has it, kept as a
// superseded turn. It folds only the recent facts when that gives the same
// turns as folding all of them (#366), and records what it read.
func reconcileTurns(ctx context.Context, tx store.Tx, principal int64, launch, event string, nowMS int64) (string, bool, error) {
	cut, turns, opened, ok, err := recentTurns(ctx, tx, principal, launch, event, nowMS)
	if err != nil {
		return "", false, err
	}
	if !ok {
		cut = math.MinInt64
		facts, err := launchFacts(ctx, tx, principal, launch, cut, nowMS)
		if err != nil {
			return "", false, err
		}
		turns, opened = assignTurns(facts)
	}
	if err := writeTurns(ctx, tx, principal, launch, cut, turns); err != nil {
		return "", false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE adapter_launch SET reconciled_seq = (SELECT MAX(seq) FROM observation), reconciled_at_ms = ?
		WHERE principal_id = ? AND launch_ref = ?`, nowMS, principal, launch); err != nil {
		return "", false, err
	}
	i, found := opened[event]
	if !found || i < 0 {
		return "", false, nil
	}
	return turns[i].name, i == len(turns)-1, nil
}

const placeholderName = `(turn_ref >= 'typed-ahead:' AND turn_ref < 'typed-ahead;')`

// unreadTurnFacts is the earliest hook time of a turn fact this
// reassignment reads and the launch's last one did not: stored after it,
// or stamped past its clock slack. The first part walks the member's
// sequence, not the launch.
const unreadTurnFacts = `SELECT MIN(hook_at_ms) FROM (
	SELECT hook_at_ms FROM observation WHERE principal_id = ?1 AND seq > ?3 AND +launch_ref = ?2 AND +hook_at_ms <= ?5 AND ` + turnFactKind + `
	UNION ALL
	SELECT hook_at_ms FROM observation WHERE principal_id = ?1 AND launch_ref = ?2 AND hook_at_ms > ?4 AND hook_at_ms <= ?5 AND ` + turnFactKind + `)`

// The queries of a reassignment. Each reads through an index on
// (principal_id, launch_ref, ...), so the recent fold reads only recent rows
// of the launch (TestTheRecentReassignmentReadsThroughIndexes).
const (
	// ?1 principal, ?2 launch, ?3 from, ?4 to (hook times).
	launchFactsQuery = `SELECT kind, COALESCE(turn_ref, ''), hook_at_ms, event_ref FROM observation
		WHERE principal_id = ?1 AND launch_ref = ?2 AND hook_at_ms >= ?3 AND hook_at_ms <= ?4 AND ` + turnFactKind + `
		ORDER BY hook_at_ms, event_ref`
	// ?1 principal, ?2 launch, ?3 after, ?4 to (hook times).
	droppedTurnFacts = `SELECT EXISTS (SELECT 1 FROM observation WHERE principal_id = ?1 AND launch_ref = ?2
		AND hook_at_ms > ?3 AND hook_at_ms <= ?4 AND ` + turnFactKind + `)`
	// ?1 principal, ?2 launch, ?3 before (hook time), ?4 limit.
	cutTurns = `SELECT turn_ref, opened_hook_at_ms FROM turn
		WHERE principal_id = ?1 AND launch_ref = ?2 AND opened_hook_at_ms < ?3 AND NOT ` + placeholderName + `
		ORDER BY opened_hook_at_ms DESC LIMIT ?4`
	// ?1 principal, ?2 launch, ?3 cut.
	runningPlaceholders = `SELECT turn_ref, opened_hook_at_ms,
			EXISTS (SELECT 1 FROM turn_end_notice n WHERE n.principal_id = t.principal_id AND n.launch_ref = t.launch_ref AND n.turn_ref = t.turn_ref)
		FROM turn t INDEXED BY turn_placeholder_finishing
		WHERE principal_id = ?1 AND launch_ref = ?2 AND ` + placeholderName + ` AND opened_hook_at_ms < ?3 AND finishing_hook_at_ms IS NULL
		UNION ALL
		SELECT turn_ref, opened_hook_at_ms,
			EXISTS (SELECT 1 FROM turn_end_notice n WHERE n.principal_id = t.principal_id AND n.launch_ref = t.launch_ref AND n.turn_ref = t.turn_ref)
		FROM turn t INDEXED BY turn_placeholder_finishing
		WHERE principal_id = ?1 AND launch_ref = ?2 AND ` + placeholderName + ` AND opened_hook_at_ms < ?3 AND finishing_hook_at_ms >= ?3
		ORDER BY 2, 1`
	// ?1 principal, ?2 launch, ?3 names (JSON).
	factNamed = `SELECT EXISTS (SELECT 1 FROM json_each(?3) j CROSS JOIN observation o
		ON o.principal_id = ?1 AND o.launch_ref = ?2 AND o.turn_ref = j.value)`
	// ?1 principal, ?2 launch, ?3 names (JSON), ?4 cut, ?5 names to skip (JSON).
	earlierTurnNamed = `SELECT EXISTS (SELECT 1 FROM json_each(?3) j CROSS JOIN turn t
		ON t.principal_id = ?1 AND t.launch_ref = ?2 AND t.turn_ref = j.value
		WHERE +t.opened_hook_at_ms < ?4 AND t.turn_ref NOT IN (SELECT value FROM json_each(?5)))`
	// ?1 principal, ?2 launch, ?3 cut, ?4 names opened before it (JSON).
	turnRowsFrom = `SELECT t.turn_ref, t.opened_hook_at_ms, t.finishing_hook_at_ms, t.superseded,
			EXISTS (SELECT 1 FROM turn_end_notice n WHERE n.principal_id = t.principal_id AND n.launch_ref = t.launch_ref AND n.turn_ref = t.turn_ref)
		FROM turn t WHERE t.principal_id = ?1 AND t.launch_ref = ?2 AND t.opened_hook_at_ms >= ?3
		UNION ALL
		SELECT t.turn_ref, t.opened_hook_at_ms, t.finishing_hook_at_ms, t.superseded,
			EXISTS (SELECT 1 FROM turn_end_notice n WHERE n.principal_id = t.principal_id AND n.launch_ref = t.launch_ref AND n.turn_ref = t.turn_ref)
		FROM json_each(?4) j CROSS JOIN turn t ON t.principal_id = ?1 AND t.launch_ref = ?2 AND t.turn_ref = j.value
		WHERE +t.opened_hook_at_ms < ?3`
)

// cutCandidates is how many of the latest turns before the first unread
// fact recentTurns tries as the cut.
const cutCandidates = 8

// recentTurns folds the facts from cut on: the opening of one of the latest
// turns before every fact the last reassignment did not read. ok is false when that fold can differ from folding every fact;
// the caller then does so.
//
// The rows hold the fold of every fact read last time, and the facts
// before cut are the same now: retention clears the record when it
// deletes. The fold from cut on is exact when, besides that:
//   - the first fact at cut is a prompt that opened its turn under its own
//     name, so the earlier turns are as before cut, and no later fact
//     renames or adopts them (those look back only to the latest turn
//     with a name of its own);
//   - no earlier placeholder runs at cut, which a later idle prompt would
//     finish;
//   - no fact or turn from cut on has the name of an earlier row, so no
//     later fact reaches an earlier turn and no new turn takes its name;
//   - the event's own fact is among those folded.
func recentTurns(ctx context.Context, tx store.Tx, principal int64, launch, event string, nowMS int64) (cut int64, turns []assignedTurn, opened map[string]int, ok bool, err error) {
	var readSeq, readAt sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT reconciled_seq, reconciled_at_ms FROM adapter_launch WHERE principal_id = ? AND launch_ref = ?`,
		principal, launch).Scan(&readSeq, &readAt)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil, nil, false, nil
	}
	if err != nil || !readSeq.Valid || !readAt.Valid {
		return 0, nil, nil, false, err
	}
	// A fact the last reassignment read and this one, its clock back, does
	// not may have changed any earlier turn.
	if nowMS < readAt.Int64 {
		var dropped bool
		if err := tx.QueryRowContext(ctx, droppedTurnFacts, principal, launch, nowMS+futureSlackMS, readAt.Int64+futureSlackMS).
			Scan(&dropped); err != nil || dropped {
			return 0, nil, nil, false, err
		}
	}
	var unread sql.NullInt64
	if err := tx.QueryRowContext(ctx, unreadTurnFacts, principal, launch, readSeq.Int64,
		readAt.Int64+futureSlackMS, nowMS+futureSlackMS).
		Scan(&unread); err != nil {
		return 0, nil, nil, false, err
	}
	bound := int64(math.MaxInt64)
	if unread.Valid {
		bound = unread.Int64
	}
	name, found := "", false
	rows, err := tx.QueryContext(ctx, cutTurns, principal, launch, bound, cutCandidates)
	if err != nil {
		return 0, nil, nil, false, err
	}
	type candidate struct {
		name   string
		opened int64
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.name, &c.opened); err != nil {
			rows.Close()
			return 0, nil, nil, false, err
		}
		candidates = append(candidates, c)
	}
	if err := rows.Close(); err != nil {
		return 0, nil, nil, false, err
	}
	for _, c := range candidates {
		at, err := launchFacts(ctx, tx, principal, launch, c.opened, c.opened-futureSlackMS)
		if err != nil {
			return 0, nil, nil, false, err
		}
		if len(at) > 0 && at[0].kind == FactPromptSubmitted && at[0].ref == c.name {
			name, cut, found = c.name, c.opened, true
			break
		}
	}
	if !found {
		return 0, nil, nil, false, nil
	}
	// The placeholders still running at cut: only an idle prompt, or a fact
	// with a placeholder's own name, changes them after cut, so the fold
	// starts with them. A row a turn-end notice keeps may be no turn at all.
	rows, err = tx.QueryContext(ctx, runningPlaceholders, principal, launch, cut)
	if err != nil {
		return 0, nil, nil, false, err
	}
	var seed []assignedTurn
	claimed := false
	for rows.Next() {
		var t assignedTurn
		var c bool
		if err := rows.Scan(&t.name, &t.opened, &c); err != nil {
			rows.Close()
			return 0, nil, nil, false, err
		}
		t.placeholder = true
		seed, claimed = append(seed, t), claimed || c
	}
	if err := rows.Close(); err != nil || claimed {
		return 0, nil, nil, false, err
	}
	running := make([]string, len(seed))
	for i, t := range seed {
		running[i] = t.name
	}
	seeded, err := json.Marshal(running)
	if err != nil {
		return 0, nil, nil, false, err
	}
	// A vendor may name a turn like a placeholder: such a turn is its own.
	var native bool
	if err := tx.QueryRowContext(ctx, factNamed, principal, launch, string(seeded)).Scan(&native); err != nil || native {
		return 0, nil, nil, false, err
	}
	facts, err := launchFacts(ctx, tx, principal, launch, cut, nowMS)
	if err != nil {
		return 0, nil, nil, false, err
	}
	if len(facts) == 0 || facts[0].ref != name || !slices.ContainsFunc(facts, func(f turnFact) bool { return f.event == event }) {
		return 0, nil, nil, false, nil
	}
	// The cut prompt opened its turn under its own name, as it did when the
	// rows were written: the turns before it decide that, not the seed.
	turns, opened = assignTurnsFrom(append(seed, assignedTurn{name: name, opened: cut}), facts[1:])
	opened[facts[0].event] = len(seed)
	names := make([]string, 0, len(facts)+len(turns))
	for _, f := range facts {
		names = append(names, f.ref)
	}
	for _, t := range turns[len(seed):] {
		names = append(names, t.name)
	}
	list, err := json.Marshal(names)
	if err != nil {
		return 0, nil, nil, false, err
	}
	var reached bool
	if err := tx.QueryRowContext(ctx, earlierTurnNamed, principal, launch, string(list), cut, string(seeded)).
		Scan(&reached); err != nil || reached {
		return 0, nil, nil, false, err
	}
	return cut, turns, opened, true, nil
}

// writeTurns makes the launch's rows opened from cut on, and the rows of
// turns opened before it, the given turns.
func writeTurns(ctx context.Context, tx store.Tx, principal int64, launch string, cut int64, turns []assignedTurn) error {
	want := map[string]int{}
	var before []string
	for i, t := range turns {
		want[t.name] = i
		if t.opened < cut {
			before = append(before, t.name)
		}
	}
	listed, err := json.Marshal(before)
	if err != nil {
		return err
	}
	type row struct {
		name      string
		opened    int64
		finishing sql.NullInt64
		old       bool
		claimed   bool
	}
	rows, err := tx.QueryContext(ctx, turnRowsFrom, principal, launch, cut, string(listed))
	if err != nil {
		return err
	}
	var have []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.name, &r.opened, &r.finishing, &r.old, &r.claimed); err != nil {
			rows.Close()
			return err
		}
		have = append(have, r)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	finishing := func(t assignedTurn) sql.NullInt64 {
		return sql.NullInt64{Int64: t.finishing, Valid: t.finishing != 0}
	}
	for _, r := range have {
		i, ok := want[r.name]
		switch {
		case ok:
			t := turns[i]
			old := i != len(turns)-1
			if r.opened != t.opened || r.finishing != finishing(t) || r.old != old {
				if _, err := tx.ExecContext(ctx, `UPDATE turn SET opened_hook_at_ms = ?, finishing_hook_at_ms = ?, superseded = ?
					WHERE principal_id = ? AND launch_ref = ? AND turn_ref = ?`, t.opened, finishing(t), old, principal, launch, r.name); err != nil {
					return err
				}
			}
			delete(want, r.name)
		case r.claimed:
			if _, err := tx.ExecContext(ctx, `UPDATE turn SET superseded = 1
				WHERE principal_id = ? AND launch_ref = ? AND turn_ref = ?`, principal, launch, r.name); err != nil {
				return err
			}
		default:
			if _, err := tx.ExecContext(ctx, `DELETE FROM turn WHERE principal_id = ? AND launch_ref = ? AND turn_ref = ?`,
				principal, launch, r.name); err != nil {
				return err
			}
		}
	}
	for i, t := range turns {
		if _, ok := want[t.name]; !ok {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO turn (principal_id, launch_ref, turn_ref, opened_hook_at_ms, finishing_hook_at_ms, superseded)
			VALUES (?, ?, ?, ?, ?, ?)`, principal, launch, t.name, t.opened, finishing(t), i != len(turns)-1); err != nil {
			return err
		}
	}
	return nil
}
