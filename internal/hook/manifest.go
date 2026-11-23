// Package hook translates native lifecycle events into member operations.
package hook

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/BeomSeogKim/crosshall/internal/hook/manifests"
	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/notice"
	"github.com/BeomSeogKim/crosshall/internal/publish"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

var (
	errHookInput    = errors.New("invalid hook input")
	errHookIdentity = errors.New("invalid hook identity")
)

type manifest struct {
	Install      InstallSpec `json:"install"`
	Adapter      string      `json:"adapter"`
	Revision     string      `json:"revision"`
	EventField   string      `json:"event_field"`
	SessionField string      `json:"session_field"`
	SessionEnv   string      `json:"session_env"`
	LaunchEnv    string      `json:"launch_env"`
	HostNames    []string    `json:"host_names"`
	SocketEnv    string      `json:"socket_env"`
	TurnField    string      `json:"turn_field"`
	// PromptEntryRef adds the hook's entry time to a prompt's reference:
	// Claude Code reports the running turn's id for a prompt typed ahead
	// (#163), so the id alone does not tell the two callbacks apart.
	PromptEntryRef bool                 `json:"prompt_entry_ref"`
	Events         map[string]eventSpec `json:"events"`
	// The launch's own vendor build and mode, when the running agent names
	// them in its hooks' environment: VersionEnv must match VersionPattern
	// (the observed label grammar), whose first group is the version with
	// '-' for '.'; Modes maps ModeEnv values to a mode.
	VersionEnv     string            `json:"version_env"`
	VersionPattern string            `json:"version_pattern"`
	ModeEnv        string            `json:"mode_env"`
	ModeField      string            `json:"mode_field"`
	Modes          map[string]string `json:"modes"`
}

type eventSpec struct {
	Name          string `json:"-"`
	Fact          string `json:"fact"`
	Output        string `json:"output"`
	Enroll        bool   `json:"enroll"`
	RecoverEnroll bool   `json:"recover_enroll"`
	Notice        bool   `json:"notice"`
	Stop          bool   `json:"stop"`
	TurnRequired  bool   `json:"turn_required"`
	SourceField   string `json:"source_field"`
	ReasonField   string `json:"reason_field"`
	MatchField    string `json:"match_field"`
	MatchValue    string `json:"match_value"`
}

func loadManifest(adapter string) (manifest, bool) {
	var m manifest
	raw, err := manifests.FS.ReadFile(adapter + ".json")
	if err != nil || json.Unmarshal(raw, &m) != nil || m.Adapter != adapter || m.Revision == "" {
		return manifest{}, false
	}
	return m, true
}

func (m manifest) event(name string) (eventSpec, bool) {
	spec, ok := m.Events[name]
	spec.Name = name
	return spec, ok
}

type event struct {
	Identity       ident.Identity
	CWD            string
	Source         string
	Reason         string
	SocketPath     string
	Start          bool
	PermissionMode string
	LaunchRef      string
	LaunchVerified bool
	LaunchHosted   bool
	PIDNamespace   string
	TurnRef        string
	EventRef       string
	Fact           string
	VendorVersion  string
	Mode           string
	receipt        string
	stopActive     string
	name           string
	startupRepo    *string
}

const stopActiveField = "stop_hook_active"
const agyWorkspacePathsField = "workspacePaths"
const agyTranscriptField = "transcriptPath"

var agyConversationID = regexp.MustCompile(`^[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}$`)

// Turn callbacks keep their native identity across re-delivery. Lifecycle
// callbacks have no native event ID, so also carry the publication time.
func (e event) reference(at time.Time) string {
	if e.EventRef != "" {
		return e.EventRef
	}
	return e.name + ":" + e.Source + e.Reason + ":" + strconv.FormatInt(at.UnixMilli(), 10)
}

const maxHookInput = 64 << 10

