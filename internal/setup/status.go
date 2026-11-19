package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/BeomSeogKim/crosshall/internal/hook"
	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

// AdapterStatus reports on-disk hooks, ownership and policy independently.
type AdapterStatus struct {
	Adapter      string         `json:"adapter"`
	Name         string         `json:"name"`
	HookAdapter  bool           `json:"hook_adapter"`
	CLI          string         `json:"cli"`
	Path         string         `json:"path,omitempty"`
	PathSource   string         `json:"path_source,omitempty"`
	RecordedPath string         `json:"recorded_path,omitempty"`
	PathError    string         `json:"path_error,omitempty"`
	Install      string         `json:"install"`
	Owned        bool           `json:"owned"`
	Policy       string         `json:"policy"`
	Footprint    string         `json:"footprint"`
	Pending      *PendingStatus `json:"pending,omitempty"`
}

// PendingStatus describes the scope already approved in an interrupted plan.
type PendingStatus struct {
	Action   string      `json:"action"`
	Adapters []string    `json:"adapters"`
	Rules    ClaudeRules `json:"rules"`
}

// Status reads adapter diagnostics without creating state or running a vendor.
func (s Service) Status(ctx context.Context) ([]AdapterStatus, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	j, journalKnown, policies, absentStore := inspectStatusState(ctx, s.StateRoot)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	executable := s.Executable
	if executable == "" {
		executable, _ = os.Executable()
		executable, _ = filepath.EvalSymlinks(executable)
	}
	specs := hook.InstallSpecs()
	statuses := make([]AdapterStatus, 0, len(specs)+1)
	for _, spec := range specs {
		status := AdapterStatus{
			Adapter: spec.Adapter, Name: statusAdapterName(spec.Adapter), HookAdapter: true,
			CLI: statusCLI(spec), Install: unknown, Policy: unknown, Footprint: unknown,
		}
		if reported, ok := policies[spec.Adapter]; ok && reported.Policy.Epoch != 0 {
			status.Policy = "off"
			if reported.Policy.Enabled {
				status.Policy = "on"
			}
		} else if ok || absentStore {
			status.Policy = "default"
		}
		record, owned := j.Files[spec.Adapter]
		_, hasBuild := j.Builds[spec.Adapter]
		status.Owned = journalKnown && owned
		// A pending operation reserves the setup journal for all adapters.
		// Only the rows in its original scope expose the approved plan.
		if j.Pending != nil && slices.Contains(j.Pending.Adapters, spec.Adapter) {
			status.Pending = &PendingStatus{
				Action: j.Pending.Action, Adapters: slices.Clone(j.Pending.Adapters), Rules: pendingClaudeRules(*j.Pending),
			}
		}
		recorded := record.Path
		retained := false
		for _, f := range j.Retained {
			if f.Adapter == spec.Adapter {
				retained = true
				if recorded == "" {
					recorded = f.Path
				}
			}
		}
		if recorded == "" && j.Pending != nil {
			for _, e := range j.Pending.Edits {
				if e.Adapter == spec.Adapter {
					recorded = e.Path
					break
				}
			}
		}
		inspectStatusInstall(&status, spec, executable, s.StateRoot, record)
		if journalKnown && recorded != status.Path {
			status.RecordedPath = recorded
		}
		if journalKnown && status.Install != unknown && status.Policy != unknown {
			status.Footprint = "none"
			if status.Install != "none" || owned || hasBuild || retained || j.Pending != nil || status.Policy != "default" {
				status.Footprint = "present"
			}
		}
		statuses = append(statuses, status)
	}
	statuses = append(statuses, AdapterStatus{
		Adapter: "antigravity", Name: "Antigravity", CLI: "unsupported",
		Install: "none", Policy: unknown, Footprint: "none",
	})
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return statuses, nil
}

