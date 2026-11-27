package setup

import (
	"bytes"
	"errors"
	"fmt"
	"slices"

	"github.com/BeomSeogKim/crosshall/internal/hook"
)

type commandRuleTarget struct {
	Location configTarget
	Current  fileSnapshot
	Entries  []Entry
	Text     string
	Skipped  *retainedFile
}

func (s Service) preflightCommandRules(target *installTarget, j journal) error {
	adapter := target.Spec.Adapter
	i := installerFor(adapter)
	if !i.hasMemberRules() {
		return nil
	}
	if i.inlineRules() {
		return i.addInlineRules(target, s.Executable)
	}
	location, err := resolveCommandRulePath(target.Spec)
	if err != nil {
		return err
	}
	if err := verifyHome(location.Home); err != nil {
		return err
	}
	record := j.RuleFiles[adapter]
	if record.Path != "" && record.Path != location.Path {
		return fmt.Errorf("command rules belong to another configuration home (%q); resolved %q", record.Path, location.Path)
	}
	rules := commandRuleTarget{Location: location}
	if rules.Skipped, err = i.skipRules(s.Executable, location, record.Path != ""); err != nil {
		return err
	}
	if rules.Skipped != nil {
		target.CommandRules = &rules
		return nil
	}
	entries, text, err := memberCommandRules(adapter, s.Executable)
	if err != nil {
		return err
	}
	current, err := readConfig(location.Path)
	if err != nil {
		return err
	}
	if record.Latest.Dir != "" && record.Latest.Dir != current.DirIdentity {
		return errors.New("command rules directory has changed since installation")
	}
	if text == nil && current.Exists && len(current.Bytes) == 0 {
		return errors.New("command rules configuration is empty; expected a JSON object")
	}
	rules.Current, rules.Entries, rules.Text = current, entries, string(text)
	target.CommandRules = &rules
	return nil
}

func planTextRuleInstall(adapter string, target commandRuleTarget, record ownedFile, known bool) (edit, error) {
	before := stamp(target.Current)
	e := edit{Adapter: adapter, Rules: true, Path: target.Location.Path, Before: before, After: before}
	if known && before.Exists {
		if before.File != record.Latest.File {
			return edit{}, errors.New("managed command rules file was replaced; review it before setup")
		}
		remaining, found := removeRuleText(target.Current.Bytes, record.RuleText)
		if !found {
			return edit{}, errors.New("managed command rule was edited; review it before setup")
		}
		if record.RuleText == target.Text {
			e.Record = &record
			return e, nil
		}
		if len(remaining) != 0 {
			return edit{}, errors.New("managed command rules share an edited file; review it before relocating the rule")
		}
		e.RemoveText = record.RuleText
	} else if before.Exists && len(target.Current.Bytes) != 0 {
		if string(target.Current.Bytes) == target.Text {
			return e, nil
		}
		return edit{}, fmt.Errorf("a command rules file already exists at %q; preserve it before setup", e.Path)
	}
	if !known {
		record = ownedFile{Path: e.Path, Original: before}
	}
	e.After = fingerprint{Exists: true, Hash: sum([]byte(target.Text)), Mode: 0600, Dir: before.Dir}
	if before.Exists {
		e.After.Mode = before.Mode
	}
	record.RuleText, record.Latest = target.Text, e.After
	e.AddText, e.Record = target.Text, &record
	return e, nil
}

func planTextRuleRemoval(e edit, current fileSnapshot, record ownedFile) (edit, error) {
	if !current.Exists {
		e.CleanupDirs = record.Dirs
		return e, nil
	}
	if e.Before.File != record.Latest.File {
		return edit{}, errors.New("managed command rules file was replaced; inspect it manually")
	}
	remaining, removed := removeRuleText(current.Bytes, record.RuleText)
	if !removed {
		return edit{}, errors.New("managed command rule was edited; inspect it manually")
	}
	e.RemoveText = record.RuleText
	if len(remaining) == 0 && !record.Original.Exists {
		e.After = fingerprint{Dir: e.Before.Dir}
		e.CleanupDirs = record.Dirs
	} else {
		e.After.Hash, e.After.File = sum(remaining), ""
	}
	return e, nil
}

func checkCommandRuleRelocation(target installTarget, owned []Entry) error {
	wanted := slices.Concat(target.Wanted, installerFor(target.Spec.Adapter).optionalRules())
	for _, entry := range remainingEntries(owned, wanted) {
		if entry.Event != "" || isClaudeAutoModeDefaults(entry) {
			continue
		}
		matches, err := entryMatches(target.Current.Bytes, []Entry{entry})
		if err != nil {
			return err
		}
		if matches[0] != 0 {
			return errors.New("command rules still use a previous scope; run crosshall uninstall and resolve any retained rules before setup")
		}
	}
	return nil
}

func isClaudeAutoModeDefaults(entry Entry) bool {
	return entry.AutoMode && string(entry.Value) == claudeDefaultRules
}

func removeRuleText(raw []byte, rule string) ([]byte, bool) {
	needle := []byte(rule)
	at := bytes.Index(raw, needle)
	if len(needle) == 0 || needle[len(needle)-1] != '\n' || at < 0 ||
		(at > 0 && raw[at-1] != '\n') || bytes.Contains(raw[at+len(needle):], needle) {
		return nil, false
	}
	return slices.Concat(raw[:at], raw[at+len(needle):]), true
}

func resolveEditPath(spec hook.InstallSpec, e edit) (configTarget, error) {
	if e.Skill {
		return resolveSkillPath(spec)
	}
	if e.Rules {
		return resolveCommandRulePath(spec)
	}
	return resolveConfigPath(spec)
}
