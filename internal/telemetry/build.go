package telemetry

import (
	"context"
	"database/sql"
	"runtime"
	"slices"
	"strconv"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/store"
)

// SnapshotWindow is how long after a day's close its snapshot may still be
// taken. A day built later carries none.
const SnapshotWindow = 10 * time.Minute

// unobservedAfter is the age at which an unobserved notice counts in
// notice.unobserved_10m: the hall's bound for telling a member it brings
// work (HeldReachBound in the domain).
const unobservedAfter = 10 * time.Minute

// Input names the day and the facts the store does not hold.
type Input struct {
	// Day is any instant of the UTC day to report.
	Day time.Time
	// Batch is the batch token, 32 hex characters drawn when the batch is
	// frozen.
	Batch string
	// Version is the binary's version; anything but a release tag is "dev".
	Version string
	// Now is when the report is built. Within SnapshotWindow after the
	// day's close the report adds the snapshot.
	Now time.Time
	// Hooks are the day's hook counter file counted by CountHooks, outside
	// the caller's transaction; HooksRead false marks the file missing.
	Hooks     HookCounts
	HooksRead bool
	// WeekActiveDays and WeekUnobserved10m are the freezer's figures over
	// the seven days ending with Day, before rounding.
	WeekActiveDays    int64
	WeekUnobserved10m int64
}

// Build reads one day's report in the caller's transaction. Every flow is
// counted on the day of its own timestamp, so a day built late or twice
// gives the same counts; only the snapshot depends on Now.
func Build(ctx context.Context, tx store.Tx, in Input) (Report, error) {
	start := in.Day.UTC().Truncate(24 * time.Hour)
	end := start.AddDate(0, 0, 1)
	r := Report{
		V:        1,
		Registry: registry.Registry,
		Batch:    in.Batch,
		Day:      start.Format("2006-01-02"),
		App:      App{Version: AppVersion(in.Version), Platform: runtime.GOOS + "-" + runtime.GOARCH},
		Sources:  Sources{Hooks: "missing", Snapshot: "none"},
	}
	raw := map[string]int64{"week.active_days": in.WeekActiveDays, "week.unobserved_10m": in.WeekUnobserved10m}
	if in.HooksRead {
		r.Sources.Hooks = "ok"
		for key, n := range in.Hooks {
			raw[key] += n
		}
	}
	b := builder{ctx: ctx, tx: tx, counts: raw, from: start.UnixMilli(), to: end.UnixMilli()}
	b.flows()
	if !in.Now.Before(end) && in.Now.Before(end.Add(SnapshotWindow)) {
		r.Sources.Snapshot = "taken"
		r.Switches = b.snapshot(in.Now)
	}
	if b.err != nil {
		return Report{}, b.err
	}
	r.Unobserved10m = raw["notice.unobserved_10m"]
	r.Counts = make(map[string]int64)
	for key, n := range raw {
		if v := Round(n); v > 0 && registry.has(key) {
			r.Counts[key] = v
		}
	}
	return r, nil
}

type builder struct {
	ctx      context.Context
	tx       store.Tx
	counts   map[string]int64
	from, to int64
	err      error
}

// count runs a query whose rows are (group, n) and adds each n under the key
// name returns; a key outside the registry drops the row.
func (b *builder) count(name func(group string) string, query string, args ...any) {
	if b.err != nil {
		return
	}
	rows, err := b.tx.QueryContext(b.ctx, query, args...)
	if err != nil {
		b.err = err
		return
	}
	defer rows.Close()
	for rows.Next() {
		var group sql.NullString
		var n int64
		if b.err = rows.Scan(&group, &n); b.err != nil {
			return
		}
		if key := name(group.String); registry.has(key) {
			b.counts[key] += n
		}
	}
	b.err = rows.Err()
}

func prefixed(prefix string) func(string) string { return func(g string) string { return prefix + g } }
func fixed(key string) func(string) string       { return func(string) string { return key } }

// detail reads one member of an event's detail without failing on a value
// that is not JSON.
func detail(member string) string {
	return `CASE WHEN json_valid(detail) THEN json_extract(detail, '$.` + member + `') END`
}

