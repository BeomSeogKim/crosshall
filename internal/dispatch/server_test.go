package dispatch

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/testreap"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

func TestMain(m *testing.M) {
	if handled, code := RunTestDispatcherGuardian(os.Args[1:]); handled {
		os.Exit(code)
	}
	if len(os.Args) == 2 && os.Args[1] == "__dispatch" {
		for _, variable := range []string{"CLAUDE_CODE_SESSION_ID", "CODEX_THREAD_ID", "ANTIGRAVITY_CONVERSATION_ID", "CROSSHALL_ROUTE", "CROSSHALL_TEST_EXTRA"} {
			if _, present := os.LookupEnv(variable); present {
				os.Exit(70)
			}
		}
		state := os.Getenv("CROSSHALL_STATE_DIR")
		cfg := Config{LaunchConfig: LaunchConfig{
			Version: "test", StateRoot: state, Rendezvous: filepath.Join(filepath.Dir(state), "rendezvous"),
			DefaultStateRoot: filepath.Join(filepath.Dir(state), "default"),
		}, Services: testServices()}
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
		err := Serve(ctx, cfg)
		stop()
		if err != nil {
			os.Stderr.WriteString(err.Error() + "\n")
			os.Exit(1)
		}
		os.Exit(0)
	}
	// A diagnostic test may only inspect a vendor home it sets itself.
	os.Unsetenv("CLAUDE_CONFIG_DIR")
	os.Unsetenv("CODEX_HOME")
	os.Exit(testreap.Check(m.Run()))
}

func TestFirstContactChecksExpiryBeforeRegistration(t *testing.T) {
	db, err := store.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now()
	for _, tc := range []struct {
		name        string
		expiresAtMS int64
		wantCode    core.Code
	}{
		{"expired", now.Add(-time.Minute).UnixMilli(), core.CodeUnsent},
		{"current", now.Add(time.Minute).UnixMilli(), ""},
		{"legacy", 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var registered, published bool
			s := server{db: db}
			s.cfg.Hello = func(context.Context, store.Tx, []byte) (core.Result, func() error, error) {
				registered = true
				return core.Result{Status: core.ExitOK}, func() error { published = true; return nil }, nil
			}
			hello := wire.Hello{Version: wire.Version, Attempt: wire.NewID(), Vendor: "claude", Key: "fixture", Intent: wire.HelloStart, ExpiresAtMS: tc.expiresAtMS}
			raw, err := json.Marshal(hello)
			if err != nil {
				t.Fatal(err)
			}
			result, err := s.hello(t.Context(), hello.Attempt, raw)
			wantRegistration := tc.wantCode == ""
			if err != nil || result.Code != tc.wantCode || (result.Status == core.ExitOK) != wantRegistration ||
				registered != wantRegistration || published != wantRegistration {
				t.Fatalf("first contact result %+v, registered %t, published %t, error %v", result, registered, published, err)
			}
		})
	}
}

type testHandler struct{}

func (testHandler) Handle(ctx context.Context, tx store.Tx, caller core.Caller, req domain.Request) (core.Result, error) {
	if caller.Role == core.RoleUnenrolled {
		_, err := tx.ExecContext(ctx, "INSERT INTO principal(kind, vendor, created_at_ms) VALUES ('session', 'route', 1)")
		return commandError(core.ExitRefused, core.CodeNotFound, "channel was not found"), err
	}

	// A small durable handler with operation-key replay, for exercising the real file loop.
	var stored string
	err := tx.QueryRowContext(ctx, "SELECT result FROM op WHERE op_key = ?", req.OpKey).Scan(&stored)
	if err == nil {
		return core.Result{Status: core.ExitOK, Data: json.RawMessage(stored)}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return core.Result{}, err
	}
	created, err := tx.ExecContext(ctx, "INSERT INTO principal(kind, vendor, created_at_ms) VALUES ('owner', 'owner', 1)")
	if err != nil {
		return core.Result{}, err
	}
	principal, err := created.LastInsertId()
	if err != nil {
		return core.Result{}, err
	}
	name := req.Command.(domain.CreateChannel).Name
	if _, err := tx.ExecContext(ctx, "INSERT INTO channel(name, created_at_ms) VALUES (?, 1)", name); err != nil {
		return core.Result{}, err
	}
	const result = `{"created":true}`
	_, err = tx.ExecContext(ctx, `INSERT INTO op(principal_id, op_key, scope, verb, payload_hash, result, at_ms)
		VALUES (?, ?, 'owner', 'create', 'test', ?, 1)`, principal, req.OpKey, result)
	return core.Result{Status: core.ExitOK, Data: json.RawMessage(result)}, err
}