func parseEvent(input io.Reader, m manifest, spec eventSpec, lookup func(string) (string, bool)) (event, error) {
	if input == nil {
		return event{}, errHookInput
	}
	body, err := io.ReadAll(io.LimitReader(input, maxHookInput+1))
	if err != nil || len(body) > maxHookInput || !utf8.Valid(body) {
		return event{}, errHookInput
	}
	fields, receipt, err := eventFields(body, m, spec)
	if err != nil {
		return event{}, err
	}
	callback, named := fields[m.EventField]
	if (callback != spec.Name && (m.Adapter != "agy" || named)) ||
		(spec.MatchField != "" && fields[spec.MatchField] != spec.MatchValue) {
		return event{}, errHookInput
	}

	// A native payload can supply its own vendor variable. Retain the normal
	// resolver's disagreement checks for every other recognized vendor.
	key := fields[m.SessionField]
	if key == "" {
		return event{}, errHookIdentity
	}
	if value, present := lookup(m.SessionEnv); present && value != key {
		return event{}, errHookIdentity
	}
	id, err := ident.ResolveEnvironment(func(name string) (string, bool) {
		if name == m.SessionEnv {
			return key, true
		}
		return lookup(name)
	})
	if err != nil || id.Vendor != m.Adapter {
		return event{}, errHookIdentity
	}

	e := event{
		Identity:   id,
		name:       spec.Name,
		Fact:       spec.Fact,
		TurnRef:    fields[m.TurnField],
		Source:     fields[spec.SourceField],
		Reason:     fields[spec.ReasonField],
		receipt:    receipt,
		stopActive: fields[stopActiveField],
	}
	if m.Adapter == "agy" {
		home, _ := lookup("HOME")
		logDir := filepath.Join(home, ".gemini", "antigravity-cli", "brain", id.Key, ".system_generated", "logs")
		transcript := fields[agyTranscriptField]
		if !agyConversationID.MatchString(id.Key) || !filepath.IsAbs(home) ||
			(transcript != filepath.Join(logDir, "transcript.jsonl") && transcript != filepath.Join(logDir, "transcript_full.jsonl")) {
			return event{}, errHookIdentity
		}
		if fields[agyWorkspacePathsField] == "" {
			return event{}, errHookInput
		}
		e.Source = "startup"
	}
	if (spec.TurnRequired && e.TurnRef == "") || (spec.Enroll && e.Source == "") {
		return event{}, errHookInput
	}
	for _, v := range []string{e.TurnRef, e.Source, e.Reason} {
		if len(v) > 256 || strings.IndexFunc(v, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
			return event{}, errHookInput
		}
	}
	if spec.Enroll {
		e.CWD = fields["cwd"]
		if m.Adapter == "agy" {
			e.CWD = fields[agyWorkspacePathsField]
		}
		if e.CWD != "" && (!filepath.IsAbs(e.CWD) || strings.IndexByte(e.CWD, 0) >= 0) {
			return event{}, errHookInput
		}
	}
	// A start may run before the vendor's socket exists; a turn's callbacks
	// report it too, so a route can get it later (#180).
	if m.SocketEnv != "" && (spec.Enroll || spec.TurnRequired) {
		if sock, present := lookup(m.SocketEnv); present {
			e.SocketPath = sock
		}
	}
	e.Start = spec.Enroll
	if spec.RecoverEnroll && filepath.IsAbs(fields["cwd"]) && strings.IndexByte(fields["cwd"], 0) < 0 {
		e.CWD = fields["cwd"]
	}
	if m.LaunchEnv != "" {
		value, present := lookup(m.LaunchEnv)
		if present {
			// An unusable value leaves the launch session-scoped; it is no
			// reason to drop the callback.
			if pid, err := strconv.ParseUint(value, 10, 32); err == nil && pid != 0 {
				e.LaunchRef = value
			}
		}
	}
	e.VendorVersion, e.Mode = launchBuild(m, lookup)
	if m.ModeField != "" {
		if pm := fields[m.ModeField]; pm != "" {
			if len(pm) > 256 || strings.IndexFunc(pm, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
				return event{}, errHookInput
			}
			e.PermissionMode = pm
		}
	}
	if !spec.Enroll && spec.ReasonField == "" && e.TurnRef != "" {
		e.EventRef = e.TurnRef + ":" + spec.Name
	}
	return e, nil
}

