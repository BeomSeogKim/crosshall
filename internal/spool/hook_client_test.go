package spool_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

func TestHookLookupPreservesRefusalAndReturnsNewGeneration(t *testing.T) {
	f := setup(t)
	f.member = ident.Identity{Vendor: "claude", Key: "sample-a"}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	type result struct {
		generation string
		response   wire.Response
		err        error
	}
	first := filepath.Join(f.rv, "first")
	for _, intent := range []string{"lookup", "join", wire.HelloStart} {
		f.member.Key = "sample-" + intent // a leaf of its own
		done := make(chan result, 1)
		go func() {
			gen, resp, err := f.client.HookLookup(ctx, f.member, intent)
			done <- result{gen, resp, err}
		}()
		var hello wire.Hello
		attempt := claim(t, first, &hello)
		if hello.Intent != intent || hello.Vendor != "claude" || hello.Key != f.member.Key || attempt != hello.Attempt {
			t.Fatalf("wrong first contact: %+v", hello)
		}
		if intent == "lookup" {
			answer(t, first, attempt, attempt, core.ExitNotEnrolled, core.CodeNotEnrolled, "{}")
			r := <-done
			if r.err != nil || r.generation != "" || r.response.Code != core.CodeNotEnrolled || r.response.Status != core.ExitNotEnrolled {
				t.Fatalf("lost lookup refusal: %+v", r)
			}
			if entries, err := os.ReadDir(filepath.Join(f.rv, "m")); err != nil || len(entries) != 0 {
				t.Fatalf("unmatched lookup created a leaf: %v, %v", entries, err)
			}
		} else {
			f.leaf(t)
			answer(t, first, attempt, attempt, core.ExitOK, "", "{}")
			r := <-done
			if r.err != nil || r.generation != "generation-a" || r.response.Status != core.ExitOK {
				t.Fatalf("join hello: %+v", r)
			}
		}
	}
}

