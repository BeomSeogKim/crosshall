package setup

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/BeomSeogKim/crosshall/internal/hook"
)

// installer is an adapter bundle's part of setup: what differs between the
// vendors' configuration homes, files and owner review. Setup keeps the
// preview, consent, path checks, journal, atomic edits and uninstall
// ownership. baseInstaller holds the shared behavior an installer embeds.
type installer interface {
	// defaultHome is the configuration home when the adapter's home
	// variable is unset.
	defaultHome(home string) string
	// rulePath places member command rules, given the hooks file target.
	rulePath(hooks configTarget) string
	// trustHint asks the owner to review installed hooks in the vendor, or
	// is empty when the vendor runs them without review.
	trustHint() string
	// sandboxNote warns that installed command rules run outside the
	// vendor's sandbox, or is empty.
	sandboxNote() string

	// statePinned prefixes a shell hook command with the state root, for a
	// vendor whose hooks do not inherit the owner's environment.
	statePinned() bool
	// hookGroup names the top-level group that holds the vendor's hooks as
	// {event: [handler]}; empty means matcher lists under "hooks".
	hookGroup() string
	// recognizeHooks counts Crosshall hooks in a named-group hooks file,
	// including other groups' crosshall commands.
	recognizeHooks(doc *configNode, spec hook.InstallSpec) int
	// handlerTypeOptional accepts a hook handler without a "type".
	handlerTypeOptional() bool
	// recognizeShellCommand recognizes an older or edited shell hook command
	// for event.
	recognizeShellCommand(command, event string, spec hook.InstallSpec) bool
	// recordedExecutable is the executable an installed entry runs.
	recordedExecutable(entry Entry, spec hook.InstallSpec) (string, error)
	// runsExecutable reports that an installed handler command runs old.
	runsExecutable(command, old string, spec hook.InstallSpec) bool
	// checksRelocation makes setup refuse to add hooks for a new executable
	// beside hooks it installed for another.
	checksRelocation() bool
	// ownsConfigDirs lets setup create, and uninstall remove, the vendor's
	// configuration directories it needed.
	ownsConfigDirs() bool
	// upgrade turns an earlier revision's installed hooks into the current
	// ones in the same journaled edit (known: setup recorded this file). It
	// returns the base to add to, the insertions to restore, the recorded
	// entries to remove and entries to keep ahead of the wanted ones.
	upgrade(target installTarget, record ownedFile, known bool) (base []byte, restore [][]span, remove, keep []Entry, err error)
	// legacyHook reports a recorded hook entry of an earlier layout, which a
	// resumed setup installs as approved and then asks the owner to upgrade.
	legacyHook(entry Entry) bool

	// hasMemberRules: setup installs member command permissions for the
	// vendor. A hooks-only adapter has none.
	hasMemberRules() bool
	// memberRules are the routine member command permissions for an
	// executable: entries of a JSON rules file, or a whole text rules file.
	memberRules(adapter, executable string) (entries []Entry, text []byte, err error)
	// inlineRules keeps member command rules in the hooks file;
	// addInlineRules adds them to the wanted hook entries, and optionalRules
	// are the owner's optional rule sets there that relocation leaves alone.
	inlineRules() bool
	addInlineRules(target *installTarget, executable string) error
	optionalRules() []Entry
	// skipRules leaves the rules file to the owner when setup cannot own
	// it (owned: setup recorded the file).
	skipRules(executable string, location configTarget, owned bool) (*retainedFile, error)
	// textRules owns a text rules file by its rule text, not JSON entries.
	textRules() bool
	// rulesChanged refuses an apply whose rules file changed after planning.
	rulesChanged(path string) error

	// reminderMeasurement is the release's own reminder measurement for the
	// vendor, which doctor and setup show beside imported evidence; zero
	// when the release measured none.
	reminderMeasurement() reminderEvidence
}

