package hook

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

type offerWriter func([]byte) (int, error)

func (f offerWriter) Write(b []byte) (int, error) { return f(b) }

// Freeze the available work budget at one millisecond after stdout returns.
type offerBudget struct {
	context.Context
	low bool
}

func (c *offerBudget) Deadline() (time.Time, bool) {
	if c.low {
		return time.Now().Add(time.Millisecond), true
	}
	return c.Context.Deadline()
}

func TestOfferReceiptAfterFinalOutput(t *testing.T) {
	const line = "[crosshall] pending: 3"
	descriptor := wire.HookOfferReceipt{OfferID: 23, Nonce: "fixture-nonce"}
	for _, tc := range []struct {
		name  string
		acks  int
		clean bool
	}{
		{"offer alone", 1, true},
		{"join reach and offer", 1, true},
		{"short stdout", 0, false},
		{"stdout accepted bytes and returned error", 0, false},
		{"failed enqueue after stdout", 1, false},
		{"little work budget", 0, false},
		{"callback canceled at stdout", 0, false},
		{"stale epoch", 0, false},
		{"filtered start with unrelated output", 0, false},
		{"revoked join with pending output", 1, false},
		{"empty pending frame", 0, false},
		{"old dispatcher", 0, true},
		{"sequence advanced after output", 1, false},
		{"earlier batch", 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rv, _, path := runFixture(t)
			leaf, err := os.OpenRoot(path)
			if err != nil {
				t.Fatal(err)
			}
			defer leaf.Close()
			hookFile(t, leaf, "stamp", "fixture-host fixture-gen 2 9\n")
			var membership *core.Membership
			if tc.name == "offer alone" {
				membership = &core.Membership{BindingID: 5, Generation: 2}
				if err := ident.RepublishMembership(leaf, *membership); err != nil {
					t.Fatal(err)
				}
				hookFile(t, leaf, "seen-notice", "fixture-host fixture-gen 2\n")
				if hasSeenNotice(leaf, "fixture-gen") {
					t.Fatal("a legacy stamp supplied prior membership history")
				}
			}
			result := wire.HookResult{
				Epoch: 7, PendingLine: line, OfferID: descriptor.OfferID, OfferReceipt: &descriptor,
				Ack: &wire.HookAck{Kind: "notice", Instance: "fixture-host", Generation: "fixture-gen", Seq: 2},
			}
			if membership != nil {
				result.Ack.Membership = *membership
			}
			event := "UserPromptSubmit"
			switch tc.name {
			case "join reach and offer":
				join := wire.HookJoinHintAck{Generation: "fixture-gen", BindingID: 5, RouteEpoch: 1, RosterSeq: 2, Offer: "guide"}
				reach := wire.HookReachAck{Generation: "fixture-gen", BindingID: 5, RouteEpoch: 1, Told: 1, Offer: "reach"}
				result.Hint, result.ReachLine, result.JoinHintAck, result.ReachAck = "guide line", "reach line", &join, &reach
				for file, receipt := range map[string]any{JoinHintOfferFile: join, ReachOfferFile: reach} {
					raw, _ := json.Marshal(receipt)
					hookFile(t, leaf, file, string(raw))
				}
			case "revoked join with pending output":
				result.Hint = "revoked guide"
				result.JoinHintAck = &wire.HookJoinHintAck{Generation: "fixture-gen", BindingID: 5, RouteEpoch: 1, RosterSeq: 2, Offer: "revoked"}
			case "empty pending frame":
				result.PendingLine = ""
			case "filtered start with unrelated output":
				event = "SessionStart"
				result.Enrollment, result.Hint = "none", "unrelated line"
			case "old dispatcher":
				result.OfferReceipt = nil
			case "earlier batch":
				result.Ack = nil
			case "stale epoch":
				result.Epoch = 8
			}
			base, cancel := context.WithCancel(t.Context())
			defer cancel()
			ctx := &offerBudget{Context: base}
			var stdout strings.Builder
			var wroteAt int64
			w := offerWriter(func(p []byte) (int, error) {
				switch tc.name {
				case "short stdout":
					return len(p) - 1, nil
				case "stdout accepted bytes and returned error":
					return len(p), io.ErrClosedPipe
				case "little work budget":
					ctx.low = true
				case "callback canceled at stdout":
					cancel()
				}
				wroteAt = time.Now().UnixMilli()
				return stdout.Write(p)
			})
			acks := 0
			transport := recordedTransport{call: func(_ ident.Identity, _ string, req wire.Request, oneWay bool) (spool.Reply, error) {
				if req.Verb != wire.VerbHookJoinHintAck {
					if oneWay || !req.OfferReceipts || strings.Contains(string(req.Args), "offer_receipts") {
						t.Fatalf("callback capability must be in the request envelope: %+v", req)
					}
					raw, _ := json.Marshal(result)
					return spool.Reply{Response: wire.Response{Result: core.Result{Data: raw, Membership: membership}}}, nil
				}
				acks++
				if membership != nil && (req.Membership == nil || *req.Membership != *membership) {
					t.Fatalf("acknowledgement membership = %+v, want %+v", req.Membership, membership)
				}
				var ack wire.HookOutputAckArgs
				if err := json.Unmarshal(req.Args, &ack); err != nil || ack.Offer == nil || *ack.Offer != descriptor || !oneWay ||
					!strings.Contains(stdout.String(), line) || ack.WrittenAtMS < wroteAt || ack.WrittenAtMS > time.Now().UnixMilli() {
					t.Fatalf("bad acknowledgement %s for %q (%v)", req.Args, stdout.String(), err)
				}
				if tc.name == "failed enqueue after stdout" {
					return spool.Reply{}, errors.New("enqueue failed")
				}
				if tc.name == "sequence advanced after output" {
					hookFile(t, leaf, "stamp", "fixture-host fixture-gen 3 9\n")
				}
				return spool.Reply{}, nil
			}}
			body := `{"session_id":"fixture-thread","turn_id":"turn","source":"startup","hook_event_name":"` + event + `"}`
			outcome := dispatchEvent(ctx, time.Now(), rv, transport, []string{"codex", event}, strings.NewReader(body), w, func(string) (string, bool) { return "", false })
			if tc.name == "join reach and offer" {
				_, joinSeen := ReadJoinHintAck(leaf, "fixture-gen")
				_, reachSeen := ReadReachAck(leaf, "fixture-gen")
				if !joinSeen || !reachSeen {
					t.Fatalf("combined output lost receipts: join=%t reach=%t", joinSeen, reachSeen)
				}
			}
			if acks != tc.acks || stampClean(rv, leaf, "fixture-gen", "notice", "") != tc.clean {
				t.Fatalf("acks=%d clean=%t, want %d/%t; outcome=%s stdout=%q", acks, stampClean(rv, leaf, "fixture-gen", "notice", ""), tc.acks, tc.clean, outcome, stdout.String())
			}
			if membership != nil && !hasSeenNotice(leaf, "fixture-gen") {
				t.Fatal("receipt stamp lost the current membership history")
			}
		})
	}
}

