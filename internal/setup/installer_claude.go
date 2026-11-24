package setup

import "path/filepath"

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
