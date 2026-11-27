package setup

import (
	_ "embed"
	"fmt"
	"path/filepath"

	"github.com/BeomSeogKim/crosshall/internal/hook"
	"github.com/BeomSeogKim/crosshall/internal/hook/manifests"
)

// skillContent is the crosshall skill every vendor gets, byte for byte.
//
//go:embed skill/SKILL.md
var skillContent []byte

const (
	skillMode         = 0644
	skillEditedReason = "kept the crosshall skill because it was edited"
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
// file setup wrote when its bytes are still the ones setup recorded. A file
// the owner edited, or one setup did not write, is left as it is.
func planSkillInstall(adapter string, target skillTarget, record ownedFile, known bool) (*edit, *retainedFile) {
	before := stamp(target.Current)
	want := sum(skillContent)
	path := target.Location.Path
	switch {
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

func addSkillRemoval(op *operation, spec hook.InstallSpec, record ownedFile) {
	e, kept, err := planSkillRemoval(spec, record)
	if err != nil {
		op.Retained = append(op.Retained, retainedFile{spec.Adapter, record.Path, err.Error()})
		return
	}
	if kept != nil {
		op.Retained = append(op.Retained, *kept)
	}
	op.Edits = append(op.Edits, e)
}

// planSkillRemoval deletes the skill only while its bytes are the ones setup
// recorded. Either way setup gives up its record of the file.
func planSkillRemoval(spec hook.InstallSpec, record ownedFile) (edit, *retainedFile, error) {
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
	switch {
	case !current.Exists:
		e.CleanupDirs = record.Dirs
	case now.Hash == record.Latest.Hash:
		e.After = fingerprint{Dir: now.Dir}
		e.CleanupDirs = record.Dirs
	default:
		return e, &retainedFile{spec.Adapter, location.Path, skillEditedReason}, nil
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
		}
	}
	return lines
}
