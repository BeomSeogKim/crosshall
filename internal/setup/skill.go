package setup

import (
	_ "embed"
	"fmt"
	"path/filepath"
	"slices"

	"github.com/BeomSeogKim/crosshall/internal/hook"
	"github.com/BeomSeogKim/crosshall/internal/hook/manifests"
)

// skillContent is the crosshall skill every vendor gets, byte for byte.
//
//go:embed skill/SKILL.md
var skillContent []byte

const (
	skillMode           = 0644
	skillEditedReason   = "kept the crosshall skill because it was edited"
	skillReplacedReason = "kept the crosshall skill because it is not the file setup wrote"
)

// skillTarget is the vendor's crosshall skill file as setup found it.
type skillTarget struct {
	Location configTarget
	Current  fileSnapshot
	Skipped  *retainedFile
}

// vendorSkillPath places the skill in the vendor's own user skill directory,
// never in a directory another vendor also reads.
func vendorSkillPath(hooks configTarget) string {
	return filepath.Join(filepath.Dir(hooks.Path), "skills", "crosshall", "SKILL.md")
}

func resolveSkillPath(spec hook.InstallSpec) (configTarget, error) {
	target, err := resolveConfigPath(spec)
	if err != nil {
		return configTarget{}, err
	}
	path := installerFor(spec.Adapter).skillPath(target)
	if path == "" {
		return configTarget{}, fmt.Errorf("no skill path for adapter %s", spec.Adapter)
	}
	target.Path = path
	return target, nil
}

// preflightSkill reads the skill target. Nothing about the skill stops the
// hooks: a path setup cannot use becomes a retained line in the plan.
func preflightSkill(target *installTarget, j journal) {
	adapter := target.Spec.Adapter
	if installerFor(adapter).skillPath(configTarget{Path: target.Path}) == "" {
		return
	}
	skill := &skillTarget{}
	target.Skill = skill
	skip := func(err error) {
		skill.Skipped = &retainedFile{adapter, skill.Location.Path, "skipped the crosshall skill: " + err.Error()}
	}
	location, err := resolveSkillPath(target.Spec)
	skill.Location = location
	if err != nil {
		skip(err)
		return
	}
	if record := j.SkillFiles[adapter]; record.Path != "" && record.Path != location.Path {
		skip(fmt.Errorf("it belongs to another configuration home (%q)", record.Path))
		return
	}
	if skill.Current, err = readConfig(location.Path); err != nil {
		skip(err)
	}
}

// planSkillInstall writes the skill when the path is free, or replaces the
// file setup wrote when it is still the same file with the bytes setup
// recorded. A file the owner edited or recreated, one setup did not write,
// and one another vendor's setup claims are left as they are.
func planSkillInstall(adapter string, target skillTarget, j journal) (*edit, *retainedFile) {
	before := stamp(target.Current)
	want := sum(skillContent)
	path := target.Location.Path
	record, known := j.SkillFiles[adapter]
	switch {
	case skillClaimedByAnother(j, adapter, path):
		return nil, nil
	case known && before.Exists && !sameSkillFile(before, record.Latest):
		return releaseSkill(adapter, path, before), &retainedFile{adapter, path, skillReplacedReason}
	case before.Exists && before.Hash == want:
		// Already current, or an identical file setup did not write: no edit.
		return nil, nil
	case before.Exists && !known:
		return nil, &retainedFile{adapter, path, "left an existing crosshall skill that setup did not install"}
	case before.Exists && before.Hash != record.Latest.Hash:
		return nil, &retainedFile{adapter, path, skillEditedReason}
	}
	if !known {
		record = ownedFile{Path: path, Original: before}
	}
	after := fingerprint{Exists: true, Hash: want, Mode: skillMode, Dir: before.Dir}
	if before.Exists {
		after.Mode = before.Mode
	}
	record.Latest = after
	return &edit{Adapter: adapter, Skill: true, Path: path, Before: before, After: after,
		Content: string(skillContent), Record: &record}, nil
}

// sameSkillFile reports that the file and its directory are the ones setup
// recorded, not a copy someone recreated with the same bytes.
func sameSkillFile(now, recorded fingerprint) bool {
	return now.File == recorded.File && now.Dir == recorded.Dir
}