func testServices() Services {
	return Services{
		Resolve: func(_ context.Context, _ store.Tx, arrival Arrival, _ wire.Request) (Resolution, error) {
			if arrival.Owner {
				return Resolution{Caller: core.Caller{Role: core.RoleOwner}}, nil
			}
			return Resolution{Caller: core.Caller{Role: core.RoleUnenrolled}, Vendor: "route"}, nil
		},
		Decode: func(req wire.Request, _ Resolution) (domain.Request, error) {
			return domain.Request{OpKey: req.OpKey, Command: domain.CreateChannel{Name: "checkout"}}, nil
		},
		Handle: testHandler{},
		Hello: func(context.Context, store.Tx, []byte) (core.Result, func() error, error) {
			return core.Result{Status: core.ExitOK}, nil, nil
		},
	}
}

func TestDispatcherRecoversAnInterruptedPublish(t *testing.T) {
	f := newPublishTestFixture(t)
	f.ask(t, 11)
	id := f.reserve(t, true)
	root := filepath.Dir(f.root)
	cfg := LaunchConfig{
		Version: "test", StateRoot: f.root, DefaultStateRoot: filepath.Join(root, "default"),
		Rendezvous: filepath.Join(root, "rendezvous"), Owner: true,
	}
	if spawned, err := cfg.Start(t.Context()); err != nil || !spawned {
		log, _ := os.ReadFile(filepath.Join(f.root, "dispatcher.log"))
		t.Fatalf("start: spawned=%t err=%v\n%s", spawned, err, log)
	}
	// Only the dispatcher writes the store while it runs; its publisher
	// ticks now, so read the recovered attempt after it stops.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := cfg.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	f.requireAttempt(t, id, domain.PublishUncertain)
}

