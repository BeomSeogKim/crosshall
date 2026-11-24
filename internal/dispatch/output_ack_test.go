package dispatch

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

func TestDelayedOutputAckRequiresMemberIngress(t *testing.T) {
	s, root := adapterFilesFixture(t)
	leaf := adapterLeafFixture(t, s, root, true)
	principal, binding := int64(1), int64(2)
	caller := core.Caller{Role: core.RoleMember, PrincipalID: &principal, BindingID: &binding}
	s.cfg.Resolve = func(context.Context, store.Tx, Arrival, wire.Request) (Resolution, error) {
		return Resolution{Caller: caller, Vendor: "codex"}, nil
	}
	noReply := false
	req := wire.Request{Verb: wire.VerbHookJoinHintAck, OpKey: wire.VerbHookJoinHintAck, Reply: &noReply,
		Generation: "test-gen", ExpiresAtMS: 1}
	member := Arrival{Dir: leaf}
	// Older hooks send this verb without arguments to import join and reach lines.
	if got := s.fixtureRequest(t, member, req); got.Status != core.ExitOK {
		t.Fatalf("delayed legacy acknowledgement refused: %+v", got)
	}
	if got := s.fixtureRequest(t, Arrival{Dir: leaf, Owner: true}, req); got.Status == core.ExitOK {
		t.Fatal("owner ingress admitted a late member acknowledgement")
	}
	req.OpKey = "other"
	if got := s.fixtureRequest(t, member, req); got.Status == core.ExitOK {
		t.Fatal("wrong operation key admitted a late acknowledgement")
	}
	req.OpKey, req.Verb = wire.VerbHookEvent, wire.VerbHookEvent
	req.Args = json.RawMessage(`{"want_output":false}`)
	if got := s.fixtureRequest(t, member, req); got.Code != core.CodeOutcomeUnknown {
		t.Fatalf("expired unadmitted hook observation = %+v", got)
	}
}

func TestRestartImportsOnlyOriginalLeafOfferReceipts(t *testing.T) {
	state, rendezvous := t.TempDir(), filepath.Join(t.TempDir(), "rendezvous")
	openServer := func() *server {
		t.Helper()
		db, err := store.Open(t.Context(), state)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		s := &server{db: db, cfg: Config{LaunchConfig: LaunchConfig{StateRoot: state, Rendezvous: rendezvous}}}
		resolver := ident.SQLResolver{StateRoot: state, RendezvousRoot: rendezvous}
		s.cfg.Resolve = func(ctx context.Context, tx store.Tx, a Arrival, req wire.Request) (Resolution, error) {
			r, err := resolver.Resolve(ctx, tx, ident.Ingress{Path: a.Path, Dir: a.Dir}, ident.Proof{KeyDigest: req.KeyDigest, Generation: req.Generation})
			var denied *ident.EnvironmentError
			if errors.As(err, &denied) {
				return Resolution{Refusal: &core.Result{Status: core.ExitRefused, Code: denied.Code}}, nil
			}
			return Resolution{Caller: r.Caller}, err
		}
		return s
	}
	first := openServer()
	root, err := openRendezvous(rendezvous)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	leaf := adapterLeafFixture(t, first, root, true)
	if err := spool.PrepareServerSpool(leaf); err != nil {
		t.Fatal(err)
	}
	first.fixtureSQL(t, `INSERT INTO channel (id, name, created_at_ms) VALUES (1, 'dev', 1);
		INSERT INTO binding (id, principal_id, channel_id, alias, joined_at_ms) VALUES (2, 1, 1, 'alice', 1);
		INSERT INTO offer (id, principal_id, binding_id, generation, reason, evidence, epoch, at_ms, receipt_mode, route_epoch, nonce)
		VALUES (42, 1, 2, 'test-gen', 'prompt', 'test', 1, 1, 'provisional', 1, 'original');`)
	leaves, err := first.leaves(t.Context())
	if err != nil || len(leaves) != 1 {
		t.Fatalf("member registrations: %v, %v", leaves, err)
	}
	registered := leaves[0]
	old := time.Now().Add(-48 * time.Hour)
	sent := old.UnixMilli()
	queue := func(nonce string, at int64) {
		t.Helper()
		noReply := false
		args, err := json.Marshal(wire.HookOutputAckArgs{Offer: &wire.HookOfferReceipt{OfferID: 42, Nonce: nonce}, WrittenAtMS: at})
		if err != nil {
			t.Fatal(err)
		}
		req := wire.Request{Version: wire.Version, Verb: wire.VerbHookJoinHintAck, OpKey: wire.VerbHookJoinHintAck, Attempt: wire.NewID(),
			Args: args, Reply: &noReply, KeyDigest: registered.digest, Generation: "test-gen", ExpiresAtMS: old.Add(time.Second).UnixMilli()}
		in := filepath.Join(leaf.Name(), "in")
		writePacket(t, in, req)
		if err := os.Chtimes(filepath.Join(in, req.Attempt+".json"), old, old); err != nil {
			t.Fatal(err)
		}
	}
	queue("original", sent)
	if err := first.db.Close(); err != nil {
		t.Fatal(err)
	}
	second := openServer()
	members, err := root.OpenRoot("m")
	if err != nil {
		t.Fatal(err)
	}
	defer members.Close()
	drain := func(want sql.NullInt64) {
		t.Helper()
		second.runMember(t.Context(), members, second.beginWatches(), registered, true, time.Now())
		for _, section := range []string{"in", "work"} {
			if packets, err := os.ReadDir(filepath.Join(leaf.Name(), section)); err != nil || len(packets) != 0 {
				t.Fatalf("%s after drain: %v, %v", section, packets, err)
			}
		}
		if err := second.db.Update(t.Context(), func(tx store.Tx) error {
			var at sql.NullInt64
			err := tx.QueryRowContext(t.Context(), "SELECT written_at_ms FROM offer WHERE id = 42").Scan(&at)
			if at != want {
				t.Errorf("imported written_at_ms = %v, want %v", at, want)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	second.fixtureSQL(t, `CREATE TRIGGER blocked_receipt BEFORE UPDATE OF written_at_ms ON offer
		BEGIN SELECT RAISE(ABORT, 'injected write failure'); END`)
	second.runMember(t.Context(), members, second.beginWatches(), registered, true, time.Now())
	if packets, err := os.ReadDir(filepath.Join(leaf.Name(), "work")); err != nil || len(packets) != 1 {
		t.Fatalf("recoverable claims after database error: %v, %v", packets, err)
	}
	second.fixtureSQL(t, "DROP TRIGGER blocked_receipt")
	written := sql.NullInt64{Int64: sent, Valid: true}
	drain(written)
	queue("original", sent+1)
	drain(written)
	second.fixtureSQL(t, `UPDATE offer SET written_at_ms = NULL, nonce = 'another'`)
	queue("original", sent)
	drain(sql.NullInt64{})
	queue("another", sent)
	second.fixtureSQL(t, `UPDATE leaf SET generation = 'replacement'`)
	registered.generation = "replacement"
	drain(sql.NullInt64{})
}
