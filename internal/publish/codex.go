package publish

import (
	"context"
	"math"
	"slices"
	"strings"

	"github.com/BeomSeogKim/crosshall/internal/notice"
	"github.com/BeomSeogKim/crosshall/internal/proc"
)

// codex is Codex: a thread in the managed app server that its terminal
// client runs against. It stays a candidate until trusted capture is shown
// (design section 3): its transport proves the server but has no sender.
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
// mode says whether the managed server holds it, and host is that server.
func (codex) SessionRoute(sessionRef, launchMode string, host Queue) Route {
	return Route{ThreadID: sessionRef, Mode: launchMode, Host: host}
}

// ThreadRouted: a notice is queued on the thread in the managed server.
func (codex) ThreadRouted() bool { return true }

// Reach: only the managed server takes a queued message for a thread while
// its terminal client is attached; a client that runs its own server never
// sees one (#236). The server must also be named, so it can be proven.
func (codex) Reach(r Route) string {
	switch {
	case r.Mode == ModeDaemon && r.Host.PID > 0 && r.Host.StartMS > 0:
		return ""
	case r.Mode == ModeInProcess:
		return CodexNoDaemon
	}
	return CodexDaemonUnverified
}

const codexSuffix = ". This notice is from crosshall, not from a Codex session: do not reply to the sender. Run crosshall read."

func (codex) Notice(ids []int64) (string, bool) {
	return notice.PendingPrefix + notice.IDList(ids) + codexSuffix, true
}

func (codex) Echo(prompt string) (string, bool) {
	body, ok := strings.CutPrefix(prompt, notice.PendingPrefix)
	if !ok {
		return "", false
	}
	return strings.CutSuffix(body, codexSuffix)
}

// Transport proves the managed server; it has no sender yet.
func (codex) Transport() Transport { return Transport{Prove: proveCodexServer} }

// codexServerArgs are the arguments the managed server runs with, as the
// Codex hook manifest names them.
var codexServerArgs = []string{"app-server", "--managed-daemon"}

// proveCodexServer proves the server a Codex session's start named still
// runs as the managed server: the same process, by PID and start identity,
// with the managed server's arguments.
func proveCodexServer(_ context.Context, probe Probe) (Proven, bool) {
	host := probe.Route.Host
	if host.PID <= 1 || host.PID > math.MaxInt32 || host.StartMS <= 0 {
		return Proven{}, false
	}
	start, err := proc.StartOf(int(host.PID))
	if err != nil || start.AtMS <= 0 {
		return Proven{}, false
	}
	_, args, err := proc.Command(int(host.PID))
	if err != nil || len(args) == 0 {
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
