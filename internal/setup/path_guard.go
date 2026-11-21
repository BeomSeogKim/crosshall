package setup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

var errSetupRepository = errors.New("adapter setup paths must be outside Git worktrees and repository directories")

type setupPathGuard struct{ repositories []repositoryRoot }

type repositoryRoot struct {
	path string
	info os.FileInfo
}

// Git flags passed only to another process leave no worktree marker or
// configuration for setup to discover. Explicit Git environment and local
// repository metadata are the scope of this guard.
func newSetupPathGuard() (setupPathGuard, error) {
	var guard setupPathGuard
	paths := map[string]string{}
	for _, key := range []string{"GIT_WORK_TREE", "GIT_DIR", "GIT_COMMON_DIR"} {
		if value := os.Getenv(key); value != "" {
			path, err := filepath.Abs(value)
			if err != nil {
				return guard, err
			}
			paths[key] = path
			if err := guard.addRoot(path); err != nil {
				return guard, err
			}
		}
	}
	if paths["GIT_WORK_TREE"] == "" && paths["GIT_DIR"] != "" {
		worktree, err := effectiveWorktree(paths["GIT_DIR"], paths["GIT_COMMON_DIR"])
		if err != nil {
			return guard, err
		}
		if worktree != "" {
			if err := guard.addRoot(worktree); err != nil {
				return guard, err
			}
		}
	}
	return guard, nil
}

func effectiveWorktree(gitDir, commonDir string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	// Inherit the invocation directory. A non-bare GIT_DIR without an
	// explicit worktree uses that directory as its worktree.
	cmd := exec.CommandContext(ctx, "git", "--git-dir", gitDir, "rev-parse", "--is-bare-repository", "--show-toplevel")
	cmd.Env = []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null"}
	if commonDir != "" {
		cmd.Env = append(cmd.Env, "GIT_COMMON_DIR="+commonDir)
	}
	value, err := cmd.Output()
	if err != nil {
		var result *exec.ExitError
		if errors.As(err, &result) && result.ExitCode() == 128 && string(value) == "true\n" {
			// Git reports bare status, then fails --show-toplevel because
			// a bare repository has no worktree.
			return "", nil
		}
		return "", fmt.Errorf("cannot inspect GIT_DIR for its effective worktree: %w", err)
	}
	bare, top, ok := strings.Cut(string(value), "\n")
	path, eol := strings.CutSuffix(top, "\n")
	if !ok || bare != "false" || !eol || !filepath.IsAbs(path) || strings.ContainsRune(path, '\x00') {
		return "", errors.New("GIT_DIR has an invalid effective worktree")
	}
	return filepath.Clean(path), nil
}

func (g *setupPathGuard) addRoot(path string) error {
	info, err := os.Stat(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	g.repositories = append(g.repositories, repositoryRoot{systemConfigDirectory(path), info})
	return nil
}

func (g setupPathGuard) checkPath(path string) error {
	path = systemConfigDirectory(path)
	for _, root := range g.repositories {
		rel, err := filepath.Rel(root.path, path)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
			return errSetupRepository
		}
	}
	return nil
}

func (g setupPathGuard) checkDirectory(dir *os.Root) error {
	if len(g.repositories) > 0 {
		info, err := dir.Stat(".")
		if err != nil {
			return err
		}
		for _, root := range g.repositories {
			if root.info != nil && os.SameFile(info, root.info) {
				return errSetupRepository
			}
		}
	}
	if _, err := dir.Lstat(".git"); err == nil {
		return errSetupRepository
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// A bare repository has no .git marker. HEAD, refs and objects identify
	// the repository directory without opening Git config or refs.
	for _, name := range []string{"HEAD", "refs", "objects"} {
		if _, err := dir.Lstat(name); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
	}
	return errSetupRepository
}

func managedConfigDirectories(path string) []string {
	dir := filepath.Dir(path)
	vendorHome := filepath.Dir(dir)
	name := filepath.Base(path)
	if name == "crosshall.rules" && filepath.Base(dir) == "rules" && validConfigHome(vendorHome) {
		return []string{vendorHome, dir}
	}
	if filepath.Base(vendorHome) == ".gemini" &&
		(name == "hooks.json" && filepath.Base(dir) == "config" ||
			name == "settings.json" && filepath.Base(dir) == "antigravity-cli") {
		return []string{vendorHome, dir}
	}
	return nil
}

// openSetupState pins the state root and applies the same ancestry guard as
// vendor files. It may create missing state components, each through its
// already verified parent.
func openSetupState(path string, create bool) (root *os.Root, err error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return nil, errors.New("adapter setup state requires an absolute, clean directory path")
	}
	guard, err := newSetupPathGuard()
	if err != nil {
		return nil, err
	}
	if err := guard.checkPath(path); err != nil {
		return nil, err
	}
	dir, err := os.OpenRoot("/")
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			dir.Close()
		}
	}()
	parts := strings.Split(strings.TrimPrefix(systemConfigDirectory(path), "/"), "/")
	for i, name := range parts {
		if err := guard.checkDirectory(dir); err != nil {
			return nil, err
		}
		final := i == len(parts)-1
		child, err := openConfigChild(dir, name, final)
		if create && errors.Is(err, os.ErrNotExist) {
			if err := dir.Mkdir(name, 0700); err != nil && !errors.Is(err, os.ErrExist) {
				return nil, err
			}
			child, err = openConfigChild(dir, name, final)
		}
		if err != nil {
			return nil, err
		}
		dir.Close()
		dir = child
	}
	if err := guard.checkDirectory(dir); err != nil {
		return nil, err
	}
	info, err := dir.Stat(".")
	if err != nil {
		return nil, err
	}
	if info.Mode() != os.ModeDir|0700 {
		return nil, errors.New("adapter setup state directory must have mode 0700")
	}
	return dir, nil
}
