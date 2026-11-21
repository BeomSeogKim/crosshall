package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/cli"
	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

type handlerFunc func(context.Context, store.Tx, core.Caller, domain.Request) (core.Result, error)

func (f handlerFunc) Handle(ctx context.Context, tx store.Tx, caller core.Caller, req domain.Request) (core.Result, error) {
	return f(ctx, tx, caller, req)
}

type resultTransport struct{ result core.Result }

func (r resultTransport) Do(context.Context, cli.Call) (core.Result, error) { return r.result, nil }

func reviewServer(t *testing.T) *server {
	t.Helper()
	db, err := store.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &server{cfg: Config{Services: testServices()}, db: db}
}

func (s *server) fixtureSQL(t *testing.T, sql string) {
	t.Helper()
	if err := s.db.Update(t.Context(), func(tx store.Tx) error {
		_, err := tx.ExecContext(t.Context(), sql)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func (s *server) fixtureRequest(t *testing.T, arrival Arrival, req wire.Request) core.Result {
	t.Helper()
	req.Version = wire.Version
	req.Attempt = "attempt"
	if req.ExpiresAtMS == 0 {
		req.ExpiresAtMS = time.Now().Add(time.Second).UnixMilli()
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.command(t.Context(), arrival, req.Attempt, raw)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestUnknownOutcomeRendersOriginalKey(t *testing.T) {
	for _, scenario := range []string{"expired", "commit error"} {
		t.Run(scenario, func(t *testing.T) {
			s := reviewServer(t)
			s.fixtureSQL(t, `INSERT INTO principal (kind, vendor, created_at_ms) VALUES ('owner', 'owner', 1);
				INSERT INTO channel (name, created_at_ms) VALUES ('checkout', 1);`)
			s.cfg.Handle = handlerFunc(func(ctx context.Context, tx store.Tx, _ core.Caller, _ domain.Request) (core.Result, error) {
				if scenario == "expired" {
					t.Fatal("expired request reached the handler")
				}
				// A dangling deferred reference fails at Commit, after the handler returns.
				_, err := tx.ExecContext(ctx, `INSERT INTO exchange
					(channel_id, root_message_id, opened_by_id, granted) VALUES (1, 99, 1, 8)`)
				if err != nil {
					t.Fatalf("wanted a deferred constraint error at Commit, got %v", err)
				}
				return core.Result{Status: core.ExitOK}, nil
			})
			req := wire.Request{Verb: domain.VerbCreateChannel, OpKey: "keep-me"}
			if scenario == "expired" {
				req.ExpiresAtMS = time.Now().Add(-time.Second).UnixMilli()
			}
			result := s.fixtureRequest(t, Arrival{Owner: true}, req)
			if result.Code != core.CodeOutcomeUnknown || result.Status != core.ExitUnavailable {
				t.Fatalf("wanted uncertain result, got %+v", result)
			}
			env := cli.Env{Transport: resultTransport{result}}
			const hint = "retry with --key keep-me"
			var stdout, stderr bytes.Buffer
			args := []string{"owner", "channel", "create", "checkout"}
			if code := cli.RunEnv(t.Context(), env, args, &stdout, &stderr); code != core.ExitUnavailable ||
				!strings.Contains(stderr.String(), hint) {
				t.Fatalf("plain exit %d, stdout=%q stderr=%q", code, &stdout, &stderr)
			}
			stdout.Reset()
			stderr.Reset()
			if code := cli.RunEnv(t.Context(), env, append([]string{"--json"}, args...), &stdout, &stderr); code != core.ExitUnavailable {
				t.Fatalf("JSON exit %d, stdout=%q stderr=%q", code, &stdout, &stderr)
			}
			var response struct {
				Hint string `json:"hint"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &response); err != nil || response.Hint != hint {
				t.Fatalf("JSON response %q, parse error %v", &stdout, err)
			}
		})
	}
}

func TestKeylessChannelCreateWritesNothing(t *testing.T) {
	s := reviewServer(t)
	s.cfg.Handle = &domain.Service{}
	result := s.fixtureRequest(t, Arrival{Owner: true}, wire.Request{Verb: domain.VerbCreateChannel})
	if result.Status != core.ExitUsage {
		t.Fatalf("keyless mutation returned %+v", result)
	}
	if err := s.db.Update(t.Context(), func(tx store.Tx) error {
		var rows int
		if err := tx.QueryRowContext(t.Context(), "SELECT (SELECT count(*) FROM channel) + (SELECT count(*) FROM op)").Scan(&rows); err != nil {
			return err
		}
		if rows != 0 {
			t.Fatalf("keyless command wrote %d rows", rows)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRosterAckVisibleBeforeHandler(t *testing.T) {
	for _, refuse := range []bool{false, true} {
		name := "success"
		if refuse {
			name = "refusal"
		}
		t.Run(name, func(t *testing.T) {
			s := reviewServer(t)
			s.fixtureSQL(t, `INSERT INTO channel (name, created_at_ms) VALUES ('checkout', 1);
				INSERT INTO principal (kind, vendor, created_at_ms) VALUES ('session', 'route', 1);
				INSERT INTO binding (principal_id, channel_id, alias, joined_at_ms) VALUES (1, 1, 'form', 1);
				INSERT INTO roster_change (channel_id, seq, principal_id, field) VALUES (1, 1, 1, 'joined');`)
			bindingID := int64(1)
			s.cfg.Resolve = func(context.Context, store.Tx, Arrival, wire.Request) (Resolution, error) {
				return Resolution{Caller: core.Caller{Role: core.RoleMember, BindingID: &bindingID}}, nil
			}
			s.cfg.Decode = func(req wire.Request, _ Resolution) (domain.Request, error) {
				if req.Ack != 1 {
					t.Fatalf("decoder received unbounded cursor %d", req.Ack)
				}
				return domain.Request{OpKey: req.OpKey, Command: domain.Join{Channel: "checkout"}}, nil
			}
			s.cfg.Handle = handlerFunc(func(ctx context.Context, tx store.Tx, _ core.Caller, _ domain.Request) (core.Result, error) {
				var cursor int
				var hintAcked bool
				if err := tx.QueryRowContext(ctx, "SELECT roster_ack_seq, hint_acked FROM binding WHERE id = 1").Scan(&cursor, &hintAcked); err != nil {
					return core.Result{}, err
				}
				if refuse {
					_, err := tx.ExecContext(ctx, "INSERT INTO event(kind, subject, actor_id, detail, at_ms) VALUES ('refused', 'join', 1, '{}', 1)")
					return domain.Refuse(core.CodeOneChannel, "refused", ""), err
				}
				out := domain.JoinResult{Channel: "checkout", Restored: true}
				if !hintAcked {
					out.Hint = "first join"
				}
				if cursor == 0 {
					out.Members = []domain.Member{{Alias: "form"}}
				}
				return domain.OK(out)
			})
			out := s.fixtureRequest(t, Arrival{}, wire.Request{Verb: domain.VerbJoin, OpKey: "join", Ack: ^uint64(0)})
			if !refuse {
				var join domain.JoinResult
				if err := json.Unmarshal(out.Data, &join); out.Status != core.ExitOK || err != nil ||
					join.Hint != "" || len(join.Members) != 0 {
					t.Fatalf("response repeated acknowledged output: %+v, error %v", join, err)
				}
			}
			if err := s.db.Update(t.Context(), func(tx store.Tx) error {
				var cursor, events int
				var hinted bool
				if err := tx.QueryRowContext(t.Context(), "SELECT roster_ack_seq, hint_acked, (SELECT count(*) FROM event) FROM binding WHERE id = 1").Scan(&cursor, &hinted, &events); err != nil {
					return err
				}
				if refuse && (out.Status != core.ExitRefused || cursor != 0 || hinted || events != 1) {
					t.Fatalf("refusal persisted cursor %d, hint %t, events %d, status %d", cursor, hinted, events, out.Status)
				}
				if !refuse && (cursor != 1 || !hinted) {
					t.Fatalf("success did not persist cursor %d and hint %t", cursor, hinted)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestStartRefusesLinkedLog(t *testing.T) {
	root := t.TempDir()
	cfg := LaunchConfig{
		Version: "test", Owner: true, StateRoot: filepath.Join(root, "state"),
		DefaultStateRoot: filepath.Join(root, "default"), Rendezvous: filepath.Join(root, "rendezvous"),
	}
	if err := os.Mkdir(cfg.StateRoot, 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(cfg.StateRoot, "keep")
	const original = "keep"
	if err := os.WriteFile(target, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("keep", filepath.Join(cfg.StateRoot, "dispatcher.log")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if marker := readReady(cfg.Rendezvous); marker.PID > 0 {
			syscall.Kill(marker.PID, syscall.SIGTERM)
			waitFor(t, func() bool { return readReady(cfg.Rendezvous).Instance == "" })
		}
	})
	if spawned, err := cfg.Start(t.Context()); spawned || err == nil {
		t.Fatalf("linked log was accepted: spawned=%t, err=%v", spawned, err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != original {
		t.Fatalf("linked log target changed: %q, %v", got, err)
	}
}

func TestStartDoesNotSpawnWithCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	cfg := LaunchConfig{Version: "test", StateRoot: t.TempDir(), DefaultStateRoot: t.TempDir()}
	if spawned, err := cfg.Start(ctx); spawned || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled start: spawned=%t err=%v", spawned, err)
	}
}

func TestPruneLeavesPreservesRegisteredAndNonLeaves(t *testing.T) {
	s := reviewServer(t)
	rendezvous, err := openRendezvous(filepath.Join(t.TempDir(), "rendezvous"))
	if err != nil {
		t.Fatal(err)
	}
	defer rendezvous.Close()
	s.fixtureSQL(t, `INSERT INTO leaf(digest, vendor, key, generation, file_identity, state, created_at_ms)
		VALUES ('aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 'route', 'live', 'g', 'id', 'live', 1),
		       ('bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb', 'route', 'fenced', 'g', 'id', 'fenced', 1);`)
	const orphan = "m/cccccccccccccccc"
	keep := []string{"m/aaaaaaaaaaaaaaaa", "m/bbbbbbbbbbbbbbbb", "m/.register-stage", "m/other"}
	for _, path := range append([]string{orphan}, keep...) {
		if err := rendezvous.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	link := "m/dddddddddddddddd"
	if err := rendezvous.Symlink("other", link); err != nil {
		t.Fatal(err)
	}
	if err := pruneOrphanedLeaves(t.Context(), s.db, rendezvous); err != nil {
		t.Fatal(err)
	}
	for _, path := range append(keep, link) {
		if _, err := rendezvous.Lstat(path); err != nil {
			t.Fatalf("pruned %s: %v", path, err)
		}
	}
	if _, err := rendezvous.Lstat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan survived: %v", err)
	}
}

// An unconfirmed outcome without an operation key cannot be retried by key,
// so its next step is to check whether the message was stored.
func TestUnknownOutcomeWithoutKeyNamesANextStep(t *testing.T) {
	var refusal domain.Refusal
	if err := json.Unmarshal(unknownOutcome("unconfirmed", "").Data, &refusal); err != nil || refusal.Hint == "" {
		t.Fatalf("hint %q (%v)", refusal.Hint, err)
	}
}

// A member a hook joined owes its rules until the CLI recorded printing
// them. The roster cursor alone does not settle them: the version before the
// start line wrote one after a reply that carried no rules, and that file
// outlives the upgrade. A member that joined by hand got its rules from the
// join, and its cursor settles them as before.
func TestOnlyARulesReceiptSettlesAHookJoinedMembersRules(t *testing.T) {
	s := reviewServer(t)
	s.fixtureSQL(t, `INSERT INTO channel (name, created_at_ms) VALUES ('checkout', 1);
		INSERT INTO principal (kind, vendor, created_at_ms) VALUES ('session', 'route', 1), ('session', 'route', 1);
		INSERT INTO binding (principal_id, channel_id, alias, joined_at_ms, roster_baseline_seq) VALUES (1, 1, 'hooked', 1, 1);
		INSERT INTO binding (principal_id, channel_id, alias, joined_at_ms) VALUES (2, 1, 'byhand', 1);
		INSERT INTO roster_change (channel_id, seq, principal_id, field) VALUES (1, 1, 1, 'joined'), (1, 2, 2, 'joined');`)
	apply := func(binding int64, cursor, rules uint64) (acked int64, given bool) {
		t.Helper()
		if err := s.db.Update(t.Context(), func(tx store.Tx) error {
			if _, _, err := acknowledgeRoster(t.Context(), tx, &binding, cursor, rules); err != nil {
				return err
			}
			return tx.QueryRowContext(t.Context(), "SELECT roster_ack_seq, hint_acked FROM binding WHERE id = ?", binding).Scan(&acked, &given)
		}); err != nil {
			t.Fatal(err)
		}
		return acked, given
	}
	if acked, given := apply(1, 2, 0); acked != 2 || given {
		t.Fatalf("an older cursor for a hook-joined member: cursor %d, rules given %t", acked, given)
	}
	if acked, given := apply(1, 0, 2); acked != 2 || !given {
		t.Fatalf("a rules receipt alone, as a hook request brings it: cursor %d, rules given %t", acked, given)
	}
	if acked, given := apply(2, 2, 0); acked != 2 || !given {
		t.Fatalf("a cursor for a member that joined by hand: cursor %d, rules given %t", acked, given)
	}
}
