package publish

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"unicode"

	"github.com/BeomSeogKim/crosshall/internal/notice"
)

// claude is Claude Code: a session inbox socket, registered by SessionStart.
// It has no transport in this build.
type claude struct{}

func (claude) Complete(r Route) (bool, string) {
	if r.SocketPath == "" {
		return false, "no socket"
	}
	return true, ""
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

func (claude) Notice(ids []int64) (string, bool) { return notice.Claude(ids), true }

func (claude) Transport() Transport { return Transport{} }

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