func TestDispatcherRestartRecoversClaim(t *testing.T) {
	const recoveryReadyTimeout = 10 * time.Second
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("TMPDIR", root)
	t.Setenv("XDG_STATE_HOME", root)
	t.Setenv("CODEX_THREAD_ID", "test-thread")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "test-session")
	t.Setenv("ANTIGRAVITY_CONVERSATION_ID", "test-conversation")
	t.Setenv("CROSSHALL_ROUTE", "test-route")
	t.Setenv("CROSSHALL_TEST_EXTRA", "unexpected")

	cfg := LaunchConfig{
		Version: "test", StateRoot: filepath.Join(root, "state"),
		DefaultStateRoot: filepath.Join(root, "default"), Rendezvous: filepath.Join(root, "rendezvous"), Owner: true,
	}
	ctx := t.Context()
	testreap.Arm(t, filepath.Join(cfg.Rendezvous, "ready"))
	t.Cleanup(func() {
		marker := readReady(cfg.Rendezvous)
		if marker.PID > 0 {
			syscall.Kill(marker.PID, syscall.SIGTERM)
			waitFor(t, func() bool { return readReady(cfg.Rendezvous).Instance == "" })
		}
	})
	if spawned, err := cfg.start(ctx, syscall.Kill, recoveryReadyTimeout); err != nil || !spawned {
		log, _ := os.ReadFile(filepath.Join(cfg.StateRoot, "dispatcher.log"))
		t.Fatalf("start: spawned=%t err=%v\n%s", spawned, err, log)
	}
	first := readReady(cfg.Rendezvous)
	if spawned, err := cfg.start(ctx, syscall.Kill, recoveryReadyTimeout); err != nil || spawned || !readReady(cfg.Rendezvous).same(first) {
		t.Fatalf("second start must retain the dispatcher: spawned=%t err=%v", spawned, err)
	}
	owner := filepath.Join(cfg.StateRoot, "owner")
	request := wire.Request{
		Version: wire.Version, Attempt: "1234567890abcdef1234567890abcdef",
		OpKey: "original-key", Verb: "owner channel create", Args: json.RawMessage(`{"name":"checkout"}`),
		IssuedAtMS: time.Now().UnixMilli(), ExpiresAtMS: time.Now().Add(30 * time.Second).UnixMilli(),
	}
	writePacket(t, filepath.Join(owner, "in"), request)
	result := awaitResponse(t, owner, request.Attempt)
	if result.Status != core.ExitOK || string(result.Data) != `{"created":true}` {
		t.Fatalf("first response: %+v", result)
	}
	if err := os.Remove(filepath.Join(owner, "out", request.Attempt+".json")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(first.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	state, err := spool.OpenServerRoot(cfg.StateRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	waitFor(t, func() bool {
		held, err := lockState(state)
		return err == nil && !held
	})
	// Model a crash after commit with the claimed input still present.
	writePacket(t, filepath.Join(owner, "work"), request)
	cfg.Owner = false
	if spawned, err := cfg.start(ctx, syscall.Kill, recoveryReadyTimeout); err != nil || !spawned {
		held, lockErr := lockState(state)
		log, _ := os.ReadFile(filepath.Join(cfg.StateRoot, "dispatcher.log"))
		t.Fatalf("restart: spawned=%t err=%v lock held=%t lock err=%v marker=%+v\n%s",
			spawned, err, held, lockErr, readReady(cfg.Rendezvous), log)
	}
	if ready := readReady(cfg.Rendezvous); ready.Instance == first.Instance {
		t.Fatal("did not restart with a fresh readiness instance")
	}
	replay := awaitResponse(t, owner, request.Attempt)
	if replay.Status != core.ExitOK || string(replay.Data) != string(result.Data) {
		t.Fatalf("claimed request did not replay after restart: %+v", replay)
	}
}

func TestUnenrolledRefusalRollsBack(t *testing.T) {
	ctx := t.Context()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := server{cfg: Config{Services: testServices()}, db: db}
	req := wire.Request{
		Version: wire.Version, Attempt: "attempt", OpKey: "join", Verb: "join", Args: json.RawMessage(`{}`),
		ExpiresAtMS: time.Now().Add(time.Second).UnixMilli(),
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.command(ctx, Arrival{}, req.Attempt, raw)
	if err != nil || r.Status != core.ExitRefused {
		t.Fatalf("wanted refusal, got %+v: %v", r, err)
	}
	if err := db.Update(ctx, func(tx store.Tx) error {
		var rows int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM principal").Scan(&rows); err != nil {
			return err
		}
		if rows != 0 {
			t.Fatalf("refused join left %d principals", rows)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func writePacket(t *testing.T, path string, request wire.Request) {
	t.Helper()
	out, err := spool.OpenServerRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if err := spool.PublishServerFile(out, request.Attempt+".json", request); err != nil {
		t.Fatal(err)
	}
}

func awaitResponse(t *testing.T, dir, attempt string) wire.Response {
	t.Helper()
	var response wire.Response
	waitFor(t, func() bool {
		raw, err := os.ReadFile(filepath.Join(dir, "out", attempt+".json"))
		return err == nil && json.Unmarshal(raw, &response) == nil
	})
	return response
}

func waitFor(t *testing.T, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for !ready() {
		if !time.Now().Before(deadline) {
			t.Fatal("timed out waiting for dispatcher")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
