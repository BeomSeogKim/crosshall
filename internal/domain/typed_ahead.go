package domain

import (
	"context"
	"database/sql"
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

// turnFact is a recorded prompt or Stop of a launch.
type turnFact struct {
	prompt bool
	ref    string
	at     int64
	event  string
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
	var turns []assignedTurn
	byName := map[string]int{}
	opened := map[string]int{}
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
		case f.prompt && known:
			opened[f.event] = -1
			if i == len(turns)-1 && turns[i].finishing == 0 {
				opened[f.event] = open(assignedTurn{name: placeholder(f.ref, f.at), opened: f.at, placeholder: true})
			}
		case f.prompt:
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

// launchFacts reads a launch's recorded prompts and Stops in hook-time
// order. Within one millisecond, a Stop comes before the prompts (it ended
// the turn they follow), except after a prompt with its own id: a Stop
// never precedes its turn's first prompt. Remaining ties go by event, not
// by delivery. A fact stamped beyond the dispatcher's clock slack is left
// out until the clock reaches it.
func launchFacts(ctx context.Context, tx store.Tx, principal int64, launch string, nowMS int64) ([]turnFact, error) {
	rows, err := tx.QueryContext(ctx, `SELECT o.kind = ?4, o.turn_ref, o.hook_at_ms, o.event_ref FROM observation o
		WHERE o.principal_id = ?1 AND o.launch_ref = ?2 AND o.kind IN (?4, ?5) AND o.turn_ref IS NOT NULL AND o.hook_at_ms <= ?3
		ORDER BY o.hook_at_ms, CASE WHEN o.kind = ?4 THEN 1
			WHEN EXISTS (SELECT 1 FROM observation p WHERE p.principal_id = o.principal_id AND p.launch_ref = o.launch_ref
				AND p.kind = ?4 AND p.turn_ref = o.turn_ref AND p.hook_at_ms = o.hook_at_ms) THEN 2 ELSE 0 END, o.event_ref`,
		principal, launch, nowMS+futureSlackMS, FactPromptSubmitted, FactTurnFinishing)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var facts []turnFact
	for rows.Next() {
		var f turnFact
		if err := rows.Scan(&f.prompt, &f.ref, &f.at, &f.event); err != nil {
			return nil, err
		}
		facts = append(facts, f)
	}
	return facts, rows.Err()
}

// reconcileTurns makes the launch's turn rows the hook-order assignment of
// its recorded facts, and returns the turn the prompt event opened ("" for
// a repeat) and whether that turn is the latest one opened. Rows are
// matched by name. A turn that gave a turn-end notice keeps its name: it is
// updated in place, or, when the assignment no longer has it, kept as a
// superseded turn.
func reconcileTurns(ctx context.Context, tx store.Tx, principal int64, launch, event string, nowMS int64) (string, bool, error) {
	facts, err := launchFacts(ctx, tx, principal, launch, nowMS)
	if err != nil {
		return "", false, err
	}
	turns, opened := assignTurns(facts)
	want := map[string]int{}
	for i, t := range turns {
		want[t.name] = i
	}
	type row struct {
		name      string
		opened    int64
		finishing sql.NullInt64
		old       bool
		claimed   bool
	}
	rows, err := tx.QueryContext(ctx, `SELECT t.turn_ref, t.opened_hook_at_ms, t.finishing_hook_at_ms, t.superseded,
			EXISTS (SELECT 1 FROM turn_end_notice n WHERE n.principal_id = t.principal_id AND n.launch_ref = t.launch_ref AND n.turn_ref = t.turn_ref)
		FROM turn t WHERE t.principal_id = ? AND t.launch_ref = ?`, principal, launch)
	if err != nil {
		return "", false, err
	}
	var have []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.name, &r.opened, &r.finishing, &r.old, &r.claimed); err != nil {
			rows.Close()
			return "", false, err
		}
		have = append(have, r)
	}
	if err := rows.Close(); err != nil {
		return "", false, err
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
					return "", false, err
				}
			}
			delete(want, r.name)
		case r.claimed:
			if _, err := tx.ExecContext(ctx, `UPDATE turn SET superseded = 1
				WHERE principal_id = ? AND launch_ref = ? AND turn_ref = ?`, principal, launch, r.name); err != nil {
				return "", false, err
			}
		default:
			if _, err := tx.ExecContext(ctx, `DELETE FROM turn WHERE principal_id = ? AND launch_ref = ? AND turn_ref = ?`,
				principal, launch, r.name); err != nil {
				return "", false, err
			}
		}
	}
	for i, t := range turns {
		if _, ok := want[t.name]; !ok {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO turn (principal_id, launch_ref, turn_ref, opened_hook_at_ms, finishing_hook_at_ms, superseded)
			VALUES (?, ?, ?, ?, ?, ?)`, principal, launch, t.name, t.opened, finishing(t), i != len(turns)-1); err != nil {
			return "", false, err
		}
	}
	i, ok := opened[event]
	if !ok || i < 0 {
		return "", false, nil
	}
	return turns[i].name, i == len(turns)-1, nil
}
