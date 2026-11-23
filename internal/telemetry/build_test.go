package telemetry

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/hook"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

var update = flag.Bool("update", false, "rewrite testdata/report-v1.json")

// The fixture's text columns carry this marker wherever a name, a body, a
// path or a reference goes, so a leak shows in the report bytes.
const marker = "zqxmarker"

var day = time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC)

func at(days, hour, minute int) int64 {
	return day.AddDate(0, 0, days).Add(time.Duration(hour)*time.Hour + time.Duration(minute)*time.Minute).UnixMilli()
}

// The last line has no newline and is skipped, as doctor skips it; so is
// one whose time is not a number.
const fixtureHooks = "claude SessionStart clean 12\n" +
	"claude Stop clean 4\n" +
	"claude Stop deadline 151\n" +
	"claude StopFailure clean 30\n" +
	"codex SessionStart no_dispatcher 3\n" +
	"agy SessionStart rpc 40\n" +
	"claude Stop clean 1.5\n" +
	"claude Stop clean 9"

var fixtureSQL = []string{
	`INSERT INTO channel (id, name, topic, created_at_ms) VALUES
		(1, 'zqxmarker-channel', 'zqxmarker topic', 0), (2, 'zqxmarker-other', '', 0)`,
	`INSERT INTO principal (id, kind, vendor, vendor_version, created_at_ms) VALUES
		(1, 'owner', 'owner', NULL, 0), (2, 'session', 'claude', '2.1.330', 0),
		(3, 'session', 'codex', 'zqxmarker-version', 0)`,
	`INSERT INTO binding (id, principal_id, channel_id, alias, joined_at_ms, focus, reach_told) VALUES
		(1, 2, 1, 'zqxmarker-alias', 0, 'zqxmarker focus', 1), (2, 3, 1, 'zqxmarker-alias2', 0, '', NULL)`,
	// m1 ask answered by m2; m3 a note from the day before; m4 an ask left
	// open; m5 an ask from the day before, picked up on the day.
	fmt.Sprintf(`INSERT INTO message (id, channel_id, sender_id, kind, outcome, in_reply_to, audience_mode, due_at_ms, body, created_at_ms) VALUES
		(1, 1, 2, 'ask', NULL, NULL, 'explicit', %d, 'zqxmarker body', %d),
		(2, 1, 3, 'reply', 'answer', 1, 'explicit', NULL, 'zqxmarker reply', %d),
		(3, 1, 2, 'note', NULL, NULL, 'all', NULL, 'zqxmarker note', %d),
		(4, 1, 2, 'ask', NULL, NULL, 'explicit', %d, 'zqxmarker open', %d),
		(5, 1, 3, 'ask', NULL, NULL, 'explicit', NULL, 'zqxmarker late', %d)`,
		at(0, 11, 0), at(0, 10, 0), at(0, 10, 30), at(-1, 23, 59), at(1, 0, 30), at(0, 12, 0), at(-1, 22, 0)),
	fmt.Sprintf(`INSERT INTO recipient (message_id, principal_id, alias_at_commit, owes, picked_up_at_ms) VALUES
		(1, 3, 'zqxmarker-alias2', 1, %d), (2, 2, 'zqxmarker-alias', 0, %d),
		(3, 3, 'zqxmarker-alias2', 0, %d), (4, 3, 'zqxmarker-alias2', 1, NULL), (5, 2, 'zqxmarker-alias', 1, %d)`,
		at(0, 10, 0)+5_000, at(1, 0, 10), at(0, 0, 1), at(0, 0, 30)),
	fmt.Sprintf(`INSERT INTO obligation (message_id, principal_id, state, settled_at_ms) VALUES
		(1, 3, 'replied', %d), (4, 3, 'open', NULL), (5, 2, 'open', NULL)`, at(0, 10, 30)),
	fmt.Sprintf(`INSERT INTO op (principal_id, op_key, scope, verb, payload_hash, result, at_ms) VALUES
		(2, 'zqxmarker-k1', 'channel:zqxmarker', 'read', 'zqxmarker', '{"note":"zqxmarker"}', %d)`, at(0, 10, 6)),
	fmt.Sprintf(`INSERT INTO event (kind, subject, actor_id, detail, at_ms) VALUES
		('joined', 'channel:zqxmarker', 2, '{"alias":"zqxmarker","by":"hook"}', %[1]d),
		('joined', 'channel:zqxmarker', 3, '{"alias":"zqxmarker"}', %[1]d),
		('rejoined', 'binding:1', 2, '{"alias":"zqxmarker"}', %[1]d),
		('budget_exhausted', 'exchange:1', 2, '{}', %[1]d),
		('publish_receipt_rejected', 'binding:1', 2, '{"reason":"stale"}', %[1]d),
		('publish_receipt_rejected', 'binding:1', 2, '{"reason":"zqxmarker"}', %[1]d),
		('publish_switch', 'binding:1', 2, '{"by":"member","switch":"off"}', %[1]d),
		('joined', 'channel:zqxmarker', 2, '{"alias":"zqxmarker","by":"hook"}', %[2]d)`,
		at(0, 9, 0), at(1, 9, 0)),
	fmt.Sprintf(`INSERT INTO adapter_launch (principal_id, launch_ref, profile, verified, started_hook_at_ms, pid_namespace) VALUES
		(2, 'zqxmarker-l1', '{"adapter":"claude","manifest_rev":"claude-2","vendor_version":"2.1.330","platform":"darwin","mode":"interactive"}', 1, %d, 'zqxmarker'),
		(3, 'zqxmarker-l2', '{"adapter":"codex","manifest_rev":"codex-1","vendor_version":"zqxmarker","platform":"zqxmarker","mode":"zqxmarker"}', 0, %d, '')`,
		at(0, 9, 0), at(0, 9, 0)),
	fmt.Sprintf(`INSERT INTO turn (principal_id, launch_ref, turn_ref, opened_hook_at_ms) VALUES
		(2, 'zqxmarker-l1', 'zqxmarker-t1', %d)`, at(0, 10, 0)),
	fmt.Sprintf(`INSERT INTO turn_end_notice (principal_id, adapter, launch_ref, turn_ref, at_ms) VALUES
		(2, 'claude', 'zqxmarker-l1', 'zqxmarker-t1', %d)`, at(0, 10, 1)),
	// a1 crosses midnight: reserved the day before, resolved on the day,
	// observed the day after, and its item unread until then: unobserved at
	// its boundary 00:09 on the day, observed late the day after. a2 is
	// retired unobserved; a3 is observed in time, absorbed.
	fmt.Sprintf(`INSERT INTO publish_attempt (id, binding_id, principal_id, binding_epoch, generation, adapter, session_ref,
		launch_ref, policy_epoch, process_pid, process_start_ms, state, reserved_at_ms, resolved_at_ms, observed_at_ms,
		observation, retired_at_ms) VALUES
		(1, 1, 2, 1, 1, 'claude', 'zqxmarker', 'zqxmarker-l1', 1, 4242, 1, 'submitted', %d, %d, %d, 'own_turn', NULL),
		(2, 1, 2, 1, 1, 'claude', 'zqxmarker', 'zqxmarker-l1', 1, 4242, 1, 'not_submitted', %d, %d, NULL, NULL, %d),
		(3, 1, 2, 1, 1, 'claude', 'zqxmarker', 'zqxmarker-l1', 1, 4242, 1, 'submitted', %d, %d, %d, 'absorbed', NULL)`,
		at(-1, 23, 59), at(0, 0, 0), at(1, 0, 5),
		at(0, 10, 0), at(0, 10, 0), at(0, 10, 10),
		at(0, 10, 20), at(0, 10, 20), at(0, 10, 21)),
	`INSERT INTO publish_attempt_item (attempt_id, principal_id, message_id) VALUES (1, 2, 2), (3, 2, 2)`,
	fmt.Sprintf(`INSERT INTO publish_suspension (binding_id, launch_ref, reason, notice, at_ms, until_ms) VALUES
		(1, 'zqxmarker-l1', 'absorbed', 3, %d, %d), (1, 'zqxmarker-l1', 'unknown', 3, %d, NULL)`,
		at(0, 10, 21), at(0, 10, 51), at(0, 12, 0)),
	fmt.Sprintf(`INSERT INTO publish_route (binding_id, principal_id, generation, adapter, session_ref, launch_ref,
		launch_verified, policy_epoch, mode, socket_path, thread_id, watermark_message_id, state, reason, registered_at_ms, updated_at_ms)
		VALUES (1, 2, 1, 'claude', 'zqxmarker', 'zqxmarker-l1', 1, 1, 'interactive', '/zqxmarker/socket', 'zqxmarker', 0, 'active',
		'zqxmarker reason', %[1]d, %[1]d)`, at(0, 9, 0)),
	`INSERT INTO capability_build (vendor, vendor_version, platform, mode, manifest_rev, cap, confirmed, stopped_at_ms, stop_reason, stop_ref)
		VALUES ('claude', '2.1.330', 'darwin', 'interactive', 'claude-2', 'publish', 1, 1, 'absorbed', 3)`,
	`INSERT INTO adapter_policy (adapter, enabled, reminder, epoch, changed_at_ms, turn_end) VALUES
		('claude', 1, 1, 1, 0, 0), ('codex', 0, 0, 1, 0, 0)`,
	`INSERT INTO hall_publish (id, enabled, changed_at_ms) VALUES (1, 1, 0)`,
}