func (b *builder) flows() {
	day := []any{b.from, b.to}
	b.count(prefixed("msg."), `SELECT kind, COUNT(*) FROM message WHERE created_at_ms >= ?1 AND created_at_ms < ?2 GROUP BY kind`, day...)
	b.count(prefixed("ask_pair."),
		`SELECT state, COUNT(*) FROM obligation WHERE settled_at_ms >= ?1 AND settled_at_ms < ?2 GROUP BY state`, day...)
	b.pickups()
	b.count(fixed("notice.reserved"),
		`SELECT '', COUNT(*) FROM publish_attempt WHERE reserved_at_ms >= ?1 AND reserved_at_ms < ?2`, day...)
	b.count(prefixed("notice.resolved."),
		`SELECT state, COUNT(*) FROM publish_attempt WHERE resolved_at_ms >= ?1 AND resolved_at_ms < ?2 GROUP BY state`, day...)
	b.count(prefixed("notice.observed."),
		`SELECT observation, COUNT(*) FROM publish_attempt WHERE observed_at_ms >= ?1 AND observed_at_ms < ?2 GROUP BY observation`, day...)
	b.count(fixed("notice.retired_unobserved"), `SELECT '', COUNT(*) FROM publish_attempt
		WHERE retired_at_ms >= ?1 AND retired_at_ms < ?2 AND (observed_at_ms IS NULL OR observed_at_ms > retired_at_ms)`, day...)
	b.unobserved()
	b.count(prefixed("receipt.rejected."), `SELECT `+detail("reason")+`, COUNT(*) FROM event
		WHERE kind = 'publish_receipt_rejected' AND at_ms >= ?1 AND at_ms < ?2 GROUP BY 1`, day...)
	b.count(prefixed("suspend."), `SELECT reason || CASE WHEN until_ms IS NULL THEN '.lasting' ELSE '.timed' END, COUNT(*)
		FROM publish_suspension WHERE at_ms >= ?1 AND at_ms < ?2 GROUP BY 1`, day...)
	b.count(prefixed("turn_end.claim."),
		`SELECT adapter, COUNT(*) FROM turn_end_notice WHERE at_ms >= ?1 AND at_ms < ?2 GROUP BY adapter`, day...)
	b.count(func(by string) string {
		if by == "hook" {
			return "join.hook"
		}
		return "join.cli"
	}, `SELECT `+detail("by")+`, COUNT(*) FROM event WHERE kind = 'joined' AND at_ms >= ?1 AND at_ms < ?2 GROUP BY 1`, day...)
	b.count(fixed("join.rejoined"), `SELECT '', COUNT(*) FROM event WHERE kind = 'rejoined' AND at_ms >= ?1 AND at_ms < ?2`, day...)
	b.count(fixed("budget.exhausted"),
		`SELECT '', COUNT(*) FROM event WHERE kind IN ('budget_exhausted', 'loop_guard_stopped') AND at_ms >= ?1 AND at_ms < ?2`, day...)
}

// pickups buckets the lag from an ask to each recipient's pickup, on the day
// of the pickup.
func (b *builder) pickups() {
	if b.err != nil {
		return
	}
	rows, err := b.tx.QueryContext(b.ctx, `SELECT r.picked_up_at_ms - m.created_at_ms FROM recipient r
		JOIN message m ON m.id = r.message_id
		WHERE m.kind = 'ask' AND r.picked_up_at_ms >= ?1 AND r.picked_up_at_ms < ?2`, b.from, b.to)
	if err != nil {
		b.err = err
		return
	}
	defer rows.Close()
	for rows.Next() {
		var lag int64
		if b.err = rows.Scan(&lag); b.err != nil {
			return
		}
		b.counts["pickup_lag."+bucket(pickupBuckets, pickupBounds, lag)]++
	}
	b.err = rows.Err()
}

// unobserved counts, on the day of each attempt's boundary T (its submission,
// or else its reservation, plus ten minutes), the attempts that as of T were
// submitted or uncertain, neither observed nor retired, and still carried an
// item whose recipient had not picked it up and whose obligation, if any,
// was not settled. notice.late_observed counts the same attempts on the day
// they were observed after T, so it is a part of notice.unobserved_10m.
// Every condition compares stored times with T, so the counts do not depend
// on when they are taken.
func (b *builder) unobserved() {
	boundary := `COALESCE(a.submitting_at_ms, a.reserved_at_ms) + ` + strconv.FormatInt(unobservedAfter.Milliseconds(), 10)
	outstanding := `a.state IN ('submitted', 'uncertain') AND a.resolved_at_ms <= ` + boundary + `
		AND (a.retired_at_ms IS NULL OR a.retired_at_ms > ` + boundary + `)
		AND EXISTS (SELECT 1 FROM publish_attempt_item i
			JOIN recipient r ON r.message_id = i.message_id AND r.principal_id = i.principal_id
			LEFT JOIN obligation o ON o.message_id = i.message_id AND o.principal_id = i.principal_id
			WHERE i.attempt_id = a.id
				AND (r.picked_up_at_ms IS NULL OR r.picked_up_at_ms > ` + boundary + `)
				AND (o.settled_at_ms IS NULL OR o.settled_at_ms > ` + boundary + `))`
	b.count(fixed("notice.unobserved_10m"), `SELECT '', COUNT(*) FROM publish_attempt a
		WHERE `+boundary+` >= ?1 AND `+boundary+` < ?2 AND (a.observed_at_ms IS NULL OR a.observed_at_ms > `+boundary+`)
			AND `+outstanding, b.from, b.to)
	b.count(fixed("notice.late_observed"), `SELECT '', COUNT(*) FROM publish_attempt a
		WHERE a.observed_at_ms >= ?1 AND a.observed_at_ms < ?2 AND a.observed_at_ms > `+boundary+` AND `+outstanding, b.from, b.to)
}

