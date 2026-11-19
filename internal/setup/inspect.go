package setup

import (
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/BeomSeogKim/crosshall/internal/hook"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

// Inspect reports user-level configuration without launching either vendor.
func Inspect(stateRoot string) []InstallStatus {
	j, _, journalErr := readJournal(stateRoot)
	executable, err := os.Executable()
	if err == nil {
		executable, err = filepath.EvalSymlinks(executable)
	}
	var statuses []InstallStatus
	for _, spec := range hook.InstallSpecs() {
		status := InstallStatus{
			Adapter: spec.Adapter, Executable: unknown, Version: unknown, InstallState: unknown,
			Trust: unknown, Intended: len(spec.Events),
		}
		if b, err := currentBuild(spec); err == nil {
			status.Executable = b.Executable
			status.platform, status.revision = b.Platform, b.Revision
			cached := j.Builds[spec.Adapter]
			b.Version = cached.Version
			if cached == b && cached.Version != "" {
				status.Version = cached.Version
			}
		}
		status.SetReminderEvidence(nil)
		if err == nil && journalErr == nil && j.Pending == nil {
			inspectInstall(&status, spec, executable, j.Files[spec.Adapter])
		}
		statuses = append(statuses, status)
	}
	return statuses
}

// SetReminderEvidence adds applicable imported results to install diagnostics.
func (s *InstallStatus) SetReminderEvidence(evidence []store.DoctorEvidence) {
	s.Reminder = releaseReminderStatus(s.Adapter, s.Version, s.platform, s.revision, evidence)
}

func inspectInstall(status *InstallStatus, spec hook.InstallSpec, executable string, record ownedFile) {
	home, path, err := configPath(spec)
	if err != nil || verifyHome(home) != nil {
		return
	}
	current, err := readConfig(path)
	if err != nil || (current.Exists && len(current.Bytes) == 0) {
		return
	}
	doc, err := configDocument(current.Bytes)
	if err != nil {
		return
	}
	_ = inspectHookEntries(status, spec, executable, path, record, current, doc)
}

func inspectHookEntries(status *InstallStatus, spec hook.InstallSpec, executable, path string, record ownedFile, current fileSnapshot, doc *configNode) error {
	expected, err := hookEntries(spec, executable)
	if err != nil {
		return err
	}
	var installed, drifted int
	for _, entry := range expected {
		_, _, matches, err := locateConfigEntry(doc, entry)
		if err != nil {
			return err
		}
		if matches.installed > 0 {
			installed++
		}
		if matches.installed > 1 {
			drifted++
		}
	}
	status.Installed, status.Drifted = installed, drifted
	if record.Path != "" {
		if path != record.Path || current.DirIdentity != record.Latest.Dir {
			status.Drifted += len(record.Entries)
		} else {
			_, kept, err := removeEntries(current.Bytes, record.Entries)
			if err != nil {
				return err
			}
			status.Drifted += len(kept)
		}
	}
	switch {
	case status.Drifted > 0:
		status.InstallState = "drifted"
	case status.Installed == status.Intended:
		status.InstallState = "installed"
	case status.Installed > 0:
		status.InstallState = "partial"
	default:
		status.InstallState = "not_installed"
	}
	if spec.Adapter == "codex" && status.Installed > 0 {
		status.Trust = "owner_review"
	}
	return nil
}

// recognizableHooks checks for older or edited Crosshall commands as well as
// exact installed entries. Its result gates the installer offer, not ownership.
func recognizableHooks(doc *configNode, spec hook.InstallSpec) (int, error) {
	if doc == nil {
		return 0, nil
	}
	if doc.kind != '{' {
		return 0, errInvalidConfig
	}
	hooks := doc.field("hooks")
	if hooks == nil {
		return 0, nil
	}
	if hooks.kind != '{' {
		return 0, errInvalidConfig
	}
	count := 0
	for _, event := range hooks.fields {
		if event.value.kind != '[' {
			return 0, errInvalidConfig
		}
		for _, group := range event.value.items {
			handlers, err := configHookHandlers(group)
			if err != nil {
				return 0, err
			}
			for _, handler := range handlers {
				if recognizableHook(handler, spec, event.key) {
					count++
				}
			}
		}
	}
	return count, nil
}

func recognizableHook(handler *configNode, spec hook.InstallSpec, event string) bool {
	kind, _ := handler.field("type").stringValue()
	command, _ := handler.field("command").stringValue()
	if kind != "command" || command == "" || !eventName.MatchString(event) {
		return false
	}
	if args := handler.field("args"); args != nil {
		if args.kind != '[' || len(args.items) != 3 {
			return false
		}
		for i, want := range []string{"hook", spec.Adapter, event} {
			if got, _ := args.items[i].stringValue(); got != want {
				return false
			}
		}
	} else {
		var ok bool
		command, ok = strings.CutSuffix(strings.TrimSpace(command), " hook "+spec.Adapter+" "+event)
		if !ok {
			return false
		}
		if len(command) >= 2 && strings.HasPrefix(command, "'") && strings.HasSuffix(command, "'") {
			path := strings.ReplaceAll(command[1:len(command)-1], "'\"'\"'", "'")
			if shellQuote(path) != command {
				return false
			}
			command = path
		} else if len(command) >= 2 && command[0] == '"' && command[len(command)-1] == '"' {
			command = command[1 : len(command)-1]
			// Only recognize a literal shell word, without expansion or escapes.
			if strings.ContainsAny(command, "\"\\$`") {
				return false
			}
		} else if strings.ContainsAny(command, "'\"\\`$&;|<>()*?[]{}!#~") ||
			strings.IndexFunc(command, unicode.IsSpace) >= 0 {
			return false
		}
	}
	return command == "crosshall" || (filepath.IsAbs(command) && filepath.Base(command) == "crosshall")
}
