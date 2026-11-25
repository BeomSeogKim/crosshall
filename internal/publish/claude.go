package publish

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/BeomSeogKim/crosshall/internal/notice"
	"github.com/BeomSeogKim/crosshall/internal/proc"
)

// claude is Claude Code: a session inbox socket, registered by SessionStart.
type claude struct{}

func (c claude) Resolve(r Route) (string, string) {
	switch {
	case r.SocketPath == "":
		return "", "no socket"
	case !c.ModeEligible(r.Mode):
		return "", "bypass mode"
	}
	return r.SocketPath, ""
}

func (claude) ModeEligible(mode string) bool {
	return mode != "bypassPermissions" && mode != "bypass"
}

func (claude) AcceptsSocket(path string) bool {
	if path == "" || len(path) > 512 || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return false
	}
	if strings.IndexFunc(path, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return false
	}
	sock := filepath.Base(path)
	if sock == ".sock" || !strings.HasSuffix(sock, ".sock") {
		return false
	}
	return slices.Contains(claudeSocketDirs(), filepath.Dir(path))
}

// SessionRoute is empty: SessionStart's hook.route supplies the socket.
func (claude) SessionRoute(string, Route) Route { return Route{} }

// ThreadRouted is false: a Claude route is the launch's own process.
func (claude) ThreadRouted() bool { return false }

func (claude) Reach(Route) string { return "" }

// BackgroundWakes is true: Claude Code starts a turn when a background
// Bash command exits.
func (claude) BackgroundWakes() bool { return true }

const claudeSuffix = ". This notice is from crosshall, not from a Claude session. Run crosshall read, then use crosshall reply to answer or decline each request it shows, as the member rules say."

// claudeSuffixRC4 is the v0.2.0-rc.4 text: a notice written before an upgrade
// still counts as a notice when the session receives it after.
const claudeSuffixRC4 = ". This notice is from crosshall, not from a Claude session: do not reply to the sender. Run crosshall read."

func (claude) Notice(ids []int64) (string, bool) {
	return notice.PendingPrefix + notice.IDList(ids) + claudeSuffix, true
}

var claudeResume = notice.ResumeHead("Claude")

// ResumeNotice names requests the member still owes after a restart.
func (claude) ResumeNotice(ids []int64) (string, bool) { return notice.Resume(claudeResume, ids), true }

func (claude) Echo(prompt string) (string, bool) {
	body, ok := strings.CutPrefix(prompt, notice.PendingPrefix)
	if !ok {
		return "", false
	}
	if ids, ok := strings.CutSuffix(body, claudeSuffix); ok {
		return ids, true
	}
	if ids, ok := notice.CutResume(claudeResume, body); ok {
		return ids, true
	}
	return strings.CutSuffix(body, claudeSuffixRC4)
}

func (claude) Transport() Transport {
	return Transport{Prove: proveClaude, Send: sendClaude, Retired: claudeRetired}
}

// proveClaude reads the kernel identity of the session inbox socket's
// listener without writing to it.
func proveClaude(ctx context.Context, probe Probe) (Proven, bool) {
	peer, err := proc.VerifySocket(ctx, probe.Address, probe.LaunchRef, probe.StartedHookAtMS)
	if err != nil {
		return Proven{}, false
	}
	return Proven{Queue: Queue{PID: peer.PID, StartMS: peer.Identity}, StartedAtMS: peer.StartMS}, true
}

// claudeMessage is one line of the session inbox socket. "later" queues
// it as its own turn after the current one, never inside it.
type claudeMessage struct {
	Type    string `json:"type"`
	Message struct {
		Content string `json:"content"`
	} `json:"message"`
	From     string `json:"from"`
	Priority string `json:"priority"`
}

// sendClaude writes one notice line to the proven listener. It re-dials
// with the same socket checks and requires the listener the proof named
// before writing. Nothing is written on any failure before the write, so
// that is a proven non-submission; anything after it is writeLine's.
func sendClaude(ctx context.Context, target Target, line string) Outcome {
	msg := claudeMessage{Type: "user", From: "crosshall", Priority: "later"}
	msg.Message.Content = line
	payload, err := json.Marshal(msg)
	if err != nil {
		return NotSubmitted
	}
	payload = append(payload, '\n')
	conn, peer, err := proc.DialSocketPeer(ctx, target.Address)
	if err != nil {
		return NotSubmitted
	}
	defer conn.Close()
	// The proof already checked the launch and its first hook; the same PID
	// and start identity is that process, not a later one with the PID.
	if peer.PID != target.Queue.PID || peer.Identity != target.Queue.StartMS || ctx.Err() != nil {
		return NotSubmitted
	}
	return writeLine(ctx, conn, payload)
}

// lineWriter is the part of a connection writeLine uses.
type lineWriter interface {
	SetWriteDeadline(time.Time) error
	Write([]byte) (int, error)
}

// writeLine writes the whole payload by the context's deadline. Once a
// write is attempted the listener may have read part of it, so every
// failure from here on, a failed deadline included, is uncertain.
func writeLine(ctx context.Context, w lineWriter, payload []byte) Outcome {
	if deadline, ok := ctx.Deadline(); ok {
		if err := w.SetWriteDeadline(deadline); err != nil {
			return Uncertain
		}
	}
	if n, err := w.Write(payload); err != nil || n != len(payload) {
		return Uncertain
	}
	return Submitted
}

// claudeRetired proves the listener's process is gone.
func claudeRetired(_ context.Context, queue Queue) bool {
	return proc.ProcessGone(queue.PID, queue.StartMS)
}

func claudeSocketDirs() []string {
	uid := os.Getuid()
	bases := []string{"/tmp"}
	if runtime.GOOS == "darwin" {
		bases = append(bases, "/private/tmp")
	}
	if tmp := os.TempDir(); tmp != "" && filepath.IsAbs(tmp) {
		bases = append(bases, filepath.Clean(tmp))
	}
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" && filepath.IsAbs(xdg) {
		bases = append(bases, filepath.Clean(xdg))
	}
	var dirs []string
	for _, base := range bases {
		dirs = append(dirs,
			filepath.Join(base, "cc-socks"),
			filepath.Join(base, fmt.Sprintf("cc-socks-%d", uid)),
		)
	}
	return dirs
}

func (claude) ReachReasons() []string { return nil }

func (claude) Evidence() []Evidence { return claudeEvidence }