func TestLastOfferReceiptMakesNextPromptOneWay(t *testing.T) {
	rv, _, path := runFixture(t)
	leaf, err := os.OpenRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	defer leaf.Close()
	hookFile(t, leaf, "stamp", "fixture-host fixture-gen 2 9\n")
	// An older hook acknowledged this sequence without checking historical offers.
	hookFile(t, leaf, "seen-notice", "fixture-host fixture-gen 2\n")

	if !hasSeenNotice(leaf, "fixture-gen") {
		t.Fatal("a legacy stamp lost the enrollment history for this generation")
	}
	lastAck := &wire.HookAck{Kind: "notice", Instance: "fixture-host", Generation: "fixture-gen", Seq: 2}
	first := &wire.HookOfferReceipt{OfferID: 1, Nonce: "first"}
	last := &wire.HookOfferReceipt{OfferID: 2, Nonce: "last"}
	const ten = "[crosshall] pending: 1 2 3 4 5 6 7 8 9 10"
	const one = "[crosshall] pending: 11"
	for _, step := range []struct {
		turn  string
		line  string
		offer *wire.HookOfferReceipt
		ack   *wire.HookAck
		skip  bool
	}{
		{"first", ten, first, nil, false},
		{"last-printed", one, last, lastAck, true},
		{"last-retried", one, last, lastAck, false},
		{"clean", "", nil, nil, false},
	} {
		calls := 0
		transport := recordedTransport{call: func(_ ident.Identity, _ string, req wire.Request, oneWay bool) (spool.Reply, error) {
			calls++
			if req.Verb == wire.VerbHookJoinHintAck {
				var receipt wire.HookOutputAckArgs
				if err := json.Unmarshal(req.Args, &receipt); err != nil || receipt.Offer == nil || step.offer == nil || *receipt.Offer != *step.offer {
					t.Fatalf("turn %s: wrong receipt %s", step.turn, req.Args)
				}
				if step.skip {
					return spool.Reply{}, errors.New("busy")
				}
				return spool.Reply{}, nil
			}
			if req.Verb != wire.VerbHookEvent || oneWay != (step.offer == nil) || req.OfferReceipts == oneWay {
				t.Fatalf("turn %s: unexpected request %+v, one-way=%v", step.turn, req, oneWay)
			}
			out := wire.HookResult{Epoch: 7, PendingLine: step.line, OfferReceipt: step.offer, Ack: step.ack}
			raw, _ := json.Marshal(out)
			return spool.Reply{Response: wire.Response{Result: core.Result{Data: raw}}}, nil
		}}
		out, outcome := simulate(t, rv, transport, "UserPromptSubmit", map[string]any{"turn_id": step.turn})
		if step.offer == nil {
			if calls != 1 || out != "" || outcome != "clean" {
				t.Fatalf("next prompt made %d calls, outcome %s, output %q", calls, outcome, out)
			}
		} else if calls != 2 || !strings.Contains(out, step.line) || outcome != "rpc" {
			t.Fatalf("turn %s made %d calls, outcome %s, output %q", step.turn, calls, outcome, out)
		}
	}
	if legacy, err := leaf.ReadFile("seen-notice"); err != nil || string(legacy) != "fixture-host fixture-gen 2\n" {
		t.Fatalf("capable hook changed the legacy stamp: %q (%v)", legacy, err)
	}
}

