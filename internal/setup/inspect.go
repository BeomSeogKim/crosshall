package setup

import (
	"github.com/BeomSeogKim/crosshall/internal/hook/manifests"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/BeomSeogKim/crosshall/internal/hook"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

// Inspect reports user-level configuration without launching a vendor.
func Inspect(stateRoot string) []InstallStatus {
	executable, err := os.Executable()
	if err == nil {
		executable, err = filepath.EvalSymlinks(executable)
	}
	if err != nil {
		executable = ""
	}
	return inspect(stateRoot, executable)
}

// inspect is Inspect for hook commands that run the given executable; an
// empty one leaves the installation unknown.
func inspect(stateRoot, executable string) []InstallStatus {
	j, _, journalErr := readJournal(stateRoot)
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
		if executable != "" && journalErr == nil && j.Pending == nil {
			inspectInstall(&status, spec, executable, stateRoot, j.Files[spec.Adapter])
			status.markOutdated(spec, j.Builds[spec.Adapter].Revision)
		}
		statuses = append(statuses, status)
	}
	return statuses
}

// SetReminderEvidence adds applicable imported results to install diagnostics.
func (s *InstallStatus) SetReminderEvidence(evidence []store.DoctorEvidence) {
	s.Reminder = releaseReminderStatus(s.Adapter, s.Version, s.platform, s.revision, evidence)
}

func inspectInstall(status *InstallStatus, spec hook.InstallSpec, executable, stateRoot string, record ownedFile) {
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
	_ = inspectHookEntries(status, spec, executable, stateRoot, path, record, current, doc)
}

func inspectHookEntries(status *InstallStatus, spec hook.InstallSpec, executable, stateRoot, path string, record ownedFile, current fileSnapshot, doc *configNode) error {
	recognized := 0
	// A shared managed server's hooks inherit its environment, so their
	// state routing is read back and reported.
	if caps, _ := manifests.CapabilitiesOf(spec.Adapter); caps.SharedDaemon {
		routes, err := codexHookRoutes(doc, executable, record.Entries)
		if err != nil {
			return err
		}
		recognized = len(routes)
		status.CodexHookStateRouting = codexHookStateRouting(routes)
	}
	if spec.Adapter == "agy" {
		recognized = recognizableAgyHooks(doc, spec)
	}
	expected, err := hookEntries(spec, executable, stateRoot)
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
	status.Installed, status.Drifted = installed, max(drifted, recognized-installed)
	if record.Path != "" {
		if path != record.Path || current.DirIdentity != record.Latest.Dir {
			status.Drifted += len(record.Entries)
		} else {
			_, removed, _, err := removeEntries(current.Bytes, record.Entries)
			if err != nil {
				return err
			}
			status.recorded = len(removed)
			status.Drifted += len(record.Entries) - len(removed)
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
	if spec.Adapter == "agy" {
		return recognizableAgyHooks(doc, spec), nil
	}
	count := 0
	err := visitConfigHooks(doc, func(event string, handler *configNode) {
		if recognizableHook(handler, spec, event) {
			count++
		}
	})
	return count, err
}

func recognizableAgyHooks(doc *configNode, spec hook.InstallSpec) int {
	if doc == nil {
		return 0
	}
	count := 0
	for _, group := range doc.fields {
		if group.key == agyHookGroup {
			count++
			continue
		}
		list := group.value.field("SessionStart")
		if list == nil || list.kind != '[' {
			continue
		}
		for _, handler := range list.items {
			if recognizableHook(handler, spec, "SessionStart") {
				count++
			}
		}
	}
	return count
}

func visitConfigHooks(doc *configNode, visit func(string, *configNode)) error {
	if doc == nil {
		return nil
	}
	if doc.kind != '{' {
		return errInvalidConfig
	}
	hooks := doc.field("hooks")
	if hooks == nil {
		return nil
	}
	if hooks.kind != '{' {
		return errInvalidConfig
	}
	for _, event := range hooks.fields {
		if event.value.kind != '[' {
			return errInvalidConfig
		}
		for _, group := range event.value.items {
			handlers, err := configHookHandlers(group)
			if err != nil {
				return err
			}
			for _, handler := range handlers {
				visit(event.key, handler)
			}
		}
	}
	return nil
}

func recognizableHook(handler *configNode, spec hook.InstallSpec, event string) bool {
	kind, _ := handler.field("type").stringValue()
	command, _ := handler.field("command").stringValue()
	if (kind != "command" && !(spec.Adapter == "agy" && handler.field("type") == nil)) ||
		command == "" || !eventName.MatchString(event) {
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
		if spec.Adapter == "codex" {
			route, ok := parseCodexHookCommand(command, event)
			return ok && (route.root != "" || isCrosshallExecutable(route.executable))
		}
		var ok bool
		command, ok = strings.CutSuffix(strings.TrimSpace(command), " hook "+spec.Adapter+" "+event)
		if !ok {
			return false
		}
		if spec.Adapter == "agy" {
			state, executable, ok := splitHookStatePin(command)
			command, literal := literalShellExecutable(executable)
			return ok && literal && (state != "" || isCrosshallExecutable(command))
		}
		command, ok = literalShellExecutable(command)
		if !ok {
			return false
		}
	}
	return isCrosshallExecutable(command)
}

func literalShellExecutable(command string) (string, bool) {
	if len(command) >= 2 && strings.HasPrefix(command, "'") && strings.HasSuffix(command, "'") {
		path := strings.ReplaceAll(command[1:len(command)-1], "'\"'\"'", "'")
		return path, shellQuote(path) == command
	}
	if len(command) >= 2 && command[0] == '"' && command[len(command)-1] == '"' {
		path := command[1 : len(command)-1]
		// Only recognize a literal shell word, without expansion or escapes.
		return path, !strings.ContainsAny(path, "\"\\$`")
	}
	return command, !strings.ContainsAny(command, "'\"\\`$&;|<>()*?[]{}!#~") &&
		strings.IndexFunc(command, unicode.IsSpace) < 0
}

func isCrosshallExecutable(command string) bool {
	return command == "crosshall" || (filepath.IsAbs(command) && filepath.Base(command) == "crosshall")
}

// InstalledConfigDir is the configuration directory setup installed an
// adapter's hooks into for this state root, as its journal records it (for
// Codex, the CODEX_HOME); empty when none is recorded.
func InstalledConfigDir(stateRoot, adapter string) string {
	j, _, err := readJournal(stateRoot)
	if err != nil {
		return ""
	}
	if f, ok := j.Files[adapter]; ok && filepath.IsAbs(f.Path) {
		return filepath.Dir(f.Path)
	}
	return ""
}