func inspectStatusState(ctx context.Context, path string) (journal, bool, map[string]store.DoctorAdapterState, bool) {
	state, err := openSetupState(path, false)
	if errors.Is(err, os.ErrNotExist) {
		return emptyJournal(), true, nil, true
	}
	if err != nil {
		return journal{}, false, nil, false
	}
	defer state.Close()
	j, _, journalErr := readJournalAt(state)
	if journalErr != nil {
		j = journal{}
	}
	absent := true
	for _, name := range []string{"store.sqlite3", "store.sqlite3-wal", "store.sqlite3-shm", "store.sqlite3-journal"} {
		if _, err := state.Lstat(name); !errors.Is(err, os.ErrNotExist) {
			absent = false
			break
		}
	}
	if absent {
		return j, journalErr == nil, nil, true
	}
	snapshot, err := store.InspectDoctorStore(ctx, spool.ServerFiles{Root: state}, adapterNames(), "")
	if err != nil {
		return j, journalErr == nil, nil, false
	}
	return j, journalErr == nil, snapshot.Adapters, false
}

func statusAdapterName(adapter string) string {
	switch adapter {
	case "claude":
		return "Claude Code"
	case "codex":
		return "Codex"
	default:
		return adapter
	}
}

func statusCLI(spec hook.InstallSpec) string {
	if _, err := currentBuild(spec); err == nil {
		return "found"
	} else if errors.Is(err, exec.ErrNotFound) {
		return "missing"
	}
	return unknown
}

func inspectStatusInstall(status *AdapterStatus, spec hook.InstallSpec, executable, stateRoot string, record ownedFile) {
	target, err := resolveConfigPath(spec)
	if err == nil {
		status.Path, status.PathSource = target.Path, target.Source
		err = verifyHome(target.Home)
	}
	var current fileSnapshot
	if err == nil {
		current, err = readConfig(target.Path)
	}
	if err != nil {
		status.PathError = err.Error()
		return
	}
	if current.Exists && len(current.Bytes) == 0 {
		status.PathError = "configuration is empty; expected a JSON object"
		return
	}
	doc, err := configDocument(current.Bytes)
	if err != nil {
		status.PathError = err.Error()
		return
	}
	recognizable, err := recognizableHooks(doc, spec)
	if err != nil {
		status.PathError = err.Error()
		return
	}
	measured := InstallStatus{Intended: len(spec.Events)}
	if err := inspectHookEntries(&measured, spec, executable, stateRoot, target.Path, record, current, doc); err != nil {
		status.PathError = err.Error()
		return
	}
	switch {
	case measured.InstallState == "drifted" || recognizable > measured.Installed:
		status.Install = "drift"
	case measured.InstallState == "not_installed":
		status.Install = "none"
	default:
		status.Install = measured.InstallState
	}
}

func RenderStatus(w io.Writer, statuses []AdapterStatus, jsonOutput bool) error {
	if jsonOutput {
		return json.NewEncoder(w).Encode(struct {
			Version  int             `json:"v"`
			OK       bool            `json:"ok"`
			Kind     string          `json:"kind"`
			Adapters []AdapterStatus `json:"adapters"`
		}{Version: 1, OK: true, Kind: "setup status", Adapters: statuses})
	}
	var text strings.Builder
	var pending *PendingStatus
	for _, status := range statuses {
		if !status.HookAdapter {
			fmt.Fprintf(&text, "%s — session identity supported, hook adapter unavailable\n", status.Name)
			continue
		}
		fmt.Fprintf(&text, "%s — CLI %s, hooks %s, policy %s, footprint %s\n",
			status.Name, status.CLI, status.Install, status.Policy, status.Footprint)
		if status.Path != "" {
			fmt.Fprintf(&text, "  configuration %q (%s)\n", status.Path, status.PathSource)
		}
		if status.RecordedPath != "" {
			fmt.Fprintf(&text, "  recorded configuration %q\n", status.RecordedPath)
		}
		if status.Owned {
			fmt.Fprintln(&text, "  hook entries recorded in the installation journal")
		}
		if status.PathError != "" {
			fmt.Fprintln(&text, "  "+status.PathError)
		}
		if status.Pending != nil {
			pending = status.Pending
		}
	}
	if pending != nil {
		fmt.Fprintf(&text, "Interrupted %s for %s (Claude read rules %t, send rules %t)\n",
			pending.Action, strings.Join(pending.Adapters, ", "), pending.Rules.Read, pending.Rules.Send)
	}
	_, err := io.WriteString(w, text.String())
	return err
}