func openFixture(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	exec(t, s, fixtureSQL...)
	return s
}

func exec(t *testing.T, s *store.Store, statements ...string) {
	t.Helper()
	err := s.Update(t.Context(), func(tx store.Tx) error {
		for _, q := range statements {
			if _, err := tx.ExecContext(t.Context(), q); err != nil {
				return fmt.Errorf("%s: %w", q, err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// fixtureInput builds the fixture day one minute after it closed, so the
// report carries the snapshot.
func fixtureInput() Input {
	return Input{
		Day: day.Add(13 * time.Hour), Batch: "9f1c2e4a7b3d5f60812a4c6e8b0d2f41", Version: "v0.2.0-rc.2",
		Now: day.Add(24*time.Hour + time.Minute), Hooks: []byte(fixtureHooks), HooksRead: true,
		WeekActiveDays: 3,
	}
}

func buildReport(t *testing.T, s *store.Store, in Input) Report {
	t.Helper()
	var r Report
	err := s.Update(t.Context(), func(tx store.Tx) error {
		var err error
		r, err = Build(t.Context(), tx, in)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	// Golden bytes do not depend on the machine that runs the test.
	r.App.Platform = "darwin-arm64"
	return r
}

func build(t *testing.T, s *store.Store, in Input) []byte {
	t.Helper()
	raw, err := buildReport(t, s, in).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestBuildMatchesGoldenReport(t *testing.T) {
	got := build(t, openFixture(t), fixtureInput())
	golden := filepath.Join("testdata", "report-v1.json")
	if *update {
		if err := os.WriteFile(golden, append(got, '\n'), 0644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, bytes.TrimSuffix(want, []byte("\n"))) {
		t.Fatalf("report differs from %s (rerun with -update and review the diff):\n got %s\nwant %s", golden, got, want)
	}
	if err := Validate(got, day.AddDate(0, 0, 1)); err != nil {
		t.Fatalf("golden report fails validation: %v", err)
	}
}

// An attempt reserved before midnight, resolved after it and observed a day
// later counts once on each of its three days, and a day already built does
// not change when later days get records.
func TestFlowsCountOnTheirOwnDay(t *testing.T) {
	s := openFixture(t)
	late := func(days int) Input {
		in := fixtureInput()
		in.Day, in.Now, in.WeekActiveDays = day.AddDate(0, 0, days), day.AddDate(0, 0, 9), 0
		in.Hooks, in.HooksRead = nil, false
		return in
	}
	before, on, after := buildReport(t, s, late(-1)), buildReport(t, s, late(0)), buildReport(t, s, late(1))
	for _, c := range []struct {
		r    Report
		key  string
		want int64
	}{
		{before, "notice.reserved", 1}, {on, "notice.reserved", 2}, {after, "notice.reserved", 0},
		{before, "notice.resolved.submitted", 0}, {on, "notice.resolved.submitted", 2}, {after, "notice.resolved.submitted", 0},
		{before, "notice.observed.own_turn", 0}, {on, "notice.observed.own_turn", 0}, {after, "notice.observed.own_turn", 1},
		{on, "notice.unobserved_10m", 1}, {after, "notice.unobserved_10m", 0},
		{on, "notice.late_observed", 0}, {after, "notice.late_observed", 1},
		{before, "msg.note", 1}, {on, "msg.note", 0},
		{on, "join.hook", 1}, {after, "join.hook", 1},
	} {
		if got := c.r.Counts[c.key]; got != c.want {
			t.Errorf("%s on %s = %d, want %d", c.key, c.r.Day, got, c.want)
		}
	}
	first := build(t, s, late(0))
	exec(t, s, fmt.Sprintf(`UPDATE publish_attempt SET observed_at_ms = %d, observation = 'unknown' WHERE id = 2`, at(1, 3, 0)),
		fmt.Sprintf(`INSERT INTO message (channel_id, sender_id, kind, audience_mode, body, created_at_ms)
			VALUES (1, 2, 'note', 'all', 'zqxmarker', %d)`, at(1, 4, 0)))
	if again := build(t, s, late(0)); !bytes.Equal(first, again) {
		t.Fatalf("the day changed after later records:\n%s\n%s", first, again)
	}
}

// The unobserved count compares stored times with each attempt's boundary,
// so the batch built at the day's close and one built after a restart a day
// later agree.
func TestUnobservedCountDoesNotDependOnWhenItIsTaken(t *testing.T) {
	s := openFixture(t)
	atClose := buildReport(t, s, fixtureInput())
	in := fixtureInput()
	in.Now = day.AddDate(0, 0, 2).Add(5 * time.Hour)
	dayLate := buildReport(t, s, in)
	if atClose.Counts["notice.unobserved_10m"] != 1 || dayLate.Counts["notice.unobserved_10m"] != 1 {
		t.Fatalf("unobserved_10m %d at close, %d a day late; want 1 and 1",
			atClose.Counts["notice.unobserved_10m"], dayLate.Counts["notice.unobserved_10m"])
	}
	for key, n := range atClose.Counts {
		if !strings.HasPrefix(key, "snap.") && dayLate.Counts[key] != n {
			t.Errorf("%s: %d at close, %d a day late", key, n, dayLate.Counts[key])
		}
	}
}

func TestSnapshotOnlyWithinTenMinutesOfTheClose(t *testing.T) {
	s := openFixture(t)
	end := day.AddDate(0, 0, 1)
	for _, c := range []struct {
		now  time.Time
		want string
	}{
		{end.Add(-time.Second), "none"},
		{end, "taken"},
		{end.Add(SnapshotWindow - time.Millisecond), "taken"},
		{end.Add(SnapshotWindow), "none"},
		{end.Add(48 * time.Hour), "none"},
	} {
		in := fixtureInput()
		in.Now = c.now
		r := buildReport(t, s, in)
		if r.Sources.Snapshot != c.want || (r.Switches != nil) != (c.want == "taken") {
			t.Errorf("built at %s: snapshot %s, switches %v; want %s", c.now, r.Sources.Snapshot, r.Switches, c.want)
		}
		for key := range r.Counts {
			if c.want == "none" && strings.HasPrefix(key, "snap.") {
				t.Errorf("built at %s: %s without a snapshot", c.now, key)
			}
		}
	}
}

func TestMissingHookFileSendsNoHookKeys(t *testing.T) {
	in := fixtureInput()
	in.Hooks, in.HooksRead = nil, false
	r := buildReport(t, openFixture(t), in)
	if r.Sources.Hooks != "missing" {
		t.Fatalf("sources.hooks = %s", r.Sources.Hooks)
	}
	for key := range r.Counts {
		if strings.HasPrefix(key, "hook") {
			t.Fatalf("hook count %s without a hook file", key)
		}
	}
}

func TestReadHookFile(t *testing.T) {
	dir := t.TempDir()
	rv, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer rv.Close()
	if _, ok := ReadHookFile(rv, day); ok {
		t.Fatal("read a hook file that does not exist")
	}
	if err := os.Mkdir(filepath.Join(dir, "hooks"), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "hooks", day.Format("20060102"))
	if err := os.WriteFile(path, []byte(fixtureHooks), 0600); err != nil {
		t.Fatal(err)
	}
	if raw, ok := ReadHookFile(rv, day.Add(time.Hour)); !ok || string(raw) != fixtureHooks {
		t.Fatalf("read %q, %v", raw, ok)
	}
	if err := os.WriteFile(path, bytes.Repeat([]byte("claude Stop clean 1\n"), maxHookFile/20+1), 0600); err != nil {
		t.Fatal(err)
	}
	if _, ok := ReadHookFile(rv, day); ok {
		t.Fatal("read a hook file over the size cap")
	}
}

func TestHookBucketsFollowTheHookDeadline(t *testing.T) {
	if hook.Budget != 150*time.Millisecond || hookMSBucket(149) != "lt150" || hookMSBucket(150) != "ge150" {
		t.Fatalf("hook deadline %v: the lt150/ge150 buckets no longer split at it", hook.Budget)
	}
}

// TestNoMarkerReachesTheReport writes a marker into one text column at a
// time, in every row the schema lets it into, and builds the report each
// time; a JSON column is marked one member at a time so the rest of the row
// still counts. The hook file gets marked records too. No marker may reach
// a report.
func TestNoMarkerReachesTheReport(t *testing.T) {
	s := openFixture(t)
	in := fixtureInput()
	in.Hooks = []byte(fixtureHooks + "\n" + marker + " Stop clean 1\nclaude " + marker + " clean 1\n" +
		"claude Stop " + marker + " 1\nclaude Stop clean " + marker + "\n")
	check := func(what string, raw []byte) {
		t.Helper()
		if bytes.Contains(bytes.ToLower(raw), []byte(marker)) {
			t.Errorf("%s: marker reached the report: %s", what, raw)
		}
		if err := Validate(raw, day.AddDate(0, 0, 1)); err != nil {
			t.Errorf("%s: report fails validation: %v", what, err)
		}
	}
	check("fixture", build(t, s, in))
	marked := make(map[string]bool)
	err := s.Update(t.Context(), func(tx store.Tx) error {
		ctx := t.Context()
		tables, err := texts(tx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
		if err != nil {
			return err
		}
		for _, table := range tables {
			columns, err := texts(tx, `SELECT name FROM pragma_table_info(?) WHERE type NOT LIKE 'INTEGER%'`, table)
			if err != nil {
				return err
			}
			for _, column := range columns {
				members, err := texts(tx, fmt.Sprintf(`SELECT DISTINCT j.key FROM %q t, json_each(t.%q) j
					WHERE json_valid(t.%[2]q) AND json_type(t.%[2]q) = 'object' AND j.type = 'text' ORDER BY j.key`, table, column))
				if err != nil {
					return err
				}
				// Each row gets its own value, so a unique column takes it.
				updates := map[string]string{table + "." + column: fmt.Sprintf(`UPDATE %q SET %q = ? || '-' || rowid
					WHERE NOT json_valid(%[2]q) OR json_type(%[2]q) <> 'object'`, table, column)}
				for _, m := range members {
					updates[table+"."+column+"."+m] = fmt.Sprintf(`UPDATE %q SET %q = json_set(%[2]q, '$.' || json_quote(%q), ? || '-' || rowid)
						WHERE json_valid(%[2]q) AND json_type(%[2]q, '$.' || json_quote(%[3]q)) = 'text'`, table, column, m)
				}
				for name, update := range updates {
					if _, err := tx.ExecContext(ctx, `SAVEPOINT mark`); err != nil {
						return err
					}
					// A refused update means the schema admits only its own
					// values in that column.
					if res, err := tx.ExecContext(ctx, update, marker+"-"+name); err == nil {
						if n, _ := res.RowsAffected(); n > 0 {
							marked[name] = true
							r, err := Build(ctx, tx, in)
							if err != nil {
								return fmt.Errorf("%s: %w", name, err)
							}
							r.App.Platform = "darwin-arm64"
							raw, err := r.Marshal()
							if err != nil {
								return err
							}
							check(name, raw)
						}
					}
					if _, err := tx.ExecContext(ctx, `ROLLBACK TO mark`); err != nil {
						return err
					}
					if _, err := tx.ExecContext(ctx, `RELEASE mark`); err != nil {
						return err
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"channel.name", "channel.topic", "binding.alias", "binding.focus", "message.body",
		"recipient.alias_at_commit", "op.verb", "op.scope", "op.result.note", "event.kind", "event.subject",
		"event.detail.alias", "event.detail.by", "event.detail.reason", "adapter_launch.profile.vendor_version",
		"adapter_launch.profile.mode", "adapter_launch.profile.manifest_rev", "adapter_launch.profile.adapter",
		"adapter_launch.pid_namespace", "principal.vendor", "principal.vendor_version", "publish_attempt.adapter",
		"publish_attempt.session_ref", "publish_route.socket_path", "publish_route.reason", "turn_end_notice.adapter",
		"capability_build.vendor", "capability_build.vendor_version", "capability_build.stop_reason"} {
		if !marked[name] {
			t.Errorf("the canary did not mark %s", name)
		}
	}
}

func texts(tx store.Tx, query string, args ...any) ([]string, error) {
	rows, err := tx.QueryContext(context.Background(), query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s sql.NullString
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s.String)
	}
	return out, rows.Err()
}

func TestEncodingIsCanonical(t *testing.T) {
	r := Report{V: 1, Registry: 1, Batch: "9f1c2e4a7b3d5f60812a4c6e8b0d2f41", Day: "2026-11-25",
		App:     App{Version: "v0.2.0-rc.2", Platform: "linux-amd64"},
		Sources: Sources{Hooks: "ok", Snapshot: "none"},
		Counts:  map[string]int64{"msg.note": 1, "hook.claude.stop.clean": 2, "budget.exhausted": 5}}
	raw, err := r.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	want := `{"v":1,"registry":1,"batch":"9f1c2e4a7b3d5f60812a4c6e8b0d2f41","day":"2026-11-25",` +
		`"app":{"version":"v0.2.0-rc.2","platform":"linux-amd64"},"sources":{"hooks":"ok","snapshot":"none"},` +
		`"counts":{"budget.exhausted":5,"hook.claude.stop.clean":2,"msg.note":1}}`
	if string(raw) != want {
		t.Fatalf("got  %s\nwant %s", raw, want)
	}
	var back map[string]any
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
}
