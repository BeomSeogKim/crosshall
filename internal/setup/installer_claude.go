package setup

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
)

// claudeInstaller installs Claude Code hooks and command rules in one
// settings file.
type claudeInstaller struct{ baseInstaller }

func (claudeInstaller) defaultHome(home string) string { return filepath.Join(home, ".claude") }

func (claudeInstaller) rulePath(hooks configTarget) string { return hooks.Path }

// upgrade widens an earlier revision's Notification matcher in place, and
// keeps the auto-mode defaults ahead of the wanted entries when it does.
func (claudeInstaller) upgrade(target installTarget, record ownedFile, known bool) ([]byte, [][]span, []Entry, []Entry, error) {
	if !known {
		return target.Current.Bytes, nil, nil, nil, nil
	}
	base, restore, remove, err := claudeNotificationUpgrade(target, record)
	if err != nil || len(restore) == 0 {
		return base, restore, remove, nil, err
	}
	return base, restore, remove, keepFirst(record, isClaudeAutoModeDefaults), nil
}

func (claudeInstaller) memberRules(adapter, executable string) ([]Entry, []byte, error) {
	if strings.ContainsRune(executable, '*') {
		return nil, nil, errors.New("Claude command permissions cannot represent an executable path containing '*'")
	}
	entries, err := jsonMemberRules(adapter, executable, "Bash(%s %s:*)")
	return entries, nil, err
}

// Claude Code's permissions live in its settings file beside the hooks.
func (claudeInstaller) inlineRules() bool { return true }

// addInlineRules adds the member permissions and the auto-mode rules,
// including Claude Code's defaults when the settings hold no auto-mode list.
func (claudeInstaller) addInlineRules(target *installTarget, executable string) error {
	entries, _, err := memberCommandRules(target.Spec.Adapter, executable)
	if err != nil {
		return err
	}
	doc, err := configDocument(target.Current.Bytes)
	if err != nil {
		return err
	}
	defaults := true
	if doc != nil {
		_, missing, err := findConfigSlot(doc, Entry{AutoMode: true})
		if err != nil {
			return err
		}
		defaults = len(missing) > 0
	}
	target.Wanted = append(target.Wanted, entries...)
	target.Wanted = append(target.Wanted, claudeAutoModeEntries(executable, defaults)...)
	return nil
}

// optionalRules are the send and receive rules the owner turns on.
func (claudeInstaller) optionalRules() []Entry {
	return slices.Concat(claudeRules(false), claudeRules(true))
}

func (claudeInstaller) reminderMeasurement() reminderEvidence {
	return reminderEvidence{
		Version: "2.1.327", Platform: "darwin-arm64", Mode: "print", ManifestRev: "claude-1", Level: "unavailable",
		Reason: "The read process did not receive the originating tool-call ID in conformance.",
	}
}

func (claudeInstaller) hasMemberRules() bool { return true }
