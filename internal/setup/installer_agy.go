package setup

import (
	"bytes"
	"path/filepath"
	"slices"
	"strings"

	"github.com/BeomSeogKim/crosshall/internal/hook"
)

// agyInstaller installs Antigravity hooks, pinned to the state root, in a
// named group of its config home's hooks file, and its command rules in the
// CLI's settings. Setup owns the configuration directories it creates.
type agyInstaller struct{ baseInstaller }

func (agyInstaller) defaultHome(home string) string { return filepath.Join(home, ".gemini", "config") }

func (agyInstaller) rulePath(hooks configTarget) string {
	return filepath.Join(hooks.Home, ".gemini", "antigravity-cli", "settings.json")
}

// skillPath is $HOME/.gemini/config/skills, which agy reads first.
func (agyInstaller) skillPath(hooks configTarget) string { return vendorSkillPath(hooks) }

func (agyInstaller) statePinned() bool { return true }

func (agyInstaller) hookGroup() string { return agyHookGroup }

// recognizeHooks counts the crosshall group and any installed event's
// handler of another group that runs crosshall.
func (agyInstaller) recognizeHooks(doc *configNode, spec hook.InstallSpec) int {
	if doc == nil {
		return 0
	}
	count := 0
	for _, group := range doc.fields {
		if group.key == agyHookGroup {
			count++
			continue
		}
		for _, event := range spec.Events {
			list := group.value.field(event)
			if list == nil || list.kind != '[' {
				continue
			}
			for _, handler := range list.items {
				if recognizableHook(handler, spec, event) {
					count++
				}
			}
		}
	}
	return count
}

func (agyInstaller) handlerTypeOptional() bool { return true }

func (agyInstaller) recognizeShellCommand(command, event string, spec hook.InstallSpec) bool {
	command, ok := strings.CutSuffix(strings.TrimSpace(command), " hook "+spec.Adapter+" "+event)
	if !ok {
		return false
	}
	state, executable, ok := splitHookStatePin(command)
	command, literal := literalShellExecutable(executable)
	return ok && literal && (state != "" || isCrosshallExecutable(command))
}

// upgrade replaces the crosshall group an earlier revision installed. A
// stale group the owner already removed is dropped from the record too: it
// is in the remove list, where applying skips an entry the file lacks.
func (i agyInstaller) upgrade(target installTarget, record ownedFile, known bool) ([]byte, [][]span, []Entry, []Entry, error) {
	stale := slices.DeleteFunc(slices.Clone(record.Entries), func(e Entry) bool {
		return e.Group == "" || len(target.Wanted) == 0 || !sameRecordedExecutable(i, e, target.Wanted[0], target.Spec) ||
			slices.ContainsFunc(target.Wanted, func(w Entry) bool { return bytes.Equal(w.Value, e.Value) })
	})
	if !known || len(stale) == 0 {
		return target.Current.Bytes, nil, nil, nil, nil
	}
	base, restore, remove, err := replaceOwned(target, record, stale, "Antigravity hooks")
	if err != nil || len(restore) > 0 {
		return base, restore, remove, nil, err
	}
	doc, err := configDocument(target.Current.Bytes)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	for _, e := range stale {
		if _, _, matches, err := locateConfigEntry(doc, e); err == nil && matches.full == 0 && matches.related == 0 {
			remove = append(remove, e)
		}
	}
	return base, restore, remove, nil, nil
}

func (agyInstaller) checksRelocation() bool { return false }
func (agyInstaller) ownsConfigDirs() bool   { return true }

func (agyInstaller) memberRules(adapter, executable string) ([]Entry, []byte, error) {
	entries, err := jsonMemberRules(adapter, executable, "command(%s %s)")
	return entries, nil, err
}

func (agyInstaller) hasMemberRules() bool { return true }
