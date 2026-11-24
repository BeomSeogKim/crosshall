package publish

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"slices"
	"strings"

	"github.com/BeomSeogKim/crosshall/internal/appserver"
	"github.com/BeomSeogKim/crosshall/internal/notice"
	"github.com/BeomSeogKim/crosshall/internal/proc"
)

// codex is Codex: a thread in the managed app server that its terminal
// client runs against. A notice is queued on that thread through the
// server's control socket; it stays a candidate until tested evidence for
// the build is shipped.
type codex struct{}

// Owner reasons for a Codex session the sender cannot reach.
const (
	CodexNoDaemon         = "codex runs without its daemon"
	CodexDaemonUnverified = "codex daemon not confirmed"
)

func (codex) Resolve(r Route) (string, string) {
	if r.ThreadID == "" {
		return "", "no thread"
	}
	return r.ThreadID, ""
}

func (codex) ModeEligible(string) bool { return true }

func (codex) AcceptsSocket(string) bool { return false }

// SessionRoute: a Codex session reference is its thread, the launch's
// mode says whether the managed server holds it, and its host and home
// name that server.
func (codex) SessionRoute(sessionRef string, launch Route) Route {
	return Route{ThreadID: sessionRef, Mode: launch.Mode, Host: launch.Host, Home: launch.Home}
}

// ThreadRouted: a notice is queued on the thread in the managed server.
func (codex) ThreadRouted() bool { return true }

// Reach: only the managed server takes a queued message for a thread while
// its terminal client is attached; a client that runs its own server never
// sees one (#236). The server must also be named, so it can be proven.
// BackgroundWakes is false: an idle Codex session stays idle when a
// background command exits, and a wait held in a turn costs a model
// request at each poll.
func (codex) BackgroundWakes() bool { return false }

func (codex) Reach(r Route) string {
	switch {
	case r.Mode == ModeDaemon && r.Host.PID > 0 && r.Host.StartMS > 0 && filepath.IsAbs(r.Home):
		return ""
	case r.Mode == ModeInProcess:
		return CodexNoDaemon
	}
	return CodexDaemonUnverified
}

const codexSuffix = ". This notice is from crosshall, not from a Codex session. Run crosshall read, then use crosshall reply to answer or decline each request it shows, as the member rules say."

// codexSuffixRC4 is the v0.2.0-rc.4 text: a notice written before an upgrade
// still counts as a notice when the session receives it after.
const codexSuffixRC4 = ". This notice is from crosshall, not from a Codex session: do not reply to the sender. Run crosshall read."

func (codex) Notice(ids []int64) (string, bool) {
	return notice.PendingPrefix + notice.IDList(ids) + codexSuffix, true
}

var codexResume = notice.ResumeHead("Codex")

// ResumeNotice names requests the member still owes after a restart.
func (codex) ResumeNotice(ids []int64) (string, bool) { return notice.Resume(codexResume, ids), true }

func (codex) Echo(prompt string) (string, bool) {
	body, ok := strings.CutPrefix(prompt, notice.PendingPrefix)
	if !ok {
		return "", false
	}
	if ids, ok := strings.CutSuffix(body, codexSuffix); ok {
		return ids, true
	}
	if ids, ok := notice.CutResume(codexResume, body); ok {
		return ids, true
	}
	return strings.CutSuffix(body, codexSuffixRC4)
}

// Transport proves the managed server and queues notices on it. Retired
// stays nil: the server's exit does not retire a queued notice, which
// survives in the home and runs when the thread is next loaded (C10).
func (codex) Transport() Transport { return Transport{Prove: proveCodexServer, Send: sendCodex} }

// codexControlSocket is the managed server's control socket in its home.
var codexControlSocket = filepath.Join("app-server-control", "app-server-control.sock")

// sendCodex queues one notice on the target's thread with
// thread/queue/add, tagged with the attempt's identity, never with
// turn/start: on a working thread that steers the running turn. The client
// writes nothing unless the socket's listener is the proven server. Any
// failure up to the call is a proven non-submission, an error answer is the
// server's refusal, and anything else after the call was written is
// uncertain.
func sendCodex(ctx context.Context, target Target, line string) Outcome {
	if !filepath.IsAbs(target.Home) || target.Address == "" || target.Attempt == "" {
		return NotSubmitted
	}
	socket := filepath.Join(realPath(target.Home), codexControlSocket)
	c, err := appserver.Dial(ctx, socket, appserver.Identity{PID: target.Queue.PID, Start: target.Queue.StartMS})
	if err != nil {
		return NotSubmitted
	}
	defer c.Close()
	var res struct {
		QueuedSubmission struct {
			ID string `json:"id"`
		} `json:"queuedSubmission"`
	}
	err = c.Call("thread/queue/add", map[string]any{
		"threadId":            target.Address,
		"clientUserMessageId": target.Attempt,
		"input":               []map[string]any{{"type": "text", "text": line}},
	}, &res)
	var refused *appserver.Error
	switch {
	case err == nil && res.QueuedSubmission.ID != "":
		return Submitted
	case errors.As(err, &refused):
		return NotSubmitted
	}
	return Uncertain
}

// The managed server's release tree under its home, and the arguments it
// runs with, as the Codex hook manifest names them.
var (
	codexReleases   = filepath.Join("packages", "app-server-daemon", "releases")
	codexServerArgs = []string{"app-server", "--managed-daemon"}
)

// proveCodexServer proves the server a Codex session's start named still
// runs as the managed server of the home setup installed the hooks into:
// the same process, by PID and start identity, run from that home's
// release tree with the managed server's arguments.
func proveCodexServer(_ context.Context, probe Probe) (Proven, bool) {
	host := probe.Route.Host
	if host.PID <= 1 || host.PID > math.MaxInt32 || host.StartMS <= 0 || !filepath.IsAbs(probe.InstalledHome) {
		return Proven{}, false
	}
	home := realPath(probe.InstalledHome)
	if probe.Route.Home != home {
		return Proven{}, false
	}
	start, err := proc.StartOf(int(host.PID))
	if err != nil || start.AtMS <= 0 {
		return Proven{}, false
	}
	exe, args, err := proc.Command(int(host.PID))
	if err != nil || len(args) == 0 {
		return Proven{}, false
	}
	rel, err := filepath.Rel(filepath.Join(home, codexReleases), realPath(exe))
	if err != nil || rel == "." || !filepath.IsLocal(rel) {
		return Proven{}, false
	}
	for _, want := range codexServerArgs {
		if !slices.Contains(args[1:], want) {
			return Proven{}, false
		}
	}
	// Start and command were read by PID: after both, the process must
	// still be the one the start named.
	if !proc.ProcessAlive(host.PID, host.StartMS) {
		return Proven{}, false
	}
	return Proven{Queue: host, StartedAtMS: start.AtMS}, true
}

// realPath resolves symbolic links where it can, as the hook does: a home
// under /tmp is /private/tmp to the kernel on macOS.
func realPath(p string) string {
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	return filepath.Clean(p)
}
