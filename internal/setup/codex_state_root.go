package setup

import (
	"fmt"
	"path/filepath"
	"strings"
)

const hookStateAssignment = "CROSSHALL_STATE_DIR="

var errCodexLegacyHooks = &refusalWithNext{
	message: "Codex has legacy or edited Crosshall hooks. From the state directory that installed the hooks, run crosshall uninstall codex and review any retained hooks, or use a separate CODEX_HOME",
	next:    "CROSSHALL_STATE_DIR='<original-state-directory>' crosshall uninstall codex",
}

type codexHookRoute struct {
	event      string
	command    string
	executable string
	root       string
	ambiguous  bool
}

func validHookStateRoot(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && path != "/" && !strings.ContainsAny(path, "\r\n\x00")
}

func parseCodexHookCommand(command, event string) (codexHookRoute, bool) {
	route := codexHookRoute{event: event, command: command}
	words, ok := strings.CutSuffix(strings.TrimSpace(command), " hook codex "+event)
	if !ok || !eventName.MatchString(event) {
		return route, false
	}
	var unpinned string
	route.root, unpinned, ok = splitHookStatePin(words)
	if !ok {
		return route, false
	}
	route.executable, ok = literalShellExecutable(unpinned)
	return route, ok && route.executable != ""
}

func splitHookStatePin(command string) (root, remaining string, ok bool) {
	pin, present := strings.CutPrefix(command, hookStateAssignment)
	if !present {
		return "", command, true
	}
	var path strings.Builder
	for strings.HasPrefix(pin, "'") {
		part, tail, found := strings.Cut(pin[1:], "'")
		if !found {
			break
		}
		path.WriteString(part)
		if rest, escaped := strings.CutPrefix(tail, `"'"'`); escaped {
			path.WriteByte('\'')
			pin = "'" + rest
			continue
		}
		root = path.String()
		remaining, ok = strings.CutPrefix(tail, " ")
		return root, remaining, ok && validHookStateRoot(root)
	}
	return "", "", false
}

func codexHookRoutes(doc *configNode, executable string, owned []Entry) ([]codexHookRoute, error) {
	known := map[string]bool{executable: true}
	for _, entry := range owned {
		if route, err := recordedCodexRoute(entry); err == nil {
			known[route.executable] = true
		}
	}
	var routes []codexHookRoute
	err := visitConfigHooks(doc, func(event string, handler *configNode) {
		kind, _ := handler.field("type").stringValue()
		command, _ := handler.field("command").stringValue()
		if kind != "command" {
			return
		}
		route, ok := parseCodexHookCommand(command, event)
		pinSyntax := strings.HasPrefix(strings.TrimSpace(command), hookStateAssignment) &&
			strings.HasSuffix(strings.TrimSpace(command), " hook codex "+event)
		switch {
		case ok && (route.root != "" || known[route.executable] || isCrosshallExecutable(route.executable)):
			route.ambiguous = handler.field("args") != nil
			routes = append(routes, route)
		case pinSyntax:
			routes = append(routes, codexHookRoute{event: event, ambiguous: true})
		}
	})
	return routes, err
}

func codexHookStateRouting(routes []codexHookRoute) string {
	if len(routes) == 0 {
		return ""
	}
	root := routes[0].root
	mixed := false
	for _, route := range routes {
		if route.ambiguous {
			return "uncertain"
		}
		mixed = mixed || route.root != root
	}
	if mixed {
		return "mixed"
	}
	if root == "" {
		return "inherited"
	}
	return "pinned"
}

// codexInstallBase returns the original config and whether setup must replace
// its previously installed legacy hooks in one journaled operation.
func codexInstallBase(target installTarget, record ownedFile) ([]byte, bool, error) {
	doc, err := configDocument(target.Current.Bytes)
	if err != nil {
		return nil, false, err
	}
	wantCommands := make(map[string]string, len(target.Wanted))
	var executable string
	for _, entry := range target.Wanted {
		route, err := recordedCodexRoute(entry)
		if err != nil {
			return nil, false, err
		}
		executable = route.executable
		wantCommands[entry.Event] = route.command
	}
	routes, err := codexHookRoutes(doc, executable, record.Entries)
	if err != nil {
		return nil, false, err
	}
	var inherited int
	for _, route := range routes {
		if route.ambiguous {
			return nil, false, errCodexLegacyHooks
		}
		if route.root == "" {
			inherited++
			continue
		}
		if route.root != target.StateRoot {
			return nil, false, &refusalWithNext{
				message: fmt.Sprintf("Codex hooks in this CODEX_HOME already target Crosshall state root %q. Use that root or a separate CODEX_HOME", route.root),
				next:    hookStateAssignment + shellQuote(route.root) + " crosshall setup codex",
			}
		}
		if expected := wantCommands[route.event]; expected != "" && route.command != expected {
			return nil, false, errCodexLegacyHooks
		}
	}
	var legacy int
	for _, entry := range record.Entries {
		route, err := recordedCodexRoute(entry)
		if err != nil {
			return nil, false, err
		}
		if route.root == "" {
			legacy++
		}
	}
	if inherited == 0 && legacy == 0 {
		return target.Current.Bytes, false, nil
	}
	if legacy != inherited || record.Path != target.Path || stamp(target.Current) != record.Latest || len(record.Undo) == 0 {
		return nil, false, errCodexLegacyHooks
	}
	for _, entry := range record.Entries {
		_, _, matches, err := locateConfigEntry(doc, entry)
		if err != nil {
			return nil, false, err
		}
		if matches.full != 1 || matches.related != 1 {
			return nil, false, errCodexLegacyHooks
		}
	}
	base, err := undoInsertions(target.Current.Bytes, record.Undo)
	if err != nil || (record.Original.Exists && sum(base) != record.Original.Hash) ||
		(!record.Original.Exists && len(base) != 0) {
		return nil, false, errCodexLegacyHooks
	}
	return base, true, nil
}

func recordedCodexRoute(entry Entry) (codexHookRoute, error) {
	parsed, err := parseConfigEntry(entry)
	if err != nil || parsed.handler == nil {
		return codexHookRoute{}, errInvalidEntry
	}
	command, _ := parsed.handler.field("command").stringValue()
	if route, ok := parseCodexHookCommand(command, entry.Event); ok {
		return route, nil
	}
	return codexHookRoute{}, errInvalidEntry
}