func TestHookCallUsesOnlyPinnedGenerationAndNoRosterCursor(t *testing.T) {
	f := setup(t)
	f.member = ident.Identity{Vendor: "codex", Key: "sample-b"}
	leaf := f.leaf(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := os.WriteFile(filepath.Join(leaf, "ack"), []byte("32\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ gen, verb string }{
		{"generation-before-restore", "hook.event"},
		{"", "hook.event"},
		{"generation-a", "owner grant"},
	} {
		_, err := f.client.HookCall(ctx, f.member, tc.gen, wire.Request{Verb: tc.verb, OpKey: "stable"}, true)
		if err == nil {
			t.Fatalf("accepted call %v", tc)
		}
	}
	done := make(chan callResult, 1)
	go func() {
		wantsReply := false
		r, err := f.client.HookCall(ctx, f.member, "generation-a", wire.Request{Verb: "hook.enroll", OpKey: "stable", Ack: 999, Reply: &wantsReply}, false)
		done <- callResult{r, err}
	}()
	var req wire.Request
	attempt := claim(t, leaf, &req)
	if req.Verb != "hook.enroll" || req.OpKey != "stable" || req.Ack != 0 || req.Generation != "generation-a" || req.KeyDigest != f.member.Digest() {
		t.Fatalf("incorrect hook request: %+v", req)
	}
	if req.Reply != nil && !*req.Reply {
		t.Fatal("synchronous hook suppressed its reply")
	}
	answer(t, leaf, attempt, attempt, core.ExitOK, "", `{"roster_seq":45,"epoch":12}`)
	r := <-done
	if r.err != nil || r.reply.Status != core.ExitOK || r.reply.OpKey != "stable" {
		t.Fatalf("unexpected RPC result: %+v", r)
	}
	if err := r.reply.Acknowledge(); err != nil {
		t.Fatal(err)
	}
	if ack, err := os.ReadFile(filepath.Join(leaf, "ack")); err != nil || string(ack) != "32\n" {
		t.Fatalf("hook consumed ordinary roster cursor: %q, %v", ack, err)
	}
}

func TestHookOneWayPublishesWithoutWaiting(t *testing.T) {
	f := setup(t)
	f.member = ident.Identity{Vendor: "codex", Key: "sample-c"}
	leaf := f.leaf(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	input := wire.Request{Verb: "hook.event", OpKey: "stop-7", Args: json.RawMessage(`{"fact":"turn_finishing","want_output":false}`)}
	r, err := f.client.HookCall(ctx, f.member, "generation-a", input, true)
	if err != nil || r.Status != core.ExitOK || len(r.Data) != 0 {
		t.Fatalf("one-way publish: %+v, %v", r, err)
	}
	var packet wire.Request
	attempt := claim(t, leaf, &packet)
	if packet.Reply == nil || *packet.Reply || packet.OpKey != input.OpKey || packet.Verb != input.Verb || packet.Attempt != attempt || packet.KeyDigest != f.member.Digest() {
		t.Fatalf("incorrect one-way envelope: %+v", packet)
	}
	cancel()
	if _, err := f.client.HookCall(ctx, f.member, "generation-a", input, true); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled call error: %v", err)
	}
}

func TestHookCallMissingLeafNeverInitiatesContact(t *testing.T) {
	f := setup(t)
	f.member = ident.Identity{Vendor: "codex", Key: "sample-d"}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, err := f.client.HookCall(ctx, f.member, "old-generation", wire.Request{Verb: "hook.event", OpKey: "stable"}, false)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing leaf: %v", err)
	}
	for _, in := range []string{filepath.Join(f.rv, "first", "in"), filepath.Join(f.state, "owner", "in")} {
		if entries, err := os.ReadDir(in); err != nil || len(entries) != 0 {
			t.Fatalf("unexpected first or owner contact: %v %v", entries, err)
		}
	}
}

// A hook whose dispatcher is a linked device's takes the causal token
// under its leaf's publish lock: the boot, its clocks and the leaf's stamp
// at that sequence (#400, design 6a). A native hook takes none.
func TestALinkedHookTakesItsTokenUnderThePublishLock(t *testing.T) {
	f := setup(t)
	f.member = ident.Identity{Vendor: "codex", Key: "sample-c"}
	input := wire.Request{Verb: "hook.event", OpKey: "stop-7", Args: json.RawMessage(`{"fact":"turn_finishing","want_output":false}`)}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	native := f.leaf(t)
	if _, err := f.client.HookCall(ctx, f.member, "generation-a", input, true); err != nil {
		t.Fatal(err)
	}
	var packet wire.Request
	claim(t, native, &packet)
	if packet.Capture != nil {
		t.Fatalf("a native hook took a token: %+v", packet.Capture)
	}

	// The dispatcher names a linked member rendezvous.
	linked := f
	linked.rv = filepath.Join(filepath.Dir(f.rv), "linked")
	if err := os.Mkdir(linked.rv, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(linked.rv, "m"), 0700); err != nil {
		t.Fatal(err)
	}
	leaf := linked.leaf(t)
	if err := os.WriteFile(filepath.Join(f.rv, "ready"), []byte(`{"members":"`+linked.rv+`"}`), 0600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(leaf)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	stamp := core.Membership{BindingID: 4, Generation: 2}
	if err := ident.RepublishMembership(root, stamp); err != nil {
		t.Fatal(err)
	}
	for seq := uint64(1); seq <= 2; seq++ {
		if _, err := f.client.HookCall(ctx, f.member, "generation-a", input, true); err != nil {
			t.Fatal(err)
		}
		packet = wire.Request{}
		claim(t, leaf, &packet)
		c := packet.Capture
		if c == nil || c.Boot == "" || c.MonoNS <= 0 || c.WallMS <= 0 || c.Membership == nil || *c.Membership != stamp ||
			packet.PublishSeq != seq {
			t.Fatalf("token %d: %+v (seq %d)", seq, c, packet.PublishSeq)
		}
	}
}