// hookHandler is one hook command as the vendors' hooks files hold it.
type hookHandler struct {
	Type    string   `json:"type"`
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
	Timeout int      `json:"timeout"`
}

func installerOf(spec hook.InstallSpec) (installer, bool) {
	i, ok := installers[spec.Adapter]
	return i, ok
}

// installerFor is the adapter's installer, or the shared behavior for an
// adapter that registers none.
func installerFor(adapter string) installer {
	if i, ok := installers[adapter]; ok {
		return i
	}
	return unregisteredInstaller{}
}

// unregisteredInstaller has no configuration home of its own.
type unregisteredInstaller struct{ baseInstaller }

func (unregisteredInstaller) defaultHome(string) string          { return "" }
func (unregisteredInstaller) rulePath(hooks configTarget) string { return hooks.Path }

// layoutHookEntry places one event's handler as a matcher list entry.
func layoutHookEntry(i installer, event, matcher string, h hookHandler) (Entry, error) {
	raw, err := json.Marshal(struct {
		Matcher string        `json:"matcher,omitempty"`
		Hooks   []hookHandler `json:"hooks"`
	}{matcher, []hookHandler{h}})
	if err != nil {
		return Entry{}, err
	}
	return Entry{Event: event, Value: raw}, nil
}

// baseInstaller is the shared behavior: hooks in matcher lists of the
// "hooks" object, no state pin, a literal shell executable, relocation
// checks and no revision upgrade.
type baseInstaller struct{}

func (baseInstaller) trustHint() string   { return "" }
func (baseInstaller) sandboxNote() string { return "" }
func (baseInstaller) statePinned() bool   { return false }

func (baseInstaller) hookGroup() string                                { return "" }
func (baseInstaller) recognizeHooks(*configNode, hook.InstallSpec) int { return 0 }
func (baseInstaller) handlerTypeOptional() bool                        { return false }

func (baseInstaller) recognizeShellCommand(command, event string, spec hook.InstallSpec) bool {
	command, ok := strings.CutSuffix(strings.TrimSpace(command), " hook "+spec.Adapter+" "+event)
	if !ok {
		return false
	}
	command, ok = literalShellExecutable(command)
	return ok && isCrosshallExecutable(command)
}

func (baseInstaller) recordedExecutable(entry Entry, spec hook.InstallSpec) (string, error) {
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

func (baseInstaller) runsExecutable(command, old string, spec hook.InstallSpec) bool {
	return command == old || (spec.Encoding == "shell" && strings.HasPrefix(strings.TrimSpace(command), old))
}

func (baseInstaller) checksRelocation() bool { return true }
func (baseInstaller) ownsConfigDirs() bool   { return false }

func (baseInstaller) upgrade(target installTarget, _ ownedFile, _ bool) ([]byte, [][]span, []Entry, []Entry, error) {
	return target.Current.Bytes, nil, nil, nil, nil
}

func (baseInstaller) legacyHook(Entry) bool { return false }

func (baseInstaller) memberRules(adapter, _ string) ([]Entry, []byte, error) {
	return nil, nil, fmt.Errorf("member command permissions are unsupported for adapter %q", adapter)
}

func (baseInstaller) hasMemberRules() bool { return false }

func (baseInstaller) inlineRules() bool                           { return false }
func (baseInstaller) addInlineRules(*installTarget, string) error { return nil }
func (baseInstaller) optionalRules() []Entry                      { return nil }

func (baseInstaller) skipRules(string, configTarget, bool) (*retainedFile, error) { return nil, nil }

func (baseInstaller) reminderMeasurement() reminderEvidence { return reminderEvidence{} }

func (baseInstaller) textRules() bool           { return false }
func (baseInstaller) rulesChanged(string) error { return nil }

// keepFirst returns the recorded entry that matches keep, if any.
func keepFirst(record ownedFile, keep func(Entry) bool) []Entry {
	if index := slices.IndexFunc(record.Entries, keep); index >= 0 {
		return []Entry{record.Entries[index]}
	}
	return nil
}
