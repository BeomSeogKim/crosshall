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
	// errHookSkip is a callback outside the event's boundary (When).
	errHookSkip = errors.New("callback outside the boundary")
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
	// EventFieldOptional lets a callback omit its event name: the command's
	// configured event names it.
	EventFieldOptional bool `json:"event_field_optional"`
	// CWDField names the field an enrolling callback reports its workspace
	// in, "cwd" when empty; CWDList says it is a JSON list of exactly one
	// absolute path. A declared CWDField is required.
	CWDField string `json:"cwd_field"`
	CWDList  bool   `json:"cwd_list"`
	// IdentityProof names the native check (identityProofs) that a
	// callback's session key really is this vendor's session.
	IdentityProof string `json:"identity_proof"`
	// EmptyOutput is what a callback prints when crosshall has nothing to
	// say, for a vendor that needs a reply on stdout.
	EmptyOutput string `json:"empty_output"`
	// The launch's own vendor build and mode, when the running agent names
	// them in its hooks' environment: VersionEnv must match VersionPattern
	// (the observed label grammar), whose first group is the version with
	// '-' for '.'; Modes maps ModeEnv values to a mode.
	VersionEnv     string            `json:"version_env"`
	VersionPattern string            `json:"version_pattern"`
	ModeEnv        string            `json:"mode_env"`
	ModeField      string            `json:"mode_field"`
	Modes          map[string]string `json:"modes"`
	// BuildFromHost takes the launch's vendor version from the build setup
	// recorded for the executable that runs the hook (HostBuildsName), and
	// its mode from that executable's arguments: HostModes maps a mode to
	// the flags that select it, HostModeDefault applies without one.
	BuildFromHost   bool                `json:"build_from_host"`
	HostModes       map[string][]string `json:"host_modes"`
	HostModeDefault string              `json:"host_mode_default"`
	// LiteralFields are read as their JSON text: integers and booleans a
	// vendor reports as such, never strings.
	LiteralFields []string `json:"literal_fields"`
	// StopActive names the Stop field that says a continuation is running,
	// and its value when none is; stop_hook_active ("false") when unset.
	StopActive *stopActiveSpec `json:"stop_active"`
	// Daemon names the vendor's managed server when its sessions can run as
	// its clients. The hooks of such a session run under that server, so a
	// hook's host tells the two modes apart and names the server's release
	// (#236).
	Daemon *daemonSpec `json:"daemon"`
}

// daemonSpec locates a vendor's managed server: its releases directory
// under the vendor's home (HomeEnv, else HomeDefault under $HOME), and the
// arguments it runs with.
type daemonSpec struct {
	HomeEnv     string   `json:"home_env"`
	HomeDefault string   `json:"home_default"`
	Releases    string   `json:"releases"`
	Args        []string `json:"args"`
}

type stopActiveSpec struct {
	Field    string `json:"field"`
	Inactive string `json:"inactive"`
}

type eventSpec struct {
	Name   string `json:"-"`
	Fact   string `json:"fact"`
	Output string `json:"output"`
	Enroll bool   `json:"enroll"`
	// Source is the start source of an enrolling callback whose input
	// names none (SourceField empty).
	Source        string            `json:"source"`
	RecoverEnroll bool              `json:"recover_enroll"`
	Notice        bool              `json:"notice"`
	Stop          bool              `json:"stop"`
	TurnRequired  bool              `json:"turn_required"`
	SourceField   string            `json:"source_field"`
	ReasonField   string            `json:"reason_field"`
	MatchField    string            `json:"match_field"`
	FactsByMatch  map[string]string `json:"facts_by_match"`
	// When lists field values a callback must report to be a boundary at
	// all; any other callback of the event is answered empty at once.
	When map[string]string `json:"when"`
	// TurnField overrides the manifest's turn field for this event.
	// OpenTurn records the callback's turn reference as its session's open
	// turn; TurnFromOpen takes the open turn's reference, for a vendor
	// whose turn end names no turn.
	TurnField    string `json:"turn_field"`
	OpenTurn     bool   `json:"open_turn"`
	TurnFromOpen bool   `json:"turn_from_open"`
	// RefFields name the fields whose values tell repeated callbacks of
	// one turn apart in the event reference.
	RefFields []string `json:"ref_fields"`
	// StopRequire lists the field values a Stop needs to offer a
	// continuation; any other Stop is published as an observation.
	StopRequire map[string]string `json:"stop_require"`
}

func (s eventSpec) turnField(m manifest) string {
	if s.TurnField != "" {
		return s.TurnField
	}
	return m.TurnField
}

func (m manifest) stopActive() stopActiveSpec {
	if m.StopActive != nil {
		return *m.StopActive
	}
	return stopActiveSpec{Field: stopActiveField, Inactive: "false"}
}

func loadManifest(adapter string) (manifest, bool) {
	var m manifest
	raw, err := manifests.Read(adapter)
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
	// LaunchAtMS is when the launch's record was written; a prompt, turn end
	// or idle prompt sends it so the dispatcher can register a launch whose
	// start it never saw.
	LaunchAtMS    int64
	TurnRef       string
	EventRef      string
	Fact          string
	VendorVersion string
	Mode          string
	// Host is the managed server that runs the hook, in daemon mode, and
	// VendorHome the home it serves.
	Host       *wire.Host
	VendorHome string
	receipt    string
	stopActive string
	// refSuffix carries the event's RefFields values into its reference.
	refSuffix   string
	name        string
	startupRepo *string
}

const stopActiveField = "stop_hook_active"

// identityProof is a vendor's native check that a callback's session key
// names one of its own sessions: the input fields it reads, and the check.
type identityProof struct {
	fields []string
	check  func(key string, fields map[string]string, lookup func(string) (string, bool)) bool
}

