package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/BeomSeogKim/crosshall/internal/hook"
	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

type installTarget struct {
	Spec       hook.InstallSpec
	StateRoot  string
	Path       string
	PathSource string
	Current    fileSnapshot
	Build      build
	Wanted     []Entry
	Evidence   []store.DoctorEvidence
}

func verifyHome(home string) error {
	location, err := openConfigLocation(filepath.Join(home, ".crosshall-version-probe"))
	if err != nil {
		return err
	}
	defer location.dir.Close()
	if location.missing != "" {
		return errors.New("HOME must name an existing user directory")
	}
	return nil
}

func (s Service) targets(ctx context.Context, j journal) ([]installTarget, error) {
	var targets []installTarget
	for _, spec := range hook.InstallSpecs() {
		target, err := s.preflightInstall(spec, j)
		if errors.Is(err, exec.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		targets = append(targets, target)
	}
	s.completeTargets(ctx, j, targets)
	return targets, nil
}

func (s Service) preflightInstall(spec hook.InstallSpec, j journal) (installTarget, error) {
	target := installTarget{Spec: spec, StateRoot: s.StateRoot}
	b, err := currentBuild(spec)
	if err != nil {
		return target, fmt.Errorf("%s executable: %w", spec.Adapter, err)
	}
	location, err := resolveConfigPath(spec)
	target.Path, target.PathSource = location.Path, location.Source
	if err != nil {
		return target, err
	}
	if err := verifyHome(location.Home); err != nil {
		return target, err
	}
	old := j.Files[spec.Adapter]
	if old.Path != "" && old.Path != target.Path {
		return target, fmt.Errorf("%s has journal entries in another configuration home (%q); resolved %q", spec.Adapter, old.Path, target.Path)
	}
	for _, retained := range j.Retained {
		if retained.Adapter == spec.Adapter && retained.Path != target.Path {
			return target, fmt.Errorf("%s has retained hooks to inspect in another configuration home (%q); resolved %q", spec.Adapter, retained.Path, target.Path)
		}
	}
	current, err := readConfig(target.Path)
	if err != nil {
		return target, fmt.Errorf("%s configuration: %w", spec.Adapter, err)
	}
	if old.Latest.Dir != "" && old.Latest.Dir != current.DirIdentity {
		return target, fmt.Errorf("%s configuration directory has changed since installation", spec.Adapter)
	}
	if current.Exists && len(current.Bytes) == 0 {
		return target, fmt.Errorf("%s configuration is empty; expected a JSON object", spec.Adapter)
	}
	wanted, err := hookEntries(spec, s.Executable, s.StateRoot)
	if err != nil {
		return target, err
	}
	matches, err := entryMatches(current.Bytes, wanted)
	if err != nil {
		return target, fmt.Errorf("%s configuration: %w", spec.Adapter, err)
	}
	if spec.Adapter == "agy" {
		doc, err := configDocument(current.Bytes)
		if err != nil {
			return target, err
		}
		if count := recognizableAgyHooks(doc, spec); count > matches[0] {
			return target, errors.New("Antigravity configuration has an existing crosshall hook group or conflicting hook; inspect it before setup")
		}
	}
	target.Current, target.Build, target.Wanted = current, b, wanted
	return target, nil
}

func (s Service) completeTargets(ctx context.Context, j journal, targets []installTarget) {
	for i := range targets {
		target := &targets[i]
		previous := j.Builds[target.Spec.Adapter]
		version := previous.Version
		previous.Version = ""
		if target.Build != previous || version == unknown {
			version = detectVersion(ctx, target.Spec, target.Build)
		}
		target.Build.Version = version
	}
	s.attachEvidence(ctx, targets)
}

func (s Service) attachEvidence(ctx context.Context, targets []installTarget) {
	if len(targets) == 0 {
		return
	}
	state, err := spool.OpenServerRoot(s.StateRoot)
	if err != nil {
		return
	}
	defer state.Close()
	names := make([]string, 0, len(targets))
	for _, target := range targets {
		names = append(names, target.Spec.Adapter)
	}
	snapshot, err := store.InspectDoctorStore(ctx, spool.ServerFiles{Root: state}, names, "")
	if err != nil {
		return
	}
	for i := range targets {
		target := &targets[i]
		for _, e := range snapshot.Adapters[target.Spec.Adapter].Evidence {
			if e.Version == target.Build.Version && e.Platform == target.Build.Platform && e.ManifestRev == target.Build.Revision {
				target.Evidence = append(target.Evidence, e)
			}
		}
	}
}

func makeInstall(j journal, targets []installTarget, read, send bool) (operation, error) {
	op := operation{Action: "setup", Builds: make(map[string]build)}
	for _, target := range targets {
		adapter := target.Spec.Adapter
		op.Adapters = append(op.Adapters, adapter)
		op.Builds[adapter] = target.Build
		record, known := j.Files[adapter]
		base := target.Current.Bytes
		var restore [][]span
		var remove []Entry
		if adapter == "codex" {
			var migrate bool
			var err error
			base, migrate, err = codexInstallBase(target, record)
			if err != nil {
				return operation{}, err
			}
			if migrate {
				restore, remove = record.Undo, record.Entries
			}
		}
		if len(restore) > 0 {
			record.Undo, record.Entries = nil, nil
		} else if known && adapter != "agy" {
			entries, err := checkHookRelocation(target, record.Entries)
			if err != nil {
				return operation{}, fmt.Errorf("%s configuration: %w", adapter, err)
			}
			record.Entries = entries
		}
		wanted := target.Wanted
		if adapter == "claude" && (read || send) {
			wanted = slices.Clone(wanted)
			if read {
				wanted = append(wanted, claudeRules(false)...)
			}
			if send {
				wanted = append(wanted, claudeRules(true)...)
			}
		}
		updated, added, err := addEntries(base, wanted)
		if err != nil {
			return operation{}, fmt.Errorf("%s configuration: %w", adapter, err)
		}
		if len(updated) > maxConfigBytes {
			return operation{}, fmt.Errorf("%s configuration would exceed the %d-byte limit", adapter, maxConfigBytes)
		}
		before := stamp(target.Current)
		if known && before != record.Latest {
			// An owner edit invalidates exact restoration even when setup
			// adds no hooks and only refreshes the cached vendor build.
			record.Undo = nil
		}
		if len(added) == 0 && len(restore) == 0 {
			e := edit{Adapter: adapter, Path: target.Path, Before: before, After: before}
			if known {
				e.Record = &record
			}
			op.Edits = append(op.Edits, e)
			continue
		}
		after := fingerprint{Exists: true, Hash: sum(updated), Mode: 0600, Dir: before.Dir}
		if before.Exists {
			after.Mode = before.Mode
		}
		if !known {
			record = ownedFile{Path: target.Path, Original: before}
		}
		if !known || len(record.Undo) > 0 || len(restore) > 0 {
			inverse, err := insertionSpans(base, updated)
			if err != nil {
				return operation{}, err
			}
			record.Undo = append(slices.Clone(record.Undo), inverse)
		}
		record.Entries = append(slices.Clone(record.Entries), remainingEntries(added, record.Entries)...)
		record.Latest = after
		op.Edits = append(op.Edits, edit{
			Adapter: adapter, Path: target.Path, Before: before, After: after,
			Add: added, Remove: remove, Restore: restore, Record: &record,
		})
	}
	return op, nil
}

// Setup never adds a relocated executable beside one it already installed.
// An owner can uninstall or manually resolve any retained old commands.
func checkHookRelocation(target installTarget, owned []Entry) ([]Entry, error) {
	if len(owned) == 0 || len(target.Wanted) == 0 {
		return owned, nil
	}
	spec := target.Spec
	executable, err := recordedHookExecutable(spec, target.Wanted[0])
	if err != nil {
		return nil, err
	}
	var doc *configNode
	var remaining []Entry
	for _, entry := range owned {
		if entry.Event == "" {
			remaining = append(remaining, entry)
			continue
		}
		old, err := recordedHookExecutable(spec, entry)
		if err != nil {
			return nil, err
		}
		if executable == old {
			remaining = append(remaining, entry)
			continue
		}
		if doc == nil {
			doc, err = configDocument(target.Current.Bytes)
			if err != nil {
				return nil, err
			}
		}
		if doc == nil {
			continue
		}
		list, missing, err := findConfigSlot(doc, entry)
		if err != nil {
			return nil, err
		}
		if len(missing) != 0 {
			continue
		}
		for _, group := range list.items {
			handlers, err := configHookHandlers(group)
			if err != nil {
				return nil, err
			}
			for _, handler := range handlers {
				command, _ := handler.field("command").stringValue()
				if spec.Adapter == "codex" {
					_, command, _ = splitHookStatePin(strings.TrimSpace(command))
					if strings.HasPrefix(command, shellQuote(old)) {
						return nil, errors.New("hooks still use a previous Crosshall executable; run crosshall uninstall and resolve any retained hooks before setup")
					}
				} else if command == old || (spec.Encoding == "shell" && strings.HasPrefix(strings.TrimSpace(command), old)) {
					return nil, errors.New("hooks still use a previous Crosshall executable; run crosshall uninstall and resolve any retained hooks before setup")
				}
			}
		}
	}
	return remaining, nil
}

func recordedHookExecutable(spec hook.InstallSpec, entry Entry) (string, error) {
	if spec.Adapter == "codex" {
		route, err := recordedCodexRoute(entry)
		return route.executable, err
	}
	parsed, err := parseConfigEntry(entry)
	if err != nil || parsed.handler == nil {
		return "", errInvalidEntry
	}
	command, _ := parsed.handler.field("command").stringValue()
	if spec.Encoding == "shell" {
		executable, ok := strings.CutSuffix(command, " hook "+spec.Adapter+" "+entry.Event)
		if !ok || executable == "" {
			return "", errInvalidEntry
		}
		return executable, nil
	}
	return command, nil
}

func makeUninstall(j journal) operation {
	op := operation{Action: "uninstall", Retained: slices.Clone(j.Retained)}
	for _, spec := range hook.InstallSpecs() {
		addRemoval(&op, spec, j)
	}
	for adapter, record := range j.Files {
		if !slices.Contains(op.Adapters, adapter) {
			op.Retained = append(op.Retained, retainedFile{adapter, record.Path, "unknown adapter in install journal"})
		}
	}
	return op
}

func addRemoval(op *operation, spec hook.InstallSpec, j journal) {
	op.Adapters = append(op.Adapters, spec.Adapter)
	record, ok := j.Files[spec.Adapter]
	if !ok {
		return
	}
	e, err := planRemoval(spec, record)
	if err != nil {
		op.Retained = append(op.Retained, retainedFile{spec.Adapter, record.Path, err.Error()})
		return
	}
	op.Edits = append(op.Edits, e)
}

func planRemoval(spec hook.InstallSpec, record ownedFile) (edit, error) {
	home, path, err := configPath(spec)
	if err != nil {
		return edit{}, err
	}
	if path != record.Path {
		return edit{}, fmt.Errorf("journal belongs to another configuration home (%q); resolved %q", record.Path, path)
	}
	if err := verifyHome(home); err != nil {
		return edit{}, err
	}
	current, err := readConfig(path)
	if err != nil {
		return edit{}, err
	}
	if current.DirIdentity != record.Latest.Dir {
		return edit{}, errors.New("configuration directory has changed since installation")
	}
	e := edit{Adapter: spec.Adapter, Path: path, Before: stamp(current), After: stamp(current)}
	if len(record.Undo) > 0 && e.Before == record.Latest {
		e.Remove, e.Restore = record.Entries, record.Undo
		e.After.Exists, e.After.Mode = record.Original.Exists, record.Original.Mode
		e.After.Hash, e.After.File = record.Original.Hash, ""
		return e, nil
	}
	updated, kept, err := removeEntries(current.Bytes, record.Entries)
	if err != nil {
		return edit{}, err
	}
	e.Kept = kept
	e.Remove = remainingEntries(record.Entries, kept)
	if len(e.Remove) != 0 {
		e.After.Hash, e.After.File = sum(updated), ""
	}
	if len(kept) > 0 {
		record.Entries, record.Undo = kept, nil
		e.Record = &record
	}
	return e, nil
}

func remainingEntries(entries, kept []Entry) []Entry {
	var removed []Entry
	for _, e := range entries {
		if !slices.ContainsFunc(kept, func(k Entry) bool {
			return k.Event == e.Event && k.Group == e.Group && string(k.Value) == string(e.Value)
		}) {
			removed = append(removed, e)
		}
	}
	return removed
}

func claudeRules(send bool) []Entry {
	verbs := []string{"inbox", "who", "waiting", "status", "read", "log"}
	if send {
		verbs = []string{"note", "ask", "reply"}
	}
	rules := make([]Entry, 0, len(verbs))
	for _, verb := range verbs {
		suffix := ""
		if send || verb == "read" || verb == "log" {
			suffix = ":*"
		}
		raw, _ := json.Marshal("Bash(crosshall " + verb + suffix + ")")
		rules = append(rules, Entry{Value: raw})
	}
	return rules
}

func verifyTargets(op operation) error {
	for _, e := range op.Edits {
		if !slices.Contains(op.Adapters, e.Adapter) {
			return errors.New("install journal includes an unexpected adapter")
		}
		found := false
		for _, spec := range hook.InstallSpecs() {
			if spec.Adapter != e.Adapter {
				continue
			}
			home, selected, err := configPath(spec)
			if err != nil {
				return err
			}
			if selected != e.Path {
				return fmt.Errorf("%s pending plan belongs to another configuration home (%q); resolved %q", spec.Adapter, e.Path, selected)
			}
			if err := verifyHome(home); err != nil {
				return err
			}
			found = true
			break
		}
		if !found {
			return errors.New("install journal includes an unknown adapter")
		}
	}
	return nil
}
