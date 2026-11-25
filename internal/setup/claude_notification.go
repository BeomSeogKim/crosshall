package setup

import (
	"bytes"
	"errors"
	"slices"
)

// claudeNotificationUpgrade prepares an install over claude-2 hooks, whose
// notification hook matched permission prompts only. When the file is as
// setup left it, the install starts again from the original bytes (and
// restores them on uninstall); after an owner edit, it removes only the old
// hook. Either way the current hooks are added afterwards.
func claudeNotificationUpgrade(target installTarget, record ownedFile) ([]byte, [][]span, []Entry, error) {
	raw := target.Current.Bytes
	index := slices.IndexFunc(target.Wanted, func(e Entry) bool { return e.Event == "Notification" })
	if index < 0 {
		return raw, nil, nil, nil
	}
	wanted := target.Wanted[index]
	legacy := wanted
	legacy.Value = bytes.Replace(wanted.Value, []byte(`"matcher":"permission_prompt|idle_prompt"`), []byte(`"matcher":"permission_prompt"`), 1)
	if bytes.Equal(legacy.Value, wanted.Value) || !slices.ContainsFunc(record.Entries, func(e Entry) bool {
		return e.Event == legacy.Event && bytes.Equal(e.Value, legacy.Value)
	}) {
		return raw, nil, nil, nil
	}
	return replaceOwned(target, record, []Entry{legacy}, "Claude notification hooks")
}

// replaceOwned prepares an install over entries an earlier revision of
// setup wrote. When the file is as setup left it, the install starts again
// from the original bytes (and restores them on uninstall); after an owner
// edit, it removes only the stale entries.
func replaceOwned(target installTarget, record ownedFile, stale []Entry, what string) ([]byte, [][]span, []Entry, error) {
	raw := target.Current.Bytes
	if stamp(target.Current) == record.Latest && len(record.Undo) > 0 {
		original, err := undoInsertions(raw, record.Undo)
		if err != nil || (record.Original.Exists && sum(original) != record.Original.Hash) ||
			(!record.Original.Exists && len(original) != 0) {
			return nil, nil, nil, errors.New("could not restore configuration before upgrading " + what)
		}
		return original, record.Undo, record.Entries, nil
	}
	raw, removed, _, err := removeEntries(raw, stale)
	return raw, nil, removed, err
}
