package spool_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

// A Stop, a session end and an idle report each wait for an earlier hook of
// the launch that arrives while the spool drains.
func TestMemberSpoolRescansBeforeDependentStop(t *testing.T) {
	for _, fact := range []string{wire.HookTurnFinishing, wire.HookSessionEnded, wire.HookIdlePrompt} {
		t.Run(fact, func(t *testing.T) {
			f := setup(t)
			leaf := f.leaf(t)
			root, err := os.OpenRoot(leaf)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()

			ordinary := strings.Repeat("1", 32)
			writeMemberPacket(t, leaf, "in", wire.Request{Attempt: ordinary, Verb: "who"})
			stop := writeOrderHook(t, leaf, "in", "2", fact, 2)
			var prompt string
			var order []string
			err = spool.DrainMemberSpool(root, func(id string, _ []byte, _ bool) (core.Result, error) {
				order = append(order, id)
				if id == ordinary {
					prompt = writeOrderHook(t, leaf, "in", "3", wire.HookPromptSubmitted, 1)
				}
				return core.Result{}, nil
			})
			if err != nil || !slices.Equal(order, []string{ordinary, prompt, stop}) {
				t.Fatalf("drain order %q: %v", order, err)
			}
		})
	}
}

func TestMemberSpoolRechecksNewlyClaimedPredecessors(t *testing.T) {
	f := setup(t)
	leaf := f.leaf(t)
	root, err := os.OpenRoot(leaf)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	ordinary := strings.Repeat("1", 32)
	writeMemberPacket(t, leaf, "in", wire.Request{Attempt: ordinary, Verb: "who"})
	stop := writeOrderHook(t, leaf, "in", "2", wire.HookTurnFinishing, 4)
	var start, prompt, pickup string
	var order []string
	err = spool.DrainMemberSpool(root, func(id string, _ []byte, _ bool) (core.Result, error) {
		order = append(order, id)
		switch id {
		case ordinary:
			start = writeOrderHook(t, leaf, "in", "3", "", 1)
			pickup = writeOrderHook(t, leaf, "in", "4", wire.HookPickupSeen, 2)
		case start:
			prompt = writeOrderHook(t, leaf, "in", "5", wire.HookPromptSubmitted, 3)
		}
		return core.Result{}, nil
	})
	if err != nil || !slices.Equal(order, []string{ordinary, start, prompt, pickup, stop}) {
		t.Fatalf("drain order %q: %v", order, err)
	}
}

// A session end, and an idle report, wait for the launch's earlier hooks.
func TestMemberSpoolOrdersEqualHookTimesByPublication(t *testing.T) {
	for _, fact := range []string{wire.HookSessionEnded, wire.HookIdlePrompt} {
		t.Run(fact, func(t *testing.T) {
			f := setup(t)
			leaf := f.leaf(t)
			root, err := os.OpenRoot(leaf)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()

			end := writeOrderHook(t, leaf, "work", "1", fact, 2)
			older := time.Now().Add(-time.Second)
			if err := os.Chtimes(filepath.Join(leaf, "work", end+".json"), older, older); err != nil {
				t.Fatal(err)
			}
			start := writeOrderHook(t, leaf, "in", "2", "", 1)
			var order []string
			err = spool.DrainMemberSpool(root, func(id string, _ []byte, _ bool) (core.Result, error) {
				order = append(order, id)
				return core.Result{}, nil
			})
			if err != nil || !slices.Equal(order, []string{start, end}) {
				t.Fatalf("drain order %q: %v", order, err)
			}
		})
	}
}

func writeOrderHook(t *testing.T, leaf, section, digit, fact string, seq uint64) string {
	t.Helper()
	profile := wire.HookProfile{Adapter: "codex"}
	req := wire.Request{
		Attempt: strings.Repeat(digit, 32), Generation: "generation-a", KeyDigest: "member-a",
		PublishSeq: seq, ExpiresAtMS: time.Now().Add(time.Minute).UnixMilli(),
	}
	var err error
	if fact == "" {
		req.Verb = wire.VerbHookEnroll
		req.Args, err = json.Marshal(wire.HookEnrollArgs{Profile: profile, LaunchRef: "launch-a", HookAtMS: 5})
	} else {
		req.Verb = wire.VerbHookEvent
		req.Args, err = json.Marshal(wire.HookEventArgs{
			Profile: profile, LaunchRef: "launch-a", TurnRef: "turn-1", HookAtMS: 5,
			Fact: fact, WantOutput: fact == wire.HookTurnFinishing,
		})
		if fact != wire.HookTurnFinishing {
			noReply := false
			req.Reply = &noReply
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	writeMemberPacket(t, leaf, section, req)
	return req.Attempt
}

func writeMemberPacket(t *testing.T, leaf, section string, req wire.Request) {
	t.Helper()
	req.Version = wire.Version
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(leaf, section, req.Attempt+".json")
	if err := os.WriteFile(path+".part", data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".part", path); err != nil {
		t.Fatal(err)
	}
}

func TestMemberSpoolDrainsPublishedPredecessorsBeforeRecoveredStop(t *testing.T) {
	f := setup(t)
	leaf := f.leaf(t)
	dir, err := os.OpenRoot(leaf)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	falseValue := false
	// The Stop was already claimed. Native event times are tied or earlier
	// than the prompt after a wall clock step.
	writeHook := func(section, name, fact, launch string, at int64, reply *bool) string {
		t.Helper()
		id := strings.Repeat(name, 32)
		args, err := json.Marshal(wire.HookEventArgs{
			Profile: wire.HookProfile{Adapter: "codex"}, LaunchRef: launch, Fact: fact, TurnRef: "turn-1",
			HookAtMS: at, WantOutput: fact == wire.HookTurnFinishing,
		})
		if err != nil {
			t.Fatal(err)
		}
		req := wire.Request{Version: wire.Version, Attempt: id, Verb: wire.VerbHookEvent,
			Generation: "generation-a", KeyDigest: f.member.Digest(), Reply: reply, Args: args,
			ExpiresAtMS: time.Now().Add(time.Minute).UnixMilli()}
		path := filepath.Join(leaf, section, id+".json")
		data, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		return id
	}
	stop := writeHook("work", "1", wire.HookTurnFinishing, "launch-a", 4, nil)
	other := writeHook("in", "2", wire.HookTurnFinishing, "other-launch", 0, nil)
	pickup := writeHook("in", "3", wire.HookPickupSeen, "launch-a", 5, nil)
	prompt := writeHook("in", "f", wire.HookPromptSubmitted, "launch-a", 5, &falseValue)
	var order []string
	err = spool.DrainMemberSpool(dir, func(id string, _ []byte, _ bool) (core.Result, error) {
		order = append(order, id)
		return core.Result{Data: json.RawMessage(`{}`)}, nil
	})
	if err != nil || !slices.Equal(order, []string{prompt, other, pickup, stop}) {
		t.Fatalf("drain order %q: %v", order, err)
	}
	for _, section := range []string{"in", "work"} {
		entries, err := os.ReadDir(filepath.Join(leaf, section))
		if err != nil || len(entries) != 0 {
			t.Fatalf("%s after drain: %v, %v", section, entries, err)
		}
	}
	if _, err := os.Stat(filepath.Join(leaf, "out", prompt+".json")); !os.IsNotExist(err) {
		t.Fatalf("one-way prompt left a response: %v", err)
	}
	for _, id := range []string{other, pickup, stop} {
		if _, err := os.Stat(filepath.Join(leaf, "out", id+".json")); err != nil {
			t.Fatal(err)
		}
	}
}
