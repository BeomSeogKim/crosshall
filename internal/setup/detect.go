// Package setup plans and applies user-level adapter installation.
package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/hook"
)

const unknown = "unknown"

type build struct {
	Executable string `json:"executable"`
	Version    string `json:"version"`
	Revision   string `json:"manifest_rev"`
	Platform   string `json:"platform"`
	Identity   string `json:"file_id"`
	Size       int64  `json:"size"`
	ModifiedNS int64  `json:"modified_ns"`
}

var versionPattern = regexp.MustCompile(`^v?([0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?)$`)
var eventName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*$`)

func currentBuild(spec hook.InstallSpec) (build, error) {
	file, err := exec.LookPath(spec.Adapter)
	if err != nil {
		return build{}, err
	}
	if !filepath.IsAbs(file) {
		return build{}, errors.New("vendor executable must resolve to an absolute path")
	}
	file, err = filepath.EvalSymlinks(file)
	if err != nil {
		return build{}, err
	}
	info, err := os.Stat(file)
	if err != nil {
		return build{}, err
	}
	return build{Executable: file, Revision: spec.Revision, Platform: runtime.GOOS + "-" + runtime.GOARCH,
		Identity: configIdentity(info), Size: info.Size(), ModifiedNS: info.ModTime().UnixNano()}, nil
}

func detectVersion(ctx context.Context, spec hook.InstallSpec, b build) string {
	// A version command may initialize caches. Keep its writable directories
	// out of the selected home even when the caller's TMPDIR points there.
	scratch, err := os.MkdirTemp("/tmp", "crosshall-version-")
	if err != nil {
		return unknown
	}
	defer os.RemoveAll(scratch)
	config := filepath.Join(scratch, "config")
	if err := os.Mkdir(config, 0700); err != nil {
		return unknown
	}
	probe, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	cmd := exec.CommandContext(probe, b.Executable, "--version")
	cmd.Dir = scratch
	cmd.Env = []string{"HOME=" + scratch, spec.HomeEnv + "=" + config, "TMPDIR=" + scratch, "PATH=" + os.Getenv("PATH")}
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 100 * time.Millisecond
	var output bytes.Buffer
	cmd.Stdout = &limitedOutput{buffer: &output}
	if err := cmd.Run(); err != nil {
		return unknown
	}
	for _, field := range strings.Fields(output.String()) {
		if match := versionPattern.FindStringSubmatch(field); match != nil {
			return match[1]
		}
	}
	return unknown
}

type limitedOutput struct{ buffer *bytes.Buffer }

func (w *limitedOutput) Write(p []byte) (int, error) {
	if w.buffer.Len()+len(p) > 4096 {
		return 0, errors.New("version output exceeds limit")
	}
	return w.buffer.Write(p)
}

type configTarget struct {
	Home   string
	Path   string
	Source string
}

func configPath(spec hook.InstallSpec) (home, config string, err error) {
	target, err := resolveConfigPath(spec)
	return target.Home, target.Path, err
}

// resolveConfigPath selects a path without opening vendor settings. Callers
// verify the home and open the target through the filesystem guards.
func resolveConfigPath(spec hook.InstallSpec) (configTarget, error) {
	home := os.Getenv("HOME")
	if !validConfigHome(home) {
		return configTarget{}, errors.New("HOME must explicitly name an absolute configuration home")
	}
	config, overridden := os.LookupEnv(spec.HomeEnv)
	source := "override"
	if overridden {
		if !validConfigHome(config) {
			return configTarget{}, fmt.Errorf("%s must explicitly name an absolute configuration home", spec.HomeEnv)
		}
	} else {
		source = "default"
		switch spec.Adapter {
		case "claude":
			config = filepath.Join(home, ".claude")
		case "codex":
			config = filepath.Join(home, ".codex")
		default:
			return configTarget{}, fmt.Errorf("no default configuration home for adapter %s", spec.Adapter)
		}
	}
	if spec.File != filepath.Base(spec.File) || spec.File == "." || spec.File == ".." {
		return configTarget{}, errors.New("invalid adapter install filename")
	}
	return configTarget{Home: home, Path: filepath.Join(config, spec.File), Source: source}, nil
}

func validConfigHome(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && path != "/" && !strings.ContainsAny(path, "\r\n\x00")
}

func hookEntries(spec hook.InstallSpec, executable, stateRoot string) ([]Entry, error) {
	if !filepath.IsAbs(executable) || strings.ContainsAny(executable, "\r\n\x00") || spec.Timeout <= 0 {
		return nil, errors.New("invalid hook executable or timeout")
	}
	if spec.Adapter == "codex" && !validHookStateRoot(stateRoot) {
		return nil, errors.New("the Codex hook state root must be an absolute path")
	}
	entries := make([]Entry, 0, len(spec.Events))
	seen := make(map[string]bool)
	for _, event := range spec.Events {
		if !eventName.MatchString(event) || seen[event] {
			return nil, errors.New("invalid or duplicate install event")
		}
		seen[event] = true
		type handler struct {
			Type    string   `json:"type"`
			Command string   `json:"command"`
			Args    []string `json:"args,omitempty"`
			Timeout int      `json:"timeout"`
		}
		args := []string{"hook", spec.Adapter, event}
		h := handler{Type: "command", Timeout: spec.Timeout}
		switch spec.Encoding {
		case "argv":
			h.Command, h.Args = executable, args
		case "shell":
			h.Command = shellQuote(executable) + " " + strings.Join(args, " ")
			if spec.Adapter == "codex" {
				h.Command = codexStateAssignment + shellQuote(stateRoot) + " " + h.Command
			}
		default:
			return nil, fmt.Errorf("unsupported install encoding %q", spec.Encoding)
		}
		raw, err := json.Marshal(struct {
			Matcher string    `json:"matcher,omitempty"`
			Hooks   []handler `json:"hooks"`
		}{spec.Matchers[event], []handler{h}})
		if err != nil {
			return nil, err
		}
		entries = append(entries, Entry{Event: event, Value: raw})
	}
	return entries, nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

type InstallStatus struct {
	Adapter               string         `json:"adapter"`
	Executable            string         `json:"executable"`
	Version               string         `json:"version"`
	InstallState          string         `json:"install_state"`
	Trust                 string         `json:"trust"`
	Intended              int            `json:"intended"`
	Installed             int            `json:"installed"`
	Drifted               int            `json:"drifted"`
	CodexHookStateRouting string         `json:"codex_hook_state_routing,omitempty"`
	Reminder              reminderStatus `json:"reminder"`
	platform              string
	revision              string
}