// releaseSkill leaves the file as it is and gives up setup's record of it.
func releaseSkill(adapter, path string, now fingerprint) *edit {
	return &edit{Adapter: adapter, Skill: true, Path: path, Before: now, After: now}
}

func skillClaimedByAnother(j journal, adapter, path string) bool {
	for other, record := range j.SkillFiles {
		if other != adapter && record.Path == path {
			return true
		}
	}
	return false
}

// skillConsumers names the other adapters whose recorded installation puts
// the skill at path: their own skill record, or the skill path of their
// recorded hooks file. It reads only the journal, never the environment.
// sameOp names adapters that one consent removes together with adapter.
func skillConsumers(j journal, adapter, path string, sameOp []string) []string {
	var consumers []string
	for _, spec := range hook.InstallSpecs() {
		other := spec.Adapter
		if other == adapter || slices.Contains(sameOp, other) {
			continue
		}
		hooks, recorded := j.Files[other]
		if j.SkillFiles[other].Path == path ||
			recorded && installerFor(other).skillPath(configTarget{Path: hooks.Path}) == path {
			consumers = append(consumers, other)
		}
	}
	return consumers
}

func addSkillRemoval(op *operation, spec hook.InstallSpec, record ownedFile, j journal, sameOp []string) {
	e, kept, err := planSkillRemoval(spec, record, j, sameOp)
	if err != nil {
		op.Retained = append(op.Retained, retainedFile{spec.Adapter, record.Path, err.Error()})
		return
	}
	if kept != nil {
		op.Retained = append(op.Retained, *kept)
	}
	op.Edits = append(op.Edits, e)
}

// planSkillRemoval deletes the skill only while it is the file setup wrote,
// with the bytes setup recorded, and no other recorded vendor shares it.
// Otherwise setup gives up its record of the file, or passes it to the
// vendor that still uses it.
func planSkillRemoval(spec hook.InstallSpec, record ownedFile, j journal, sameOp []string) (edit, *retainedFile, error) {
	location, err := resolveSkillPath(spec)
	if err != nil {
		return edit{}, nil, err
	}
	if location.Path != record.Path {
		return edit{}, nil, fmt.Errorf("journal belongs to another configuration home (%q); resolved %q", record.Path, location.Path)
	}
	if err := verifyHome(location.Home); err != nil {
		return edit{}, nil, err
	}
	current, err := readConfig(location.Path)
	if err != nil {
		return edit{}, nil, err
	}
	now := stamp(current)
	e := edit{Adapter: spec.Adapter, Skill: true, Path: location.Path, Before: now, After: now}
	kept := func(reason string) (edit, *retainedFile, error) {
		return e, &retainedFile{spec.Adapter, location.Path, reason}, nil
	}
	switch {
	case !current.Exists:
		e.CleanupDirs = record.Dirs
	case !sameSkillFile(now, record.Latest):
		return kept(skillReplacedReason)
	case now.Hash != record.Latest.Hash:
		return kept(skillEditedReason)
	default:
		if others := skillConsumers(j, spec.Adapter, location.Path, sameOp); len(others) != 0 {
			other := others[0]
			e.Record, e.Transfer = &record, other
			return kept("kept the crosshall skill: " + manifests.DisplayName(other) + " still uses it")
		}
		e.After = fingerprint{Dir: now.Dir}
		e.CleanupDirs = record.Dirs
	}
	return e, nil, nil
}

// skillOutcomes are the result lines for the skill edits of a plan.
func skillOutcomes(files []PlanFile) []string {
	var lines []string
	for _, f := range files {
		name := manifests.DisplayName(f.Adapter)
		switch f.Skill {
		case "install":
			lines = append(lines, fmt.Sprintf("installed the crosshall skill for %s (%s)", name, f.Path))
		case "update":
			lines = append(lines, fmt.Sprintf("updated the crosshall skill for %s (%s)", name, f.Path))
		case "remove":
			lines = append(lines, fmt.Sprintf("removed the crosshall skill for %s (%s)", name, f.Path))
		case "shared":
			lines = append(lines, fmt.Sprintf("crosshall skill shared with %s (%s)", f.SharedWith, f.Path))
		}
	}
	return lines
}
