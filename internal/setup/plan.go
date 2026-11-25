package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"path/filepath"
	"slices"

	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/hook"
	"github.com/BeomSeogKim/crosshall/internal/hook/manifests"
	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

type installTarget struct {
	Spec         hook.InstallSpec
	StateRoot    string
	Path         string
	PathSource   string
	Current      fileSnapshot
	Build        build
	Wanted       []Entry
	Evidence     []store.DoctorEvidence
	CommandRules *commandRuleTarget
	disclosure   installDisclosure
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
	if i := installerFor(spec.Adapter); i.hookGroup() != "" {
		doc, err := configDocument(current.Bytes)
		if err != nil {
			return target, err
		}
		// The group this journal records for this same executable is
		// setup's own, an earlier revision's included: the edit replaces it.
		owned := 0
		if record, ok := j.Files[spec.Adapter]; ok && len(wanted) > 0 {
			same := slices.DeleteFunc(slices.Clone(record.Entries), func(e Entry) bool {
				return !sameRecordedExecutable(i, e, wanted[0], spec)
			})
			if _, removed, _, err := removeEntries(current.Bytes, same); err == nil {
				owned = len(removed)
			}
		}
		if count := i.recognizeHooks(doc, spec); count > max(matches[0], owned) {
			return target, fmt.Errorf("%s configuration has an existing crosshall hook group or conflicting hook; inspect it before setup", manifests.DisplayName(spec.Adapter))
		}
	}
	target.Current, target.Build, target.Wanted = current, b, wanted
	if err := s.preflightCommandRules(&target, j); err != nil {
		return target, fmt.Errorf("%s command rules: %w", spec.Adapter, err)
	}
	for _, retained := range j.Retained {
		if retained.Adapter == spec.Adapter && retained.Path != target.Path &&
			(target.CommandRules == nil || retained.Path != target.CommandRules.Location.Path) {
			return target, fmt.Errorf("%s has retained settings to inspect in another configuration home (%q); resolved hooks to %q", spec.Adapter, retained.Path, target.Path)
		}
	}
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
	s.attachInstallDisclosure(ctx, targets)
	var snapshot store.DoctorSnapshot
	state, err := spool.OpenServerRoot(s.StateRoot)
	if err == nil {
		defer state.Close()
		files := spool.ServerFiles{Root: state}
		_, err = fs.Stat(files, "store.sqlite3")
		if err == nil {
			names := make([]string, 0, len(targets))
			for _, target := range targets {
				names = append(names, target.Spec.Adapter)
			}
			if snapshot, err = store.InspectDoctorStore(ctx, files, names, nil); err != nil {
				return
			}
		}
	}
	if errors.Is(err, fs.ErrNotExist) {
		// No store yet: list the built-in rows the dispatcher seeds into it,
		// the evidence the install question already answers from.
		snapshot = builtinEvidenceSnapshot()
	} else if err != nil {
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

func builtinEvidenceSnapshot() store.DoctorSnapshot {
	snapshot := store.DoctorSnapshot{Adapters: map[string]store.DoctorAdapterState{}}
	for _, row := range domain.BuiltinCapabilityEvidence {
		state := snapshot.Adapters[row.Vendor]
		state.Evidence = append(state.Evidence, store.DoctorEvidence{Version: row.Version, Platform: row.Platform,
			Mode: row.Mode, ManifestRev: row.ManifestRev, Capability: row.Capability, Level: "tested"})
		snapshot.Adapters[row.Vendor] = state
	}
	return snapshot
}

func makeInstall(j journal, targets []installTarget) (operation, error) {
	op := operation{Action: "setup", Builds: make(map[string]build)}
	for _, target := range targets {
		adapter := target.Spec.Adapter
		op.Adapters = append(op.Adapters, adapter)
		op.Builds[adapter] = target.Build
		record, known := j.Files[adapter]
		e, err := planJSONInstall(target, record, known, false)
		if err != nil {
			return operation{}, fmt.Errorf("%s configuration: %w", adapter, err)
		}
		op.Edits = append(op.Edits, e)
		if rules := target.CommandRules; rules != nil {
			if rules.Skipped != nil {
				op.Retained = append(op.Retained, *rules.Skipped)
				continue
			}
			record, known := j.RuleFiles[adapter]
			if rules.Text != "" {
				e, err = planTextRuleInstall(adapter, *rules, record, known)
			} else {
				extra := installTarget{Spec: target.Spec, Path: rules.Location.Path, Current: rules.Current, Wanted: rules.Entries}
				e, err = planJSONInstall(extra, record, known, true)
			}
			if err != nil {
				return operation{}, fmt.Errorf("%s command rules: %w", adapter, err)
			}
			op.Edits = append(op.Edits, e)
		}
	}
	return op, nil
}

func planJSONInstall(target installTarget, record ownedFile, known, rules bool) (edit, error) {
	adapter := target.Spec.Adapter
	base := target.Current.Bytes
	wanted := target.Wanted
	var restore [][]span
	var remove []Entry
	i := installerFor(adapter)
	if !rules {
		var keep []Entry
		var err error
		base, restore, remove, keep, err = i.upgrade(target, record, known)
		if err != nil {
			return edit{}, err
		}
		wanted = append(keep, wanted...)
	}
	if len(restore) > 0 {
		record.Undo, record.Entries, record.Created = nil, nil, nil
	} else if known && !rules {
		entries := record.Entries
		if i.checksRelocation() {
			var err error
			if entries, err = checkHookRelocation(target, record.Entries); err != nil {
				return edit{}, err
			}
		}
		record.Entries = remainingEntries(entries, remove)
	}
	if known && (i.inlineRules() || rules) {
		if err := checkCommandRuleRelocation(target, record.Entries); err != nil {
			return edit{}, err
		}
	}
	updated, added, err := addEntries(base, wanted)
	if err != nil {
		return edit{}, err
	}
	if len(updated) > maxConfigBytes {
		return edit{}, fmt.Errorf("configuration would exceed the %d-byte limit", maxConfigBytes)
	}
	before := stamp(target.Current)
	if known && before != record.Latest {
		// An owner edit invalidates exact restoration even when setup
		// adds no hooks and only refreshes the cached vendor build.
		record.Undo = nil
	}
	if len(added) == 0 && len(remove) == 0 {
		e := edit{Adapter: adapter, Rules: rules, Path: target.Path, Before: before, After: before}
		if known {
			e.Record = &record
		}
		return e, nil
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
			return edit{}, err
		}
		record.Undo = append(slices.Clone(record.Undo), inverse)
	}
	created, err := createdSlots(base, updated, added)
	if err != nil {
		return edit{}, err
	}
	record.Created = appendSlots(slices.Clone(record.Created), created)
	record.Entries = append(slices.Clone(record.Entries), remainingEntries(added, record.Entries)...)
	record.Latest = after
	return edit{
		Adapter: adapter, Rules: rules, Path: target.Path, Before: before, After: after,
		Add: added, Remove: remove, Restore: restore, Record: &record,
	}, nil
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
				if installerFor(spec.Adapter).runsExecutable(command, old, spec) {
					return nil, errors.New("hooks still use a previous Crosshall executable; run crosshall uninstall and resolve any retained hooks before setup")
				}
			}
		}
	}
	return remaining, nil
}

