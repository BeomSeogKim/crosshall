package dispatch

import (
	"encoding/json"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

// linkServer is a home dispatcher with two linked devices and two
// channels. Every OS probe fails the test: a forwarded command never
// looks at a process on the home device.
func linkServer(t *testing.T) *server {
	t.Helper()
	s := reviewServer(t)
	probe := func(name string) { t.Errorf("a link call probed %s on the home device", name) }
	s.cfg.Handle = &domain.Service{
		LaunchAlive:        func(string, int64) bool { probe("LaunchAlive"); return false },
		LaunchProcess:      func(string) (string, bool) { probe("LaunchProcess"); return "", false },
		LaunchPIDNamespace: func() string { probe("LaunchPIDNamespace"); return "" },
		ProcessGone:        func(int64, int64) bool { probe("ProcessGone"); return false },
		ProcessAlive:       func(int64, int64) bool { probe("ProcessAlive"); return false },
		HasPromptLine:      func(string, string) bool { probe("HasPromptLine"); return false },
		HasSessionEnd:      func(string, string) bool { probe("HasSessionEnd"); return false },
		LeafReceipts: func(string, string, string, string, ...core.Membership) (uint64, uint64) {
			probe("LeafReceipts")
			return 0, 0
		},
	}
	s.fixtureSQL(t, `INSERT INTO principal (kind, vendor, created_at_ms) VALUES ('owner', 'owner', 1);
		INSERT INTO channel (name, created_at_ms) VALUES ('checkout', 1), ('payments', 1);
		INSERT INTO device (device_id, name, secret, created_at_ms, secret_at_ms) VALUES
			('dev-a', 'macbook', x'00', 1, 1), ('dev-b', 'laptop', x'00', 1, 1);`)
	return s
}

type linkReq struct {
	device, key, gen string
	cred, epoch      int64
	command          string
	args             any
	op               string
	membership       *core.Membership
}

func (s *server) link(t *testing.T, r linkReq) core.Result {
	t.Helper()
	if r.device == "" {
		r.device = "dev-a"
	}
	if r.key == "" {
		r.key = "k1"
	}
	if r.gen == "" {
		r.gen = "g1"
	}
	if r.cred == 0 {
		r.cred = 1
	}
	if r.epoch == 0 {
		r.epoch = 1
	}
	args, err := json.Marshal(r.args)
	if err != nil {
		t.Fatal(err)
	}
	call, err := json.Marshal(domain.LinkCall{Device: r.device, CredentialGen: r.cred, Vendor: "claude", Key: r.key,
		LeafGeneration: r.gen, LeafEpoch: r.epoch, Command: r.command, Args: args, OpKey: r.op, Membership: r.membership})
	if err != nil {
		t.Fatal(err)
	}
	return s.fixtureRequest(t, Arrival{Owner: true}, wire.Request{Verb: domain.VerbLinkCall, Args: call})
}

func (s *server) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.db.Update(t.Context(), func(tx store.Tx) error {
		return tx.QueryRowContext(t.Context(), query, args...).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

func mustLinkOK(t *testing.T, r core.Result) core.Result {
	t.Helper()
	if r.Status != core.ExitOK {
		t.Fatalf("status %d code %s data %s", r.Status, r.Code, r.Data)
	}
	return r
}

func mustLinkCode(t *testing.T, r core.Result, code core.Code) {
	t.Helper()
	if r.Code != code {
		t.Fatalf("code %q (status %d, %s), want %q", r.Code, r.Status, r.Data, code)
	}
}

func joinChannel(name string) map[string]any { return map[string]any{"channel": name} }

func TestALinkedDeviceMemberJoinsAndIsKeyedByDevice(t *testing.T) {
	s := linkServer(t)
	r := mustLinkOK(t, s.link(t, linkReq{command: domain.VerbJoin, args: joinChannel("checkout"), op: "j1"}))
	if r.Membership == nil || r.Membership.BindingID == 0 {
		t.Fatalf("join returned no membership to stamp: %+v", r)
	}
	if n := s.count(t, `SELECT COUNT(*) FROM device_key k JOIN principal p ON p.id = k.principal_id
		WHERE k.device_id = 'dev-a' AND p.device_id = 'dev-a' AND k.leaf_epoch = 1`); n != 1 {
		t.Fatalf("%d device keys", n)
	}
	// No home key, leaf, launch or route for a device member.
	if n := s.count(t, `SELECT (SELECT COUNT(*) FROM principal_key) + (SELECT COUNT(*) FROM leaf)
		+ (SELECT COUNT(*) FROM adapter_launch) + (SELECT COUNT(*) FROM publish_route)`); n != 0 {
		t.Fatalf("%d home-local rows", n)
	}
	// The same vendor and key on another device is another member.
	mustLinkOK(t, s.link(t, linkReq{device: "dev-b", cred: 1, command: domain.VerbJoin, args: joinChannel("checkout"), op: "j1"}))
	if n := s.count(t, `SELECT COUNT(DISTINCT principal_id) FROM device_key WHERE key = 'k1'`); n != 2 {
		t.Fatalf("%d principals for one key on two devices", n)
	}
	// A repeated first join of one tuple resolves to the same member.
	mustLinkOK(t, s.link(t, linkReq{command: domain.VerbJoin, args: joinChannel("checkout"), op: "j2", membership: r.Membership}))
	if n := s.count(t, `SELECT COUNT(*) FROM device_key WHERE device_id = 'dev-a'`); n != 1 {
		t.Fatalf("%d keys on dev-a", n)
	}
}

func TestALinkCallUnderAnOldCredentialIsRefusedBeforeLookup(t *testing.T) {
	s := linkServer(t)
	mustLinkOK(t, s.link(t, linkReq{command: domain.VerbJoin, args: joinChannel("checkout"), op: "j1"}))
	s.fixtureSQL(t, `UPDATE device SET credential_gen = 2 WHERE device_id = 'dev-a'`)
	mustLinkCode(t, s.link(t, linkReq{command: domain.VerbWho, cred: 1}), domain.CodeLinkStale)
	mustLinkCode(t, s.link(t, linkReq{device: "dev-x", command: domain.VerbWho}), domain.CodeLinkStale)
	// A link authenticated under the new credential resumes the device,
	// and its leaf counter may start again.
	mustLinkOK(t, s.link(t, linkReq{command: domain.VerbWho, cred: 2, epoch: 1, gen: "g9"}))
}

func TestStaleLeafRegistrationsAreRefused(t *testing.T) {
	s := linkServer(t)
	r := mustLinkOK(t, s.link(t, linkReq{command: domain.VerbJoin, args: joinChannel("checkout"), op: "j1", epoch: 3, gen: "g3"}))
	m := r.Membership
	mustLinkCode(t, s.link(t, linkReq{command: domain.VerbWho, epoch: 3, gen: "other", membership: m}), domain.CodeStaleLeaf)
	mustLinkCode(t, s.link(t, linkReq{command: domain.VerbWho, epoch: 2, gen: "g2", membership: m}), domain.CodeStaleLeaf)
	mustLinkOK(t, s.link(t, linkReq{command: domain.VerbWho, epoch: 3, gen: "g3", membership: m}))
	// A newer registration replaces it; the older one is then stale.
	mustLinkOK(t, s.link(t, linkReq{command: domain.VerbWho, epoch: 4, gen: "g4", membership: m}))
	mustLinkCode(t, s.link(t, linkReq{command: domain.VerbWho, epoch: 3, gen: "g3", membership: m}), domain.CodeStaleLeaf)
}

// A forwarded command carries only what the hall can act on: nothing
// about the device's processes, paths or other sessions.
func TestLinkCommandsHaveALinkShape(t *testing.T) {
	s := linkServer(t)
	for name, c := range map[string]struct {
		command string
		args    any
		code    core.Code
	}{
		"launch":        {domain.VerbJoin, map[string]any{"channel": "checkout", "launch": map[string]any{"ref": "x"}}, domain.CodeNotOnADevice},
		"takeover":      {domain.VerbJoin, map[string]any{"channel": "checkout", "alias": "a", "takeover": true}, domain.CodeNotOnADevice},
		"repo root":     {domain.VerbJoin, map[string]any{"channel": "checkout", "repo_root": "/Users/x/repo"}, domain.CodeNotOnADevice},
		"no channel":    {domain.VerbJoin, map[string]any{}, ""},
		"unknown field": {domain.VerbJoin, map[string]any{"channel": "checkout", "host": 1}, ""},
		"exit tracking": {domain.VerbWho, map[string]any{"exit_tracking": true}, domain.CodeNotOnADevice},
		"hook verb":     {domain.VerbHookEvent, map[string]any{}, domain.CodeNotOnADevice},
		"owner verb":    {domain.VerbCreateChannel, map[string]any{"name": "x"}, domain.CodeNotOnADevice},
	} {
		r := s.link(t, linkReq{command: c.command, args: c.args, op: "op-" + name})
		if r.Status == core.ExitOK || c.code != "" && r.Code != c.code {
			t.Errorf("%s: status %d code %q", name, r.Status, r.Code)
		}
	}
	if n := s.count(t, `SELECT (SELECT COUNT(*) FROM device_key) + (SELECT COUNT(*) FROM principal WHERE kind = 'session')
		+ (SELECT COUNT(*) FROM channel WHERE name = 'x')`); n != 0 {
		t.Fatalf("%d rows written by refused forms", n)
	}
}

func TestAPlainJoinElsewhereIsRefusedAndAMoveIsExplicit(t *testing.T) {
	s := linkServer(t)
	m := mustLinkOK(t, s.link(t, linkReq{command: domain.VerbJoin, args: joinChannel("checkout"), op: "j1"})).Membership
	r := s.link(t, linkReq{command: domain.VerbJoin, args: joinChannel("payments"), op: "j2", membership: m})
	if r.Status == core.ExitOK {
		t.Fatalf("a plain join moved the member: %+v", r)
	}
	if n := s.count(t, `SELECT COUNT(*) FROM member_selection s JOIN binding b ON b.id = s.binding_id
		JOIN channel c ON c.id = b.channel_id WHERE c.name = 'checkout'`); n != 1 {
		t.Fatalf("the source selection changed: %d", n)
	}
	moved := mustLinkOK(t, s.link(t, linkReq{command: domain.VerbJoin, args: map[string]any{"channel": "payments", "move": true},
		op: "j3", membership: m})).Membership
	if moved == nil || moved.BindingID == m.BindingID {
		t.Fatalf("move membership %+v, before %+v", moved, m)
	}
	if n := s.count(t, `SELECT COUNT(*) FROM member_selection s JOIN binding b ON b.id = s.binding_id
		JOIN channel c ON c.id = b.channel_id WHERE c.name = 'payments'`); n != 1 {
		t.Fatalf("the move did not select #payments: %d", n)
	}
}

// An ask whose response was lost, then a move, then the retry: the retry
// gets the original receipt and asks nothing again. A retry of an ask
// that never committed is refused once the member moved.
func TestARetryAfterAMoveGetsTheCommittedReceiptOnly(t *testing.T) {
	s := linkServer(t)
	m := mustLinkOK(t, s.link(t, linkReq{command: domain.VerbJoin, args: joinChannel("checkout"), op: "j1"})).Membership
	mustLinkOK(t, s.link(t, linkReq{device: "dev-b", command: domain.VerbJoin, args: joinChannel("checkout"), op: "jb"}))
	ask := map[string]any{"all": true, "each": true, "body": "status?"}
	first := mustLinkOK(t, s.link(t, linkReq{command: domain.VerbAsk, args: ask, op: "a1", membership: m}))
	moved := mustLinkOK(t, s.link(t, linkReq{command: domain.VerbJoin, args: map[string]any{"channel": "payments", "move": true},
		op: "j2", membership: m})).Membership
	again := mustLinkOK(t, s.link(t, linkReq{command: domain.VerbAsk, args: ask, op: "a1", membership: m}))
	// The stored receipt is the original ask; replay adds no live fields.
	var a, b struct {
		ID      int64  `json:"id"`
		Channel string `json:"channel"`
	}
	if json.Unmarshal(first.Data, &a) != nil || json.Unmarshal(again.Data, &b) != nil || a != b || a.ID == 0 || b.Channel != "checkout" {
		t.Fatalf("retry %s, first %s", again.Data, first.Data)
	}
	if n := s.count(t, `SELECT COUNT(*) FROM message WHERE body = 'status?'`); n != 1 {
		t.Fatalf("%d asks stored", n)
	}
	// The same key for another command, or under another membership, is
	// a conflict, not a replay.
	mustLinkCode(t, s.link(t, linkReq{command: domain.VerbAsk, args: map[string]any{"all": true, "each": true, "body": "other"}, op: "a1", membership: moved}),
		core.CodeKeyConflict)
	// Never committed: admission refuses it under the old membership.
	r := s.link(t, linkReq{command: domain.VerbAsk, args: map[string]any{"all": true, "each": true, "body": "late"}, op: "a2", membership: m})
	mustLinkCode(t, r, core.CodeBindingChanged)
	if n := s.count(t, `SELECT COUNT(*) FROM message WHERE body = 'late'`); n != 0 {
		t.Fatalf("%d late asks stored", n)
	}
}

func TestOnlyTheOwnerIngressCarriesALinkCall(t *testing.T) {
	s := linkServer(t)
	call, _ := json.Marshal(domain.LinkCall{Device: "dev-a", CredentialGen: 1, Vendor: "claude", Key: "k1",
		LeafGeneration: "g1", LeafEpoch: 1, Command: domain.VerbWho})
	r := s.fixtureRequest(t, Arrival{}, wire.Request{Verb: domain.VerbLinkCall, Args: call})
	if r.Status == core.ExitOK {
		t.Fatalf("a member ingress ran a link call: %+v", r)
	}
}
