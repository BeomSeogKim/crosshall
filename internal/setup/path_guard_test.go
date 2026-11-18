package setup

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestConfigFileRefusesExplicitGitWorktree(t *testing.T) {
	t.Setenv("GIT_DIR", "")
	t.Setenv("GIT_COMMON_DIR", "")
	worktree := t.TempDir()
	t.Setenv("GIT_WORK_TREE", worktree)
	if _, err := readConfig(filepath.Join(worktree, "settings.json")); err == nil {
		t.Fatal("configuration inside GIT_WORK_TREE was accepted")
	}
	if _, err := readConfig(filepath.Join(t.TempDir(), "settings.json")); err != nil {
		t.Fatalf("unrelated configuration was refused: %v", err)
	}
}

func TestConfigFileRefusesGitDirConfiguredWorktree(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("Git is needed to build the fixture")
	}
	for _, tc := range []string{"configured worktree", "invocation directory", "bare repository"} {
		t.Run(tc, func(t *testing.T) {
			root := t.TempDir()
			gitDir, worktree := filepath.Join(root, "metadata"), filepath.Join(root, "dotfiles")
			if err := os.Mkdir(worktree, 0700); err != nil {
				t.Fatal(err)
			}
			run := func(args ...string) {
				t.Helper()
				cmd := exec.Command(git, args...)
				cmd.Dir = root
				cmd.Env = []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null"}
				if output, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git %v: %v\n%s", args, err, output)
				}
			}
			run("init", "--bare", "--template=", gitDir)
			if tc != "bare repository" {
				run("--git-dir", gitDir, "config", "core.bare", "false")
			}
			if tc == "configured worktree" {
				run("--git-dir", gitDir, "config", "core.worktree", "../dotfiles")
				t.Chdir(root)
			} else {
				t.Chdir(worktree)
			}
			t.Setenv("GIT_DIR", gitDir)
			t.Setenv("GIT_COMMON_DIR", "")
			t.Setenv("GIT_WORK_TREE", "")
			if _, err := readConfig(filepath.Join(gitDir, "settings.json")); !errors.Is(err, errSetupRepository) {
				t.Fatalf("Git metadata directory was not refused: %v", err)
			}
			_, err := readConfig(filepath.Join(worktree, "settings.json"))
			if tc == "bare repository" {
				if err != nil {
					t.Fatalf("bare Git metadata claimed invocation directory: %v", err)
				}
			} else if !errors.Is(err, errSetupRepository) {
				t.Fatalf("markerless Git worktree was not refused: %v", err)
			}
			if _, err := readConfig(filepath.Join(root, "unrelated", "settings.json")); err != nil {
				t.Fatalf("unrelated configuration was refused: %v", err)
			}
		})
	}
}

func TestConfigFileRefusesBareGitDirectory(t *testing.T) {
	repo := t.TempDir()
	for _, name := range []string{"objects", "refs"} {
		if err := os.Mkdir(filepath.Join(repo, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "HEAD"), []byte("ref: refs/heads/main\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readConfig(filepath.Join(repo, "settings.json")); err == nil {
		t.Fatal("configuration inside bare Git metadata was accepted")
	}
}

func TestInstallStateRefusesUnsafeAncestry(t *testing.T) {
	for _, tc := range []string{"repository", "symlink"} {
		t.Run(tc, func(t *testing.T) {
			root := t.TempDir()
			parent := root
			if tc == "repository" {
				if err := os.Mkdir(filepath.Join(root, ".git"), 0700); err != nil {
					t.Fatal(err)
				}
			} else {
				parent = filepath.Join(t.TempDir(), "linked")
				if err := os.Symlink(root, parent); err != nil {
					t.Fatal(err)
				}
			}
			statePath := filepath.Join(parent, "nested", "state")
			service := Service{StateRoot: statePath}
			if _, _, err := readJournal(statePath); err == nil {
				t.Error("journal read accepted an unsafe state path")
			}
			state, lock, err := service.lockState()
			if err == nil {
				lock.Close()
				state.Close()
				t.Error("journal lock accepted an unsafe state path")
			}
			if _, err := os.Lstat(filepath.Join(root, "nested")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("unsafe state path left directories behind: %v", err)
			}
		})
	}
}