// Prompt text stays local. Only a marker and validated message IDs may leave it.
func eventFields(body []byte, m manifest, spec eventSpec) (map[string]string, string, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, "", errHookInput
	}
	selected := []string{m.EventField, m.SessionField, m.TurnField, spec.SourceField, spec.ReasonField, spec.MatchField}
	if m.ModeField != "" {
		selected = append(selected, m.ModeField)
	}
	if spec.Enroll {
		if m.Adapter == "agy" {
			selected = append(selected, agyWorkspacePathsField, agyTranscriptField)
		} else {
			selected = append(selected, "cwd")
		}
	}
	if spec.RecoverEnroll {
		selected = append(selected, "cwd")
	}
	if spec.Stop {
		selected = append(selected, stopActiveField)
	}
	fields := make(map[string]string)
	// An adapter that publishes recognizes its own notice echoed back as a
	// prompt; the shared grammar validates the IDs it carries.
	sender, publishes := publish.Lookup(m.Adapter)
	captureReceipt := publishes && spec.Fact == wire.HookPromptSubmitted
	var receipt string
	var seenPrompt bool
	for dec.More() {
		tok, err := dec.Token()
		key, ok := tok.(string)
		if err != nil || !ok {
			return nil, "", errHookInput
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, "", errHookInput
		}
		if captureReceipt && key == "prompt" {
			var prompt string
			receipt = ""
			if !seenPrompt && json.Unmarshal(raw, &prompt) == nil {
				if ids, ok := sender.Echo(prompt); ok {
					receipt = notice.Receipt(ids)
				}
			}
			seenPrompt = true
			continue
		}
		if key == "" || !slices.Contains(selected, key) {
			continue
		}
		_, duplicated := fields[key]
		if key == "cwd" && spec.RecoverEnroll {
			var path string
			if duplicated || json.Unmarshal(raw, &path) != nil {
				path = ""
			}
			fields[key] = path
			continue
		}
		if key == stopActiveField {
			flag := string(bytes.TrimSpace(raw))
			if duplicated || (flag != "true" && flag != "false") {
				// Keep the ordinary observation, with no continuation offer.
				flag = ""
			}
			fields[key] = flag
			continue
		}
		if duplicated {
			return nil, "", errHookInput
		}
		if key == agyWorkspacePathsField && m.Adapter == "agy" {
			var paths []string
			if err := json.Unmarshal(raw, &paths); err != nil || len(paths) != 1 || !filepath.IsAbs(paths[0]) {
				return nil, "", errHookInput
			}
			fields[key] = paths[0]
			continue
		}
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			if key == m.SessionField {
				return nil, "", errHookIdentity
			}
			return nil, "", errHookInput
		}
		fields[key] = value
	}
	if _, err := dec.Token(); err != nil {
		return nil, "", errHookInput
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, "", errHookInput
	}
	return fields, receipt, nil
}

// launchBuild reads the launch's vendor version and mode from the hook's
// environment as the manifest describes; anything that does not parse is
// "unverified".
func launchBuild(m manifest, lookup func(string) (string, bool)) (string, string) {
	version, mode := "unverified", "unverified"
	// Only a label in the observed grammar counts; any other build, a
	// pre-release suffix included, stays unverified.
	if m.VersionEnv != "" && m.VersionPattern != "" {
		if raw, ok := lookup(m.VersionEnv); ok {
			if pattern, err := regexp.Compile(m.VersionPattern); err == nil {
				if match := pattern.FindStringSubmatch(raw); len(match) == 2 {
					version = strings.ReplaceAll(match[1], "-", ".")
				}
			}
		}
	}
	if m.ModeEnv != "" {
		if raw, ok := lookup(m.ModeEnv); ok && m.Modes[raw] != "" {
			mode = m.Modes[raw]
		}
	}
	return version, mode
}