var identityProofs = map[string]identityProof{
	// Antigravity names its transcript under the conversation's brain
	// directory; the key must be a conversation ID and the transcript its own.
	"antigravity_transcript": {
		fields: []string{"transcriptPath"},
		check: func(key string, fields map[string]string, lookup func(string) (string, bool)) bool {
			home, _ := lookup("HOME")
			logDir := filepath.Join(home, ".gemini", "antigravity-cli", "brain", key, ".system_generated", "logs")
			transcript := fields["transcriptPath"]
			return antigravityConversationID.MatchString(key) && filepath.IsAbs(home) &&
				(transcript == filepath.Join(logDir, "transcript.jsonl") || transcript == filepath.Join(logDir, "transcript_full.jsonl"))
		},
	},
}

var antigravityConversationID = regexp.MustCompile(`^[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}$`)

func (m manifest) cwdField() string {
	if m.CWDField != "" {
		return m.CWDField
	}
	return "cwd"
}

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
	if callback != spec.Name && (!m.EventFieldOptional || named) {
		return event{}, errHookInput
	}
	for field, want := range spec.When {
		if fields[field] != want {
			return event{}, errHookSkip
		}
	}
	fact := spec.Fact
	if spec.MatchField != "" {
		fact = spec.FactsByMatch[fields[spec.MatchField]]
		if fact == "" {
			return event{}, errHookInput
		}
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

	turnRef := fields[spec.turnField(m)]
	if fact == wire.HookIdlePrompt {
		turnRef = ""
	}
	e := event{
		Identity:   id,
		name:       spec.Name,
		Fact:       fact,
		TurnRef:    turnRef,
		Source:     fields[spec.SourceField],
		Reason:     fields[spec.ReasonField],
		receipt:    receipt,
		stopActive: stopActivity(m, spec, fields),
	}
	for _, field := range spec.RefFields {
		value := fields[field]
		if value == "" {
			continue // such a callback is only observed (stopActivity)
		}
		if len(value) > 32 || strings.IndexFunc(value, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
			return event{}, errHookInput
		}
		e.refSuffix += ":" + value
	}
	if m.IdentityProof != "" {
		if proof, known := identityProofs[m.IdentityProof]; !known || !proof.check(id.Key, fields, lookup) {
			return event{}, errHookIdentity
		}
	}
	if spec.Enroll && m.CWDField != "" && fields[m.CWDField] == "" {
		return event{}, errHookInput
	}
	if spec.Source != "" {
		e.Source = spec.Source
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
		e.CWD = fields[m.cwdField()]
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
	if m.Daemon != nil {
		e.VendorVersion, e.Mode, e.Host, e.VendorHome = daemonBuild(*m.Daemon, lookup)
	}
	if m.ModeField != "" {
		if pm := fields[m.ModeField]; pm != "" {
			if len(pm) > 256 || strings.IndexFunc(pm, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
				return event{}, errHookInput
			}
			e.PermissionMode = pm
		}
	}
	e.setEventRef(spec)
	return e, nil
}

func (e *event) setEventRef(spec eventSpec) {
	e.EventRef = ""
	if !spec.Enroll && spec.ReasonField == "" && e.TurnRef != "" {
		e.EventRef = e.TurnRef + ":" + spec.Name + e.refSuffix
	}
}

// stopActivity is "false" for a Stop that may offer a continuation, "true"
// while one runs, and "" when the callback cannot tell: an observation.
func stopActivity(m manifest, spec eventSpec, fields map[string]string) string {
	if !spec.Stop {
		return ""
	}
	for field, want := range spec.StopRequire {
		if fields[field] != want {
			return ""
		}
	}
	active := m.stopActive()
	switch value := fields[active.Field]; {
	case value == "":
		return ""
	case value == active.Inactive:
		return "false"
	case active.Field == stopActiveField && value != "true":
		return ""
	default:
		return "true"
	}
}

// Prompt text stays local. Only a marker and validated message IDs may leave it.
func eventFields(body []byte, m manifest, spec eventSpec) (map[string]string, string, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, "", errHookInput
	}
	selected := []string{m.EventField, m.SessionField, spec.turnField(m), spec.SourceField, spec.ReasonField, spec.MatchField}
	selected = append(selected, spec.RefFields...)
	for field := range spec.When {
		selected = append(selected, field)
	}
	for field := range spec.StopRequire {
		selected = append(selected, field)
	}
	if m.ModeField != "" {
		selected = append(selected, m.ModeField)
	}
	if spec.Enroll {
		selected = append(selected, m.cwdField())
	}
	if proof, known := identityProofs[m.IdentityProof]; known {
		selected = append(selected, proof.fields...)
	}
	if spec.RecoverEnroll {
		selected = append(selected, "cwd")
	}
	if spec.Stop {
		selected = append(selected, m.stopActive().Field)
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
		if slices.Contains(m.LiteralFields, key) {
			// A literal the vendor sends as JSON text; a repeated or other
			// value drops the field, so a gated callback is skipped and a
			// Stop is only observed.
			literal := string(bytes.TrimSpace(raw))
			if duplicated || !validLiteral(literal) {
				literal = ""
			}
			fields[key] = literal
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
		if key == m.CWDField && m.CWDList {
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

// validLiteral accepts a JSON boolean or a non-negative integer of at most
// 18 digits.
func validLiteral(v string) bool {
	if v == "true" || v == "false" {
		return true
	}
	if v == "" || len(v) > 18 || (v[0] == '0' && len(v) > 1) {
		return false
	}
	return strings.IndexFunc(v, func(r rune) bool { return r < '0' || r > '9' }) < 0
}