func recordedHookExecutable(spec hook.InstallSpec, entry Entry) (string, error) {
	return installerFor(spec.Adapter).recordedExecutable(entry, spec)
}

func makeUninstall(j journal) operation {
	op := operation{Action: "uninstall", Retained: slices.Clone(j.Retained)}
	for _, spec := range hook.InstallSpecs() {
		addRemoval(&op, spec, j)
	}
	for _, files := range []map[string]ownedFile{j.Files, j.RuleFiles} {
		for adapter, record := range files {
			if !slices.Contains(op.Adapters, adapter) {
				op.Retained = append(op.Retained, retainedFile{adapter, record.Path, "unknown adapter in install journal"})
			}
		}
	}
	return op
}

func addRemoval(op *operation, spec hook.InstallSpec, j journal) {
	op.Adapters = append(op.Adapters, spec.Adapter)
	if record, ok := j.RuleFiles[spec.Adapter]; ok {
		addFileRemoval(op, spec, record, true)
	}
	if record, ok := j.Files[spec.Adapter]; ok {
		addFileRemoval(op, spec, record, false)
	}
}

func addFileRemoval(op *operation, spec hook.InstallSpec, record ownedFile, rules bool) {
	e, err := planRemoval(spec, record, rules)
	if err != nil {
		op.Retained = append(op.Retained, retainedFile{spec.Adapter, record.Path, err.Error()})
		return
	}
	op.Edits = append(op.Edits, e)
}

