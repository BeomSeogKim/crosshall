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

// SessionThread is empty: SessionStart's hook.route supplies the socket.
func (claude) SessionThread(string) string { return "" }

const claudeSuffix = ". This notice is from crosshall, not from a Claude session: do not reply to the sender. Run crosshall inbox."

func (claude) Notice(ids []int64) (string, bool) {
	return notice.PendingPrefix + notice.IDList(ids) + claudeSuffix, true
}

func (claude) Echo(prompt string) (string, bool) {
	body, ok := strings.CutPrefix(prompt, notice.PendingPrefix)
	if !ok {
		return "", false
	}
	return strings.CutSuffix(body, claudeSuffix)
}

func (claude) Transport() Transport {
	return Transport{Prove: proveClaude, Send: sendClaude, Retired: claudeRetired}
}

// proveClaude reads the kernel identity of the session inbox socket's
// listener without writing to it.
func proveClaude(ctx context.Context, probe Probe) (Queue, bool) {
	peer, err := proc.VerifySocket(ctx, probe.Address, probe.LaunchRef, probe.StartedHookAtMS)
	if err != nil {
		return Queue{}, false
	}
	return Queue{PID: peer.PID, StartMS: peer.StartMS}, true
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
// with the same kernel checks and requires the listener the proof named
// (PID and start time) before writing. Nothing is written on any failure
// before the write, so that is a proven non-submission; a partial write,
// a write error or the deadline during the write is uncertain.
func sendClaude(ctx context.Context, target Target, line string) Outcome {
	msg := claudeMessage{Type: "user", From: "crosshall", Priority: "later"}
	msg.Message.Content = line
	payload, err := json.Marshal(msg)
	if err != nil {
		return NotSubmitted
	}
	payload = append(payload, '\n')
	conn, peer, err := proc.DialVerifiedSocket(ctx, target.Address, fmt.Sprintf("pid-%d", target.Queue.PID), target.Queue.StartMS)
	if err != nil {
		return NotSubmitted
	}
	defer conn.Close()
	if peer.PID != target.Queue.PID || peer.StartMS != target.Queue.StartMS || ctx.Err() != nil {
		return NotSubmitted
	}
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetWriteDeadline(deadline); err != nil {
			return NotSubmitted
		}
	}
	if n, err := conn.Write(payload); err != nil || n != len(payload) {
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