// snapshot adds the figures that describe the hall at now and returns the
// switches.
func (b *builder) snapshot(now time.Time) *Switches {
	b.reach(now)
	b.count(prefixed("snap.build_stopped."), `SELECT vendor, COUNT(*) FROM capability_build cb
		WHERE cap = 'publish' AND `+store.CapabilityBuildStopped("cb")+` GROUP BY vendor`)
	b.count(fixed("snap.unread_ask_pairs"), `SELECT '', COUNT(*) FROM obligation o
		JOIN recipient r ON r.message_id = o.message_id AND r.principal_id = o.principal_id
		WHERE o.state = 'open' AND r.picked_up_at_ms IS NULL`)
	b.liveLaunches()
	sw := &Switches{PublishHall: "on", TurnEndClaude: "on"}
	if b.err == nil {
		var hall, turnEnd bool
		b.err = b.tx.QueryRowContext(b.ctx, `SELECT
			COALESCE((SELECT enabled FROM hall_publish WHERE id = 1), 1) = 1, `+store.TurnEndSwitchOn("'claude'")).
			Scan(&hall, &turnEnd)
		if !hall {
			sw.PublishHall = "off"
		}
		if !turnEnd {
			sw.TurnEndClaude = "off"
		}
	}
	return sw
}

func (b *builder) reach(now time.Time) {
	if b.err != nil {
		return
	}
	var bindings []int64
	rows, err := b.tx.QueryContext(b.ctx, `SELECT id FROM binding WHERE state = 'live' ORDER BY id`)
	if err != nil {
		b.err = err
		return
	}
	for rows.Next() {
		var id int64
		if b.err = rows.Scan(&id); b.err != nil {
			rows.Close()
			return
		}
		bindings = append(bindings, id)
	}
	rows.Close()
	if b.err = rows.Err(); b.err != nil {
		return
	}
	for _, id := range bindings {
		state, found, err := store.ReadPublishState(b.ctx, b.tx, id, now)
		if err != nil {
			b.err = err
			return
		}
		if found && slices.Contains(reachStates, state.State) {
			b.counts["snap.reach."+state.State]++
		}
	}
}

// liveLaunches counts the current, unended launch of each member with a live
// binding, doctor's population, by its runtime version family. Only Claude
// Code's hooks verify a launch's version; for an unverified launch the
// family comes from the version identity resolution recorded for the
// member's session.
func (b *builder) liveLaunches() {
	if b.err != nil {
		return
	}
	rows, err := b.tx.QueryContext(b.ctx, `SELECT json_extract(l.profile, '$.adapter'),
		CASE WHEN COALESCE(json_extract(l.profile, '$.vendor_version'), 'unverified') IN ('', 'unverified')
			THEN p.vendor_version ELSE json_extract(l.profile, '$.vendor_version') END
		FROM adapter_launch l JOIN principal p ON p.id = l.principal_id
		WHERE l.ended_hook_at_ms IS NULL AND json_valid(l.profile)
			AND l.rowid = (SELECT current.rowid FROM adapter_launch current
				WHERE current.principal_id = l.principal_id
				ORDER BY current.ended_hook_at_ms IS NOT NULL, current.started_hook_at_ms DESC, current.rowid DESC LIMIT 1)
			AND EXISTS (SELECT 1 FROM binding WHERE principal_id = l.principal_id AND state = 'live')`)
	if err != nil {
		b.err = err
		return
	}
	defer rows.Close()
	for rows.Next() {
		var adapter, version sql.NullString
		if b.err = rows.Scan(&adapter, &version); b.err != nil {
			return
		}
		if slices.Contains(adapters, adapter.String) {
			b.counts["snap.live_launch."+adapter.String+"."+family(adapter.String, version.String)]++
		}
	}
	b.err = rows.Err()
}