func planRemoval(spec hook.InstallSpec, record ownedFile, rules bool) (edit, error) {
	location, err := resolveEditPath(spec, rules)
	if err != nil {
		return edit{}, err
	}
	path := location.Path
	if path != record.Path {
		return edit{}, fmt.Errorf("journal belongs to another configuration home (%q); resolved %q", record.Path, path)
	}
	if err := verifyHome(location.Home); err != nil {
		return edit{}, err
	}
	current, err := readConfig(path)
	if err != nil {
		return edit{}, err
	}
	if current.DirIdentity != record.Latest.Dir {
		return edit{}, errors.New("configuration directory has changed since installation")
	}
	e := edit{Adapter: spec.Adapter, Rules: rules, Path: path, Before: stamp(current), After: stamp(current)}
	if rules && installerFor(spec.Adapter).textRules() {
		return planTextRuleRemoval(e, current, record)
	}
	if len(record.Undo) > 0 && e.Before == record.Latest {
		e.Remove, e.Restore = record.Entries, record.Undo
		e.After.Exists, e.After.Mode = record.Original.Exists, record.Original.Mode
		e.After.Hash, e.After.File = record.Original.Hash, ""
		if e.ownsDirectories() && !e.After.Exists {
			e.CleanupDirs = record.Dirs
		}
		return e, nil
	}
	// Removing $defaults while leaving the list would discard Claude's
	// built-in rules. The byte-exact path above restores an absent list,
	// and so does removing a list setup created once only $defaults is left.
	claims := slices.DeleteFunc(slices.Clone(record.Entries), isClaudeAutoModeDefaults)
	updated, removed, kept, err := removeEntries(current.Bytes, claims)
	if err != nil {
		return edit{}, err
	}
	if defaults := slices.IndexFunc(record.Entries, isClaudeAutoModeDefaults); defaults >= 0 && onlyDefaultsInCreatedList(updated, record.Created) {
		claims = append(claims, record.Entries[defaults])
		if updated, removed, kept, err = removeEntries(current.Bytes, claims); err != nil {
			return edit{}, err
		}
	}
	if len(record.Created) != 0 {
		pruned, err := pruneSlots(updated, record.Created)
		if err != nil {
			return edit{}, err
		}
		if !bytes.Equal(pruned, updated) {
			updated, e.Prune = pruned, record.Created
		}
	}
	e.Kept = kept
	e.Remove = removed
	if len(e.Remove) != 0 || len(e.Prune) != 0 {
		e.After.Hash, e.After.File = sum(updated), ""
	}
	if len(kept) > 0 {
		record.Entries, record.Undo = kept, nil
		// A slot this edit prunes is no longer setup's: a later uninstall
		// must not remove a list of the same name the owner adds.
		doc, err := configDocument(updated)
		if err != nil {
			return edit{}, err
		}
		record.Created = slices.DeleteFunc(slices.Clone(record.Created), func(s []string) bool { return configAt(doc, s) == nil })
		e.Record = &record
	}
	return e, nil
}

func onlyDefaultsInCreatedList(raw []byte, created [][]string) bool {
	slot := slotPath(Entry{AutoMode: true})
	if !slices.ContainsFunc(created, func(s []string) bool { return slices.Equal(s, slot) }) {
		return false
	}
	doc, err := configDocument(raw)
	if err != nil {
		return false
	}
	list := configAt(doc, slot)
	if list == nil || list.kind != '[' || len(list.items) != 1 {
		return false
	}
	value, ok := list.items[0].stringValue()
	return ok && `"`+value+`"` == claudeDefaultRules
}

func remainingEntries(entries, kept []Entry) []Entry {
	var removed []Entry
	for _, e := range entries {
		if !slices.ContainsFunc(kept, func(k Entry) bool {
			return k.Event == e.Event && k.Group == e.Group && k.AutoMode == e.AutoMode && string(k.Value) == string(e.Value)
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
			location, err := resolveEditPath(spec, e.Rules)
			if err != nil {
				return err
			}
			if location.Path != e.Path {
				return fmt.Errorf("%s pending plan belongs to another configuration home (%q); resolved %q", spec.Adapter, e.Path, location.Path)
			}
			if err := verifyHome(location.Home); err != nil {
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

// sameRecordedExecutable reports that two hook entries run the same
// executable, with the same state pin.
func sameRecordedExecutable(i installer, a, b Entry, spec hook.InstallSpec) bool {
	x, errX := i.recordedExecutable(a, spec)
	y, errY := i.recordedExecutable(b, spec)
	return errX == nil && errY == nil && x == y
}
