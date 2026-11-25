//go:build conformance

package conformance

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/hook"
	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/notice"
)

type nativeCrossVendorPeer struct {
	member   *crossVendorSession
	version  string
	mode     string
	turn     func(*testing.T, string) string
	armAwait func(*testing.T) func() string
	lastTurn string
}

func (p *nativeCrossVendorPeer) sawTurn(t *testing.T, id string) {
	t.Helper()
	if id == "" || id == p.lastTurn {
		t.Fatalf("%s did not complete a new turn: %q", p.member.model.vendor, id)
	}
	p.lastTurn = id
}

func TestCrossVendorNativeRoundTrip(t *testing.T) {
	f := newNativeCrossVendorFixture(t, "")
	for _, direction := range []struct {
		name            string
		asker, answerer *nativeCrossVendorPeer
	}{
		{"claude_to_codex_notice", f.claudePeer, f.codexPeer},
		{"codex_to_claude_notice", f.codexPeer, f.claudePeer},
	} {
		if !t.Run(direction.name, func(t *testing.T) {
			runNativeCrossVendorRoundTrip(t, f.db, f.commands, direction.asker, direction.answerer)
		}) {
			return
		}
	}
}

type nativeCrossVendorFixture struct {
	db                    *sql.DB
	codex                 *nativeCodexSession
	claudePeer, codexPeer *nativeCrossVendorPeer
	commands              crossVendorCommands
	codexEvidenceSource   string
}

