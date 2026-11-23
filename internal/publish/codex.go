package publish

import (
	"strings"

	"github.com/BeomSeogKim/crosshall/internal/notice"
)

// codex is Codex: a thread in the managed app server that its terminal
// client runs against. It stays a candidate until trusted capture is shown
// (design section 3), so it has no transport.
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

// SessionRoute: a Codex session reference is its thread, and the launch's
// mode says whether the managed server holds it.
func (codex) SessionRoute(sessionRef, launchMode string) Route {
	return Route{ThreadID: sessionRef, Mode: launchMode}
}

// Reach: only the managed server takes a queued message for a thread while
// its terminal client is attached; a client that runs its own server never
// sees one (#236).
func (codex) Reach(r Route) string {
	switch r.Mode {
	case ModeDaemon:
		return ""
	case ModeInProcess:
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

func (codex) Transport() Transport { return Transport{} }
