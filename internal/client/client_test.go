package client

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/cli"
	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/spool"
)

func TestTypedRequests(t *testing.T) {
	for _, tc := range []struct {
		command domain.Command
		verb    string
		fields  map[string]string
	}{
		{domain.Join{Channel: "a", Focus: "hello"}, "join", map[string]string{"channel": "a", "focus": "hello"}},
		{domain.CreateChannel{Name: "a", Topic: "hello"}, "owner channel create", map[string]string{"name": "a", "topic": "hello"}},
		{domain.Who{}, "who", map[string]string{}},
	} {
		req, err := request(tc.command)
		parsed := map[string]string{}
		if err != nil || req.Verb != tc.verb || json.Unmarshal(req.Args, &parsed) != nil || !reflect.DeepEqual(parsed, tc.fields) {
			t.Fatalf("%s request: %+v, %v", tc.verb, req, err)
		}
	}
}

func TestUnknownVersionWarnings(t *testing.T) {
	raw := json.RawMessage(`{"id":9007199254740993,"warnings":["existing warning"],"members":[{"vendor":"codex","vendor_version":"9"},{"vendor":"claude","vendor_version":"2.1.327"}],"vendors":[{"vendor":"agy","vendor_version":""}]}`)
	body, err := withWarnings(raw, ident.Identity{Vendor: "codex", Version: "9"})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		ID       int64
		Warnings []string
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	want := []string{"existing warning", "identity variable unverified on codex 9", "identity variable unverified on agy unknown"}
	if got.ID != 9007199254740993 || !reflect.DeepEqual(got.Warnings, want) {
		t.Fatalf("unexpected details: %+v", got)
	}
}

func TestTransportOutcomes(t *testing.T) {
	// The spool intentionally does not exist. A valid member gets the
	// underlying unsent result; callers without identity never reach the spool.
	transport := New(spool.Client{Rendezvous: filepath.Join(t.TempDir(), "absent")})
	var env = map[string]string{"CODEX_THREAD_ID": "test-thread"}
	transport.lookup = func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	call := cli.Call{Command: domain.Join{Channel: "app"}, OpKey: "key-1"}
	result, err := transport.Do(context.Background(), call)
	if err != nil || result.Code != core.CodeUnsent || !strings.Contains(string(result.Data), "key-1") {
		t.Fatalf("unsent: %+v, %v", result, err)
	}
	owner := cli.Call{Owner: true, Command: domain.CreateChannel{Name: "x"}}
	if result, err = transport.Do(context.Background(), owner); err != nil || result.Code != core.CodeOwnerOnly {
		t.Fatalf("owner gate: %+v, %v", result, err)
	}
	delete(env, "CODEX_THREAD_ID")
	if result, err = transport.Do(context.Background(), call); err != nil || result.Status != core.ExitNotEnrolled || result.Code != core.CodeNotEnrolled {
		t.Fatalf("missing identity: %+v, %v", result, err)
	}
	var out bytes.Buffer
	if status := transport.Whoami(&out); status != core.ExitNotEnrolled || !strings.Contains(out.String(), "not_enrolled") {
		t.Fatalf("unbound whoami: %d %s", status, out.String())
	}
	env["CODEX_THREAD_ID"] = "test-thread"
	out.Reset()
	if status := transport.Whoami(&out); status != core.ExitUnavailable || !strings.Contains(out.String(), "codex:test-thread unknown") {
		t.Fatalf("unavailable whoami: %d %s", status, out.String())
	}
	out.Reset()
	var receipt struct {
		Code                    core.Code
		Vendor, Key, Enrollment string
	}
	if status := transport.WhoamiJSON(&out); status != core.ExitUnavailable || json.Unmarshal(out.Bytes(), &receipt) != nil ||
		receipt.Code != core.CodeUnsent || receipt.Vendor != "codex" || receipt.Key != "test-thread" || receipt.Enrollment != "unknown" {
		t.Fatalf("JSON whoami: %d %s", status, out.String())
	}
}
