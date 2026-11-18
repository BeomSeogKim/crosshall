package ident

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/core"
)

func lookupMap(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

func TestResolveEnvironment(t *testing.T) {
	const route = "a23c5547b52a42eb83d2b16d57d02535"
	cases := []struct {
		name   string
		values map[string]string
		id     Identity
		code   core.Code
	}{
		{"shell", nil, Identity{}, core.CodeNotEnrolled},
		{"valid route", map[string]string{RouteVariable: route}, Identity{Vendor: "route", Key: route}, ""},
		{"empty route", map[string]string{RouteVariable: ""}, Identity{}, core.CodeNotEnrolled},
		{"bad route", map[string]string{RouteVariable: "not-a-route"}, Identity{}, core.CodeNotEnrolled},
		{"vendor wins", map[string]string{RouteVariable: "malformed", "CODEX_THREAD_ID": "thread", "CODEX_VERSION": "0.170.0"}, Identity{"codex", "thread", "0.170.0"}, ""},
		{"claude", map[string]string{"CLAUDE_CODE_SESSION_ID": "c", "AI_AGENT": "claude-code_2-1-327_agent"}, Identity{"claude", "c", "2.1.327"}, ""},
		{"agy", map[string]string{"ANTIGRAVITY_CONVERSATION_ID": "g", "ANTIGRAVITY_LS_VERSION": "cli-1.5.10"}, Identity{"agy", "g", "ls:cli-1.5.10"}, ""},
		{"ambiguous", map[string]string{"CODEX_THREAD_ID": "parent-value", "CLAUDE_CODE_SESSION_ID": "child-value"}, Identity{}, core.CodeBindingAmbiguous},
		{"malformed vendor", map[string]string{"CODEX_THREAD_ID": "", RouteVariable: route}, Identity{}, core.CodeNotEnrolled},
		{"invalid text", map[string]string{"CODEX_THREAD_ID": "two\nlines"}, Identity{}, core.CodeNotEnrolled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveEnvironment(lookupMap(tc.values))
			if got != tc.id {
				t.Fatalf("got %+v, want %+v", got, tc.id)
			}
			if tc.code == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var failure *EnvironmentError
			if !errors.As(err, &failure) || failure.Code != tc.code {
				t.Fatalf("got %v, want %s", err, tc.code)
			}
			if tc.code == core.CodeBindingAmbiguous {
				for _, marker := range []string{"CODEX_THREAD_ID", "CLAUDE_CODE_SESSION_ID"} {
					if !strings.Contains(failure.Error(), marker) || strings.Contains(failure.Error(), tc.values[marker]) {
						t.Fatal("ambiguity diagnostic must identify each variable by name only")
					}
				}
			}
		})
	}
}

func TestOwnerMarkers(t *testing.T) {
	names := []string{RouteVariable, "CLAUDE_CODE_SESSION_ID", "CODEX_THREAD_ID", "ANTIGRAVITY_CONVERSATION_ID", "CLAUDECODE", "AI_AGENT", "CODEX_SESSION_ID", "ANTIGRAVITY_AGENT"}
	for _, name := range names {
		if AllowOwner(lookupMap(map[string]string{name: ""}), true) {
			t.Errorf("allowed empty %s", name)
		}
	}
	if AllowOwner(lookupMap(nil), false) || !AllowOwner(lookupMap(nil), true) {
		t.Error("owner prefix must select a clean environment")
	}
}

func TestVersionEvidence(t *testing.T) {
	for vendor, version := range map[string]string{"claude": "2.1.327", "codex": "0.170.0", "agy": "1.5.11"} {
		id := Identity{Vendor: vendor, Version: version}
		if source, ok := id.Evidence(); source == "" || !ok || id.Warning() != "" {
			t.Fatalf("missing evidence for %s %s", vendor, version)
		}
		id.Version = version + ".1"
		if got, want := id.Warning(), "identity variable unverified on "+vendor+" "+id.Version; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

func TestChildEnvironment(t *testing.T) {
	original := []string{"CLAUDE_CODE_SESSION_ID=parent", "CODEX_THREAD_ID=parent", "ANTIGRAVITY_CONVERSATION_ID=parent", RouteVariable + "=old", "PATH=/bin"}
	child := childEnvironment(original)
	if len(child) != 2 || !slices.Contains(child, "PATH=/bin") {
		t.Fatalf("unexpected child environment %v", child)
	}
	for _, entry := range child {
		if value, ok := strings.CutPrefix(entry, RouteVariable+"="); ok && validRoute(value) {
			if slices.Contains(childEnvironment(original), entry) {
				t.Fatal("route reused")
			}
			return
		}
	}
	t.Fatal("missing new route")
}

// Run this opt-in check in two separate vendor tool calls. First use
// -args -check-vendor-identity=sample, then pass the printed sample value.
// The random salt prevents the sample from being a stable session identifier.
var identitySample = flag.String("check-vendor-identity", "", "sample or compare a vendor conversation variable across invocations")

func TestVendorIdentityProbe(t *testing.T) {
	if *identitySample == "" {
		t.Skip("opt-in vendor environment check")
	}
	id, err := ResolveEnvironment(os.LookupEnv)
	if err != nil || id.Vendor == "route" {
		t.Fatal("vendor identity absent or ambiguous")
	}
	var salt [16]byte
	var first string
	if *identitySample == "sample" {
		rand.Read(salt[:])
	} else {
		encoded, digest, ok := strings.Cut(*identitySample, ".")
		decoded, err := hex.DecodeString(encoded)
		if !ok || err != nil || len(decoded) != len(salt) || len(digest) != 64 {
			t.Fatal("invalid sample")
		}
		copy(salt[:], decoded)
		first = digest
	}
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(string(salt[:])+"\x00"+id.Vendor+"\x00"+id.Key)))
	if first == "" {
		t.Logf("%s %s identity present; sample=%x.%s", id.Vendor, id.Version, salt, digest)
		return
	}
	if first != digest {
		t.Fatal("vendor identity changed across invocations")
	}
	t.Logf("%s %s identity present and stable across two invocations", id.Vendor, id.Version)
}

func TestAntigravityRuntimeVersionDoesNotVerifyTheCLIVersion(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  string
	}{
		{"cli-1.5.10", "ls:cli-1.5.10"},
		{"cli-1.5.11", "ls:cli-1.5.11"},
		{"", "unknown"},
		{"cli-1.5.10\ninvalid", "unknown"},
	} {
		id, err := ResolveEnvironment(lookupMap(map[string]string{
			"ANTIGRAVITY_CONVERSATION_ID": "test-member", "ANTIGRAVITY_LS_VERSION": tc.value,
		}))
		if err != nil || id.Version != tc.want {
			t.Fatalf("runtime version %q: got %q (%v), want %q", tc.value, id.Version, err, tc.want)
		}
		if _, verified := id.Evidence(); verified {
			t.Fatalf("language server %q verified an unknown CLI release", tc.value)
		}
		if tc.want != "unknown" && (!strings.Contains(id.Warning(), "CLI version unknown") || !strings.Contains(id.Warning(), tc.value)) {
			t.Fatalf("ambiguous version diagnostic: %q", id.Warning())
		}
	}
}
