package spool_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

type fixture struct {
	client spool.Client
	member ident.Identity
	state  string
	rv     string
}

func setup(t *testing.T) fixture {
	t.Helper()
	base := t.TempDir()
	state, rv := filepath.Join(base, "state"), filepath.Join(base, "rv")
	for _, p := range []string{state, rv, filepath.Join(rv, "m")} {
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	makeSpool(t, filepath.Join(rv, "first"))
	makeSpool(t, filepath.Join(state, "owner"))
	return fixture{spool.Client{StateRoot: state, Rendezvous: rv}, ident.Identity{Vendor: "route", Key: wire.NewID()}, state, rv}
}

func makeSpool(t *testing.T, path string) {
	t.Helper()
	for _, p := range []string{path, filepath.Join(path, "in"), filepath.Join(path, "out"), filepath.Join(path, "work")} {
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
}

func (f fixture) leaf(t *testing.T) string {
	t.Helper()
	path := filepath.Join(f.rv, "m", f.member.Digest()[:16])
	makeSpool(t, path)
	if err := os.WriteFile(filepath.Join(path, "gen"), []byte("generation-a\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Simulate a dispatcher claim. A staged file never matches the final suffix.
func claim(t *testing.T, dir string, dest any) string {
	t.Helper()
	end := time.Now().Add(3 * time.Second)
	for time.Now().Before(end) {
		matches, err := filepath.Glob(filepath.Join(dir, "in", "*.json"))
		if err != nil {
			t.Fatal(err)
		}
		if len(matches) == 1 {
			name := filepath.Base(matches[0])
			path := filepath.Join(dir, "work", name)
			if err := os.Rename(matches[0], path); err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(b, dest); err != nil {
				t.Fatal(err)
			}
			return strings.TrimSuffix(name, ".json")
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("no request was published")
	return ""
}

func answer(t *testing.T, spool, name, attempt string, status core.ExitCode, code core.Code, data string) {
	t.Helper()
	b, err := json.Marshal(wire.Response{Version: wire.Version, Attempt: attempt,
		Result: core.Result{Status: status, Code: code, Data: json.RawMessage(data)}})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(spool, "out")
	if err := os.WriteFile(filepath.Join(dir, name+".part"), b, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, name+".part"), filepath.Join(dir, name+".json")); err != nil {
		t.Fatal(err)
	}
}

type callResult struct {
	reply spool.Reply
	err   error
}

func callMember(ctx context.Context, f fixture, req wire.Request) <-chan callResult {
	done := make(chan callResult, 1)
	go func() {
		reply, err := f.client.Member(ctx, f.member, req)
		done <- callResult{reply, err}
	}()
	return done
}

func TestMemberFirstContactAndRenderedAck(t *testing.T) {
	f := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var hello wire.Hello
	done := callMember(ctx, f, wire.Request{Verb: "join", OpKey: "retry-join", Args: json.RawMessage(`{"channel":"one"}`)})
	first := filepath.Join(f.rv, "first")
	attempt := claim(t, first, &hello)
	if hello.Attempt != attempt || hello.Vendor != f.member.Vendor || hello.Key != f.member.Key || hello.Intent != "join" || hello.Version != wire.Version {
		t.Fatalf("incorrect hello: %+v", hello)
	}
	leaf := f.leaf(t)
	answer(t, first, attempt, attempt, core.ExitOK, "", `{}`)
	var req wire.Request
	domainAttempt := claim(t, leaf, &req)
	if req.KeyDigest != f.member.Digest() || req.Generation != "generation-a" || req.OpKey != "retry-join" ||
		req.Attempt != domainAttempt || req.Attempt == attempt || req.ExpiresAtMS <= req.IssuedAtMS || req.Ack != 0 {
		t.Fatalf("incorrect member request: %+v", req)
	}
	answer(t, leaf, domainAttempt, domainAttempt, core.ExitOK, "", `{"roster_seq":12}`)
	r := <-done
	if r.err != nil || r.reply.Status != core.ExitOK {
		t.Fatalf("unexpected reply %+v, %v", r.reply, r.err)
	}
	if _, err := os.Stat(filepath.Join(leaf, "ack")); !os.IsNotExist(err) {
		t.Fatalf("cursor advanced before output: %v", err)
	}
	if err := r.reply.Acknowledge(); err != nil {
		t.Fatal(err)
	}

	done = callMember(ctx, f, wire.Request{Verb: "who"})
	newAttempt := claim(t, leaf, &req)
	if req.Ack != 12 || req.OpKey == "" || req.Attempt == domainAttempt || req.Attempt == req.OpKey {
		t.Fatalf("incorrect second request: %+v", req)
	}
	answer(t, leaf, newAttempt, newAttempt, core.ExitOK, "", `{"roster_seq":8}`)
	if err := (<-done).reply.Acknowledge(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(leaf, "ack"))
	if err != nil || strings.TrimSpace(string(b)) != "12" {
		t.Fatalf("cursor went backwards: %q %v", b, err)
	}
}

func TestTimeoutBeforeAndAfterClaim(t *testing.T) {
	for _, phase := range []string{"missing-root", "hello-claimed", "domain-unclaimed", "domain-claimed"} {
		t.Run(phase, func(t *testing.T) {
			f := setup(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var done <-chan callResult
			var req wire.Request
			key := "original-key"
			cmd := wire.Request{Verb: "join", OpKey: key}
			switch phase {
			case "missing-root":
				f.client.Rendezvous = filepath.Join(f.rv, "absent")
				done = callMember(ctx, f, cmd)
			case "hello-claimed":
				done = callMember(ctx, f, cmd)
				claim(t, filepath.Join(f.rv, "first"), &wire.Hello{})
			case "domain-unclaimed":
				leaf := f.leaf(t)
				done = callMember(ctx, f, cmd)
				end := time.Now().Add(3 * time.Second)
				for {
					files, _ := filepath.Glob(filepath.Join(leaf, "in", "*.json"))
					if len(files) == 1 {
						break
					}
					if time.Now().After(end) {
						t.Fatal("no domain publication")
					}
					time.Sleep(time.Millisecond)
				}
			case "domain-claimed":
				leaf := f.leaf(t)
				done = callMember(ctx, f, cmd)
				claim(t, leaf, &req)
			}
			cancel()
			r := <-done
			want := core.CodeUnsent
			if phase == "domain-claimed" {
				want = core.CodeOutcomeUnknown
			}
			if r.err == nil || r.reply.Status != core.ExitUnavailable || r.reply.Code != want ||
				r.reply.OpKey != key || !strings.Contains(string(r.reply.Data), "retry with --key original-key") {
				t.Fatalf("unexpected classification: %+v %v", r.reply, r.err)
			}
		})
	}
}

func TestOwnerProofAndMalformedResponse(t *testing.T) {
	f := setup(t)
	owner := filepath.Join(f.state, "owner")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan callResult, 1)
	go func() {
		resp, err := f.client.Owner(ctx, wire.Request{Verb: "owner status", Generation: "ignored", KeyDigest: "ignored"})
		done <- callResult{resp, err}
	}()
	var fields map[string]json.RawMessage
	attempt := claim(t, owner, &fields)
	if _, ok := fields["gen"]; ok {
		t.Fatal("owner included generation")
	}
	if _, ok := fields["kd"]; ok {
		t.Fatal("owner included identity digest")
	}
	answer(t, owner, attempt, "different-attempt", core.ExitOK, "", `{}`)
	r := <-done
	if r.err == nil || r.reply.Code != core.CodeOutcomeUnknown || r.reply.OpKey == "" {
		t.Fatalf("accepted an uncorrelated reply: %+v %v", r.reply, r.err)
	}
}

func TestHelloRefusal(t *testing.T) {
	f := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := callMember(ctx, f, wire.Request{Verb: "who"})
	first := filepath.Join(f.rv, "first")
	var hello wire.Hello
	attempt := claim(t, first, &hello)
	if hello.Intent != "lookup" {
		t.Fatalf("expected lookup, got %s", hello.Intent)
	}
	answer(t, first, attempt, attempt, core.ExitNotEnrolled, core.CodeKeyRetired, `{}`)
	r := <-done
	if r.err != nil || r.reply.Code != core.CodeKeyRetired || r.reply.Status != core.ExitNotEnrolled {
		t.Fatalf("lost refusal: %+v %v", r.reply, r.err)
	}
}

func TestVendorUpgradeRefreshesHelloOnce(t *testing.T) {
	f := setup(t)
	f.member = ident.Identity{Vendor: "codex", Key: "member", Version: "0.172.0"}
	leaf := f.leaf(t)
	versionFile := filepath.Join(leaf, "version")
	if err := os.WriteFile(versionFile, []byte("0.170.0"), 0600); err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(f.rv, "first")
	for n := range 2 {
		done := callMember(t.Context(), f, wire.Request{Verb: "who"})
		if n == 0 {
			var hello wire.Hello
			claim(t, first, &hello)
			if hello.VendorVersion != f.member.Version || hello.Intent != "lookup" {
				t.Fatalf("wrong version observation: %+v", hello)
			}
			if err := os.WriteFile(versionFile, []byte(hello.VendorVersion), 0600); err != nil {
				t.Fatal(err)
			}
			answer(t, first, hello.Attempt, hello.Attempt, core.ExitOK, "", "{}")
		}
		var req wire.Request
		claim(t, leaf, &req)
		if req.VendorVersion != f.member.Version {
			t.Fatalf("wrong observed version: %q", req.VendorVersion)
		}
		answer(t, leaf, req.Attempt, req.Attempt, core.ExitOK, "", "{}")
		if got := <-done; got.err != nil || got.reply.Status != core.ExitOK {
			t.Fatalf("lookup: %+v", got)
		}
	}
}
