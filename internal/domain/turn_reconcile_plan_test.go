package domain

import (
	"strings"
	"testing"
)

// #366: every read of the recent reassignment searches an index on the
// launch, or the observation sequence; none scans a launch's history.
func TestTheRecentReassignmentReadsThroughIndexes(t *testing.T) {
	f := newFixture(t)
	queries := map[string]struct {
		query string
		args  []any
	}{
		"launchFacts":         {launchFactsQuery, []any{1, "l", 5, 9}},
		"droppedTurnFacts":    {droppedTurnFacts, []any{1, "l", 5, 9}},
		"unreadTurnFacts":     {unreadTurnFacts, []any{1, "l", 7, 5, 9}},
		"cutTurns":            {cutTurns, []any{1, "l", 5, cutCandidates}},
		"runningPlaceholders": {runningPlaceholders, []any{1, "l", 5}},
		"earlierTurnNamed":    {earlierTurnNamed, []any{1, "l", `["a"]`, 5, `[]`}},
		"turnRowsFrom":        {turnRowsFrom, []any{1, "l", 5, `["a"]`}},
	}
	bounded := []string{
		"USING INDEX observation_launch_hook (principal_id=? AND launch_ref=? AND hook_at_ms>? AND hook_at_ms<?)",
		"USING INTEGER PRIMARY KEY (rowid>?)", // the facts stored since the launch's last reassignment
		"USING INDEX turn_launch_opened (principal_id=? AND launch_ref=? AND opened_hook_at_ms>?)",
		"USING INDEX sqlite_autoindex_turn_1 (principal_id=? AND launch_ref=? AND turn_ref=?)",
		"USING INDEX turn_placeholder_finishing (principal_id=? AND launch_ref=? AND finishing_hook_at_ms=?)",
		"USING INDEX turn_placeholder_finishing (principal_id=? AND launch_ref=? AND finishing_hook_at_ms>?)",
		"USING COVERING INDEX sqlite_autoindex_turn_end_notice_1 (principal_id=? AND launch_ref=? AND turn_ref=?)",
	}
	for name, q := range queries {
		rows, err := f.db.Query("EXPLAIN QUERY PLAN "+q.query, q.args...)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(detail, "SEARCH ") && !strings.HasPrefix(detail, "SCAN ") ||
				strings.Contains(detail, "VIRTUAL TABLE") || detail == "SCAN CONSTANT ROW" || strings.HasPrefix(detail, "SEARCH (subquery") {
				continue
			}
			// Only the cut candidates walk back from a time, under their LIMIT.
			ok := name == "cutTurns" && strings.HasSuffix(detail, "USING INDEX turn_launch_opened (principal_id=? AND launch_ref=? AND opened_hook_at_ms<?)")
			for _, b := range bounded {
				ok = ok || strings.HasPrefix(detail, "SEARCH ") && strings.HasSuffix(detail, b)
			}
			if !ok {
				t.Errorf("%s: %s", name, detail)
			}
		}
		rows.Close()
	}
}
