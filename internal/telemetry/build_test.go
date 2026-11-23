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
		(3, 'session', 'codex', '0.171.0', 0), (4, 'session', 'agy', 'ls:cli-1.5.13', 0)`,
	`INSERT INTO binding (id, principal_id, channel_id, alias, joined_at_ms, focus, reach_told) VALUES
		(1, 2, 1, 'zqxmarker-alias', 0, 'zqxmarker focus', 1), (2, 3, 1, 'zqxmarker-alias2', 0, '', NULL),
		(3, 4, 1, 'zqxmarker-alias3', 0, '', NULL)`,
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
		('send_wake', 'message:1', 2, '{"wake":"notice"}', %[1]d),
		('send_wake', 'message:1', 2, '{"wake":"none","why":"suspended"}', %[1]d),
		('send_wake', 'message:4', 2, '{"wake":"none","why":"no_route"}', %[1]d),
		('send_wake', 'message:4', 2, '{"wake":"zqxmarker"}', %[1]d),
		('send_wake', 'message:5', 2, '{"wake":"none","why":"off"}', %[2]d),
		('joined', 'channel:zqxmarker', 2, '{"alias":"zqxmarker","by":"hook"}', %[2]d)`,
		at(0, 9, 0), at(1, 9, 0)),
	fmt.Sprintf(`INSERT INTO adapter_launch (principal_id, launch_ref, profile, verified, started_hook_at_ms, pid_namespace) VALUES
		(2, 'zqxmarker-l1', '{"adapter":"claude","manifest_rev":"claude-2","vendor_version":"2.1.330","platform":"darwin","mode":"interactive"}', 1, %d, 'zqxmarker'),
		(3, 'zqxmarker-l2', '{"adapter":"codex","manifest_rev":"codex-1","vendor_version":"unverified","platform":"zqxmarker","mode":"unverified"}', 0, %d, ''),
		(4, 'zqxmarker-l3', '{"adapter":"agy","manifest_rev":"agy-1","vendor_version":"unverified","platform":"zqxmarker","mode":"unverified"}', 0, %d, '')`,
		at(0, 9, 0), at(0, 9, 0), at(0, 9, 0)),
	fmt.Sprintf(`INSERT INTO turn (principal_id, launch_ref, turn_ref, opened_hook_at_ms) VALUES
		(2, 'zqxmarker-l1', 'zqxmarker-t1', %d)`, at(0, 10, 0)),
	fmt.Sprintf(`INSERT INTO turn_end_notice (principal_id, adapter, launch_ref, turn_ref, at_ms) VALUES
		(2, 'claude', 'zqxmarker-l1', 'zqxmarker-t1', %d)`, at(0, 10, 1)),
	// a1 crosses midnight: reserved the day before, resolved on the day,
	// observed the day after, and its item unread until then: unobserved at
	// its boundary 00:09 on the day, observed late the day after. a2 is
	// retired unobserved; a3 is observed in time, absorbed. a4 is observed
	// after its boundary, but its only item was picked up before it: neither
	// unobserved nor late.
	fmt.Sprintf(`INSERT INTO publish_attempt (id, binding_id, principal_id, binding_epoch, generation, adapter, session_ref,
		launch_ref, policy_epoch, process_pid, process_start_ms, state, reserved_at_ms, resolved_at_ms, observed_at_ms,
		observation, retired_at_ms) VALUES
		(1, 1, 2, 1, 1, 'claude', 'zqxmarker', 'zqxmarker-l1', 1, 4242, 1, 'submitted', %d, %d, %d, 'own_turn', NULL),
		(2, 1, 2, 1, 1, 'claude', 'zqxmarker', 'zqxmarker-l1', 1, 4242, 1, 'not_submitted', %d, %d, NULL, NULL, %d),
		(3, 1, 2, 1, 1, 'claude', 'zqxmarker', 'zqxmarker-l1', 1, 4242, 1, 'submitted', %d, %d, %d, 'absorbed', NULL),
		(4, 2, 3, 1, 1, 'codex', 'zqxmarker', 'zqxmarker-l2', 1, 4343, 1, 'submitted', %d, %d, %d, 'unknown', NULL)`,
		at(-1, 23, 59), at(0, 0, 0), at(1, 0, 5),
		at(0, 10, 0), at(0, 10, 0), at(0, 10, 10),
		at(0, 10, 20), at(0, 10, 20), at(0, 10, 21),
		at(0, 14, 0), at(0, 14, 0), at(0, 15, 0)),
	`INSERT INTO publish_attempt_item (attempt_id, principal_id, message_id) VALUES (1, 2, 2), (3, 2, 2), (4, 3, 1)`,
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
	// Every other table gets a row, so the canary reaches all of them.
	`INSERT INTO hall_auto_join (id, enabled, changed_at_ms) VALUES (1, 1, 0)`,
	`INSERT INTO exchange (id, channel_id, root_message_id, opened_by_id, granted) VALUES (1, 1, 1, 2, 8)`,
	`INSERT INTO budget_event (exchange_id, delta, reason, message_id, by_id, at_ms) VALUES (1, -1, 'debit', 1, 2, 0)`,
	`INSERT INTO announcement (principal_id, message_id, via, ref, at_ms) VALUES (3, 4, 'turn_end', 1, 0)`,
	`INSERT INTO capability (principal_id, cap, level, available, vendor_version, platform, mode, checked_at_ms)
		VALUES (3, 'observe', 'candidate', 0, 'zqxmarker', 'zqxmarker', 'zqxmarker', 0)`,
	`INSERT INTO capability_evidence (vendor, vendor_version, platform, mode, manifest_rev, cap, level, result_ref, checked_at_ms)
		VALUES ('codex', 'zqxmarker', 'zqxmarker', 'zqxmarker', 'codex-1', 'publish', 'tested', 'zqxmarker-ref', 0)`,
	`INSERT INTO delivery_attempt (message_set, principal_id, route, state, at_ms) VALUES ('zqxmarker', 3, 'zqxmarker', 'recorded', 0)`,
	`INSERT INTO principal_key (vendor, key, principal_id, created_at_ms) VALUES ('codex', 'zqxmarker-key', 3, 0)`,
	`INSERT INTO leaf (digest, vendor, key, principal_id, generation, file_identity, created_at_ms)
		VALUES ('` + strings.Repeat("ab", 32) + `', 'codex', 'zqxmarker-key', 3, 'zqxmarker', 'zqxmarker', 0)`,
	`INSERT INTO observation (principal_id, kind, origin, source, observed_at_ms, launch_ref, event_ref, turn_ref, profile)
		VALUES (3, 'command', 'unknown', 'zqxmarker', 0, 'zqxmarker-l2', 'zqxmarker', 'zqxmarker', '{"adapter":"zqxmarker"}')`,
	`INSERT INTO offer (id, principal_id, binding_id, generation, launch_ref, epoch, reason, evidence, at_ms)
		VALUES (1, 3, 2, 'zqxmarker', 'zqxmarker-l2', 0, 'prompt', 'zqxmarker', 0)`,
	`INSERT INTO offer_item (offer_id, message_id, principal_id) VALUES (1, 4, 3)`,
	`INSERT INTO pickup_receipt (token_hash, principal_id, generation, read_op_key, issued_at_ms, expires_at_ms, turn_ref, tool_call_ref)
		VALUES ('zqxmarker-token', 3, 'zqxmarker', 'zqxmarker-op', 0, 0, 'zqxmarker', 'zqxmarker')`,
	`INSERT INTO pickup_receipt_item (token_hash, message_id) VALUES ('zqxmarker-token', 4)`,
	`INSERT INTO receipt (message_id, principal_id, stage, seq, at_ms, evidence) VALUES (4, 3, 'stored', 1, 0, 'zqxmarker')`,
	`INSERT INTO repo_default (repo_root, channel_id) VALUES ('/zqxmarker/repo', 1)`,
	`INSERT INTO result_wait (message_id, asker_id, state) VALUES (4, 2, 'armed')`,
	`INSERT INTO roster_change (channel_id, seq, principal_id, field) VALUES (1, 1, 2, 'zqxmarker')`,
	`INSERT INTO waiter_lease (principal_id, lease_token, holder, armed_at_ms, renewed_at_ms, expires_at_ms, state, reason)
		VALUES (3, 'zqxmarker', 'zqxmarker', 0, 0, 0, 'live', 'zqxmarker')`,
	`INSERT INTO publish_route_proof (binding_id, generation, process_pid, process_start_ms) VALUES (1, 1, 4242, 1)`,
	`INSERT INTO member_host (principal_id, pid, start_identity, pid_namespace, seen_at_ms) VALUES (3, 4343, 1, 'zqxmarker', 0)`,
	`INSERT INTO thread_position (principal_id, thread_root_id, level_ms) VALUES (3, 1, 1)`,
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
		Now: day.Add(24*time.Hour + time.Minute), Hooks: CountHooks([]byte(fixtureHooks)), HooksRead: true,
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
		{on, "send.wake.none", 2}, {after, "send.wake.none", 1}, {on, "send.wake.notice", 1}, {after, "send.wake.notice", 0},
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
	in.Hooks = CountHooks([]byte(fixtureHooks + "\n" + marker + " Stop clean 1\nclaude " + marker + " clean 1\n" +
		"claude Stop " + marker + " 1\nclaude Stop clean " + marker + "\n"))
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
	// Every table must have a fixture row, and every text column must be
	// marked in at least one row or refused by the schema (an enum): a new
	// table or column fails here until the fixture reaches it.
	var empty, unreached []string
	err := s.Update(t.Context(), func(tx store.Tx) error {
		ctx := t.Context()
		tables, err := texts(tx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
		if err != nil {
			return err
		}
		for _, table := range tables {
			var rows int
			if err := tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM %q`, table)).Scan(&rows); err != nil {
				return err
			}
			if rows == 0 {
				empty = append(empty, table)
				continue
			}
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
				// A NULL is replaced too, unless the schema refuses it.
				updates := map[string]string{table + "." + column: fmt.Sprintf(`UPDATE %q SET %q = ? || '-' || rowid
					WHERE %[2]q IS NULL OR NOT json_valid(%[2]q) OR json_type(%[2]q) <> 'object'`, table, column)}
				for _, m := range members {
					updates[table+"."+column+"."+m] = fmt.Sprintf(`UPDATE %q SET %q = json_set(%[2]q, '$.' || json_quote(%q), ? || '-' || rowid)
						WHERE json_valid(%[2]q) AND json_type(%[2]q, '$.' || json_quote(%[3]q)) = 'text'`, table, column, m)
				}
				reached := false
				for name, update := range updates {
					if _, err := tx.ExecContext(ctx, `SAVEPOINT mark`); err != nil {
						return err
					}
					res, err := tx.ExecContext(ctx, update, marker+"-"+name)
					if err != nil {
						// The schema admits only its own values in that column.
						reached = true
					} else if n, _ := res.RowsAffected(); n > 0 {
						reached = true
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
					if _, err := tx.ExecContext(ctx, `ROLLBACK TO mark`); err != nil {
						return err
					}
					if _, err := tx.ExecContext(ctx, `RELEASE mark`); err != nil {
						return err
					}
				}
				if !reached {
					unreached = append(unreached, table+"."+column)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) > 0 {
		t.Errorf("tables without a fixture row, which the canary cannot reach: %v", empty)
	}
	if len(unreached) > 0 {
		t.Errorf("text columns neither marked nor refused by the schema: %v", unreached)
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
	r := Report{V: 1, Registry: 2, Batch: "9f1c2e4a7b3d5f60812a4c6e8b0d2f41", Day: "2026-11-25",
		App:     App{Version: "v0.2.0-rc.2", Platform: "linux-amd64"},
		Sources: Sources{Hooks: "ok", Snapshot: "none"},
		Counts:  map[string]int64{"msg.note": 1, "hook.claude.stop.clean": 2, "budget.exhausted": 5}}
	raw, err := r.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	want := `{"v":1,"registry":2,"batch":"9f1c2e4a7b3d5f60812a4c6e8b0d2f41","day":"2026-11-25",` +
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

// A hall where nothing happened still has its snapshot and the week
// figures; such a day is not active, and the fixture day is.
func TestIdleDayIsNotActive(t *testing.T) {
	s, err := store.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	exec(t, s, `INSERT INTO channel (id, name, created_at_ms) VALUES (1, 'idle', 0)`,
		`INSERT INTO principal (id, kind, vendor, vendor_version, created_at_ms) VALUES (1, 'session', 'claude', '2.1.330', 0)`,
		`INSERT INTO binding (id, principal_id, channel_id, alias, joined_at_ms) VALUES (1, 1, 1, 'idle', 0)`)
	in := fixtureInput()
	in.Hooks = nil
	r := buildReport(t, s, in)
	if len(r.Counts) == 0 || r.Active() {
		t.Fatalf("idle day: counts %v, active %v; want snapshot or week figures only and not active", r.Counts, r.Active())
	}
	if !buildReport(t, openFixture(t), fixtureInput()).Active() {
		t.Fatal("the fixture day is not active")
	}
}