func TestOverlappingOfferCallbacksKeepNewerCleanStamp(t *testing.T) {
	rv, _, path := runFixture(t)
	leaf, err := os.OpenRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	defer leaf.Close()
	hookFile(t, leaf, "stamp", "fixture-host fixture-gen 2 9\n")
	var callback func(string, uint64)
	callback = func(turn string, seq uint64) {
		result := wire.HookResult{Epoch: 7, PendingLine: "[crosshall] pending: 1", OfferReceipt: &wire.HookOfferReceipt{OfferID: 1, Nonce: "first"},
			Ack: &wire.HookAck{Kind: "notice", Instance: "fixture-host", Generation: "fixture-gen", Seq: seq}}
		var acks int
		transport := recordedTransport{call: func(_ ident.Identity, _ string, req wire.Request, oneWay bool) (spool.Reply, error) {
			if req.Verb == wire.VerbHookJoinHintAck {
				acks++
				if turn == "first" {
					// A new dirty sequence reaches a second prompt while the first
					// prompt still has its earlier acknowledgement to save.
					hookFile(t, leaf, "stamp", "fixture-host fixture-gen 3 9\n")
					callback("second", 3)
				}
				return spool.Reply{}, nil
			}
			raw, _ := json.Marshal(result)
			return spool.Reply{Response: wire.Response{Result: core.Result{Data: raw}}}, nil
		}}
		out, outcome := simulate(t, rv, transport, "UserPromptSubmit", map[string]any{"turn_id": turn})
		if acks != 1 || outcome != "rpc" || !strings.Contains(out, result.PendingLine) {
			t.Fatalf("turn %s: acks=%d outcome=%s output=%q", turn, acks, outcome, out)
		}
	}
	callback("first", 2)
	if !stampClean(rv, leaf, "fixture-gen", "notice", "") {
		t.Fatal("older callback overwrote the newer clean stamp")
	}
}