func newNativeCrossVendorFixture(t *testing.T, codexVersion string) *nativeCrossVendorFixture {
	t.Helper()
	pkg := nativeCodexPackage(t)
	if codexVersion != "" && pkg.Version != codexVersion {
		t.Fatalf("publish trial needs Codex %s, found %s", codexVersion, pkg.Version)
	}
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skipf("native cross-vendor conformance needs Claude Code on PATH: %v", err)
	}
	if runtime.GOOS == "linux" && os.Geteuid() == 0 {
		t.Fatal("run Linux native conformance as a non-root user so fixture cleanup can fence new launches")
	}

	root, entry := armNativeFixture(t)
	env := codexEnvWithReaper(t, root, entry)
	env = append(env, "CROSSHALL_TELEMETRY=off", "CROSSHALL_NOTIFY=off", "XDG_RUNTIME_DIR="+filepath.Join(root, "tmp"))
	repo := filepath.Join(root, "repo")
	codexMust(t, os.Mkdir(repo, 0700))
	git := exec.Command("git", "init", "-q", repo)
	git.Env = env
	codexMust(t, git.Run())
	exe := codexBinaryWithEnv(t, root, []string{entry})

	claudeModel := &crossVendorModel{vendor: "claude"}
	codexModel := &crossVendorModel{vendor: "codex"}
	mux := http.NewServeMux()
	mux.Handle("POST /v1/messages", claudeModel)
	mux.Handle("POST /v1/responses", codexModel)
	mux.HandleFunc("POST /v1/messages/count_tokens", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"input_tokens":1}`)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	state := filepath.Join(root, "state")
	reserved, err := os.Lstat(ident.Rendezvous(state, os.Geteuid()))
	codexMust(t, err)
	startCodexDispatcherWithRendezvous(t, exe, state, env, reserved)
	owner := func(t *testing.T, args ...string) {
		t.Helper()
		codexMust(t, codexCLI(exe, env, append([]string{"owner"}, args...)...))
	}
	owner(t, "channel", "create", crossVendorChannel)
	owner(t, "channel", "create", "lobby")
	owner(t, "channel", "default", "lobby", "--repo", repo)
	owner(t, "publish", "hall", "off")
	owner(t, "adapter", "set", "claude", "--enabled", "on")

	claude := newNativeClaude(t, root, repo, exe, env, entry, server)
	codex := newNativeCodex(t, pkg, root, repo, exe, env, entry, server.URL)
	claudePeer := &nativeCrossVendorPeer{
		member:  &crossVendorSession{model: claudeModel, id: claude.id},
		version: claude.version, mode: "interactive", turn: claude.turn, armAwait: claude.armAwait,
	}
	codexPeer := &nativeCrossVendorPeer{
		member:  &crossVendorSession{model: codexModel, id: codex.id},
		version: codex.version, mode: "daemon", turn: codex.turn, armAwait: codex.armAwait,
	}
	commands := crossVendorCommands{exe}
	db := openCodexLedger(t, state)
	codexEvidenceSource := "owner"
	for _, peer := range []*nativeCrossVendorPeer{claudePeer, codexPeer} {
		vendor := peer.member.model.vendor
		owner(t, "adapter", "set", vendor, "--turn-end", "off")
		peer.member.model.begin("", []crossVendorStep{
			commands.command("join "+crossVendorChannel+" --move --as "+vendor, "Joined #"+crossVendorChannel+" as @"+vendor),
		})
		peer.sawTurn(t, peer.turn(t, crossVendorPrompt))
		peer.member.model.verify(t)
		peer.member.model.begin("", nil)
		peer.sawTurn(t, peer.turn(t, "Finish the fixture setup."))
		peer.member.model.verify(t)
		var builtin bool
		codexMust(t, db.QueryRowContext(t.Context(), `SELECT EXISTS (SELECT 1 FROM capability_evidence
			WHERE vendor = ? AND vendor_version = ? AND platform = ? AND mode = ? AND manifest_rev = ?
			AND cap = 'publish' AND level = 'tested' AND source = 'builtin')`, vendor, peer.version,
			runtime.GOOS+"-"+runtime.GOARCH, peer.mode, nativeFixtureManifestRevision(t, vendor)).Scan(&builtin))
		if builtin {
			if peer == codexPeer {
				codexEvidenceSource = "builtin"
			}
			continue
		}
		file := filepath.Join(root, vendor+"-fixture-publish.json")
		writeNativeFixtureEvidence(t, peer, file)
		owner(t, "adapter", "evidence", file)
	}

	owner(t, "publish", "hall", "on")
	for _, peer := range []*nativeCrossVendorPeer{codexPeer, claudePeer} {
		awaitCrossVendorNativeRoute(t, db, peer.member)
	}

	return &nativeCrossVendorFixture{
		db: db, codex: codex, claudePeer: claudePeer, codexPeer: codexPeer, commands: commands,
		codexEvidenceSource: codexEvidenceSource,
	}
}

func writeNativeFixtureEvidence(t *testing.T, peer *nativeCrossVendorPeer, file string) {
	t.Helper()
	vendor := peer.member.model.vendor
	raw, err := json.Marshal(map[string]any{
		"vendor": vendor, "vendor_version": peer.version, "mode": peer.mode, "manifest_rev": nativeFixtureManifestRevision(t, vendor),
		"platform": runtime.GOOS + "-" + runtime.GOARCH, "run_at_ms": time.Now().UnixMilli(),
		"results": []evidenceResult{{"publish", "tested", "scratch hall only: exercise the native sender"}},
	})
	codexMust(t, err)
	codexPut(t, file, raw)
}

func nativeFixtureManifestRevision(t *testing.T, vendor string) string {
	t.Helper()
	for _, spec := range hook.InstallSpecs() {
		if spec.Adapter == vendor {
			return spec.Revision
		}
	}
	t.Fatalf("no hook manifest for %s", vendor)
	return ""
}

func awaitCrossVendorNativeRoute(t *testing.T, db *sql.DB, member *crossVendorSession) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	vendor := member.model.vendor
	var state, reason string
	for {
		var active bool
		err := db.QueryRowContext(ctx, `SELECT r.state, r.reason,
			r.state = 'active' AND p.activated_at_ms IS NOT NULL
			FROM publish_route r JOIN binding b ON b.id = r.binding_id
			JOIN principal_key k ON k.principal_id = r.principal_id
			JOIN channel ch ON ch.id = b.channel_id
			LEFT JOIN publish_route_proof p ON p.binding_id = r.binding_id AND p.generation = r.generation
			WHERE k.vendor = ? AND k.key = ? AND b.state = 'live' AND ch.name = ?`,
			vendor, member.id, crossVendorChannel).Scan(&state, &reason, &active)
		if err == nil && active {
			return
		}
		if err != nil && err != sql.ErrNoRows && ctx.Err() == nil {
			t.Fatal(err)
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
			t.Fatalf("%s did not activate its native route (state %q, reason %q)", vendor, state, reason)
		}
	}
}

func runNativeCrossVendorRoundTrip(t *testing.T, db *sql.DB, commands crossVendorCommands, asker, answerer *nativeCrossVendorPeer) int64 {
	t.Helper()
	from, to := asker.member.model.vendor, answerer.member.model.vendor
	request := from + " native cross-vendor request payload"
	answer := to + " native cross-vendor answer payload"

	ask := commands.ask(to, request)
	ask.wants = append(ask.wants, "notice queued")
	reply := commands.reply(from, answer)
	reply.wants = append(reply.wants, "notice queued")

	var askID int64
	idReady := make(chan struct{})
	release := sync.OnceFunc(func() { close(idReady) })
	defer release()
	answerer.member.model.beginWithFirstRequest("", []crossVendorStep{commands.readAsk(from, request), reply},
		nativeCrossVendorNotice(to, func(ctx context.Context) (int64, error) {
			select {
			case <-idReady:
				return askID, nil
			case <-ctx.Done():
				return 0, ctx.Err()
			}
		}))
	awaitAsk := answerer.armAwait(t)
	asker.member.model.begin("", []crossVendorStep{ask})
	asker.sawTurn(t, asker.turn(t, crossVendorPrompt))
	asker.member.model.verify(t)

	var err error
	askID, err = crossVendorMessageID(t.Context(), db, "ask", request)
	codexMust(t, err)
	// Hold the first receiving model call until the sender can consume the
	// native reply in its next turn.
	asker.member.model.beginWithFirstRequest("", []crossVendorStep{commands.readReply(to, answer, askID)},
		nativeCrossVendorNotice(from, func(ctx context.Context) (int64, error) {
			return crossVendorMessageID(ctx, db, "reply", answer)
		}))
	awaitReply := asker.armAwait(t)
	release()

	answerer.sawTurn(t, awaitAsk())
	answerer.member.model.verify(t)
	asker.sawTurn(t, awaitReply())
	asker.member.model.verify(t)
	replyID, err := crossVendorMessageID(t.Context(), db, "reply", answer)
	codexMust(t, err)

	assertCrossVendorNativeDelivery(t, db, answerer.member, askID)
	assertCrossVendorNativeDelivery(t, db, asker.member, replyID)
	assertCrossVendorSettlement(t, db, asker.member, answerer.member, askID, request, answer)
	return askID
}

func nativeCrossVendorNotice(vendor string, messageID func(context.Context) (int64, error)) func(context.Context, []byte) (string, error) {
	return func(ctx context.Context, raw []byte) (string, error) {
		id, err := messageID(ctx)
		if err != nil {
			return "", err
		}
		message := strconv.FormatInt(id, 10)
		if id <= 0 || !bytes.Contains(raw, []byte(notice.PendingPrefix+message+".")) {
			return "", fmt.Errorf("missing native %s notice for message #%d in model input", vendor, id)
		}
		return message, nil
	}
}

func assertCrossVendorNativeDelivery(t *testing.T, db *sql.DB, member *crossVendorSession, id int64) {
	t.Helper()
	var attempts, observed int
	codexMust(t, db.QueryRowContext(t.Context(), `SELECT COUNT(*),
		COALESCE(SUM(a.state = 'submitted' AND a.observation = 'own_turn' AND a.observed_at_ms IS NOT NULL), 0)
		FROM publish_attempt a JOIN publish_attempt_item i ON i.attempt_id = a.id
		WHERE a.adapter = ? AND a.session_ref = ? AND i.message_id = ?`,
		member.model.vendor, member.id, id).Scan(&attempts, &observed))
	if attempts != 1 || observed != 1 {
		t.Fatalf("%s native message #%d: %d attempts and %d submitted own turns, want one each",
			member.model.vendor, id, attempts, observed)
	}
}
