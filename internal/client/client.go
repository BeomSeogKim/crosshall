// Package client delivers parsed CLI commands to a local dispatcher.
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/cli"
	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/proc"
	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

// Transport belongs to one CLI invocation. It retains the reply until the
// caller has finished rendering it.
type Transport struct {
	spool       spool.Client
	lookup      func(string) (string, bool)
	pending     spool.Reply
	entry       time.Time
	startMember func(context.Context) (bool, error)
	joinLaunch  func(ident.Identity, func(string) (string, bool), time.Time) *domain.JoinLaunch
	bindLaunch  func(ident.Identity, domain.JoinLaunch) error
	host        func(ident.Identity, func(string) (string, bool)) *wire.Host
}

// EnableHost lets each member command name the agent process that runs it.
func (t *Transport) EnableHost(host func(ident.Identity, func(string) (string, bool)) *wire.Host) {
	t.host = host
}

// EnableJoinLaunch lets a join name the agent process it runs in, and records
// the launch the dispatcher bound where the session's hooks read it.
func (t *Transport) EnableJoinLaunch(propose func(ident.Identity, func(string) (string, bool), time.Time) *domain.JoinLaunch,
	bind func(ident.Identity, domain.JoinLaunch) error) {
	t.joinLaunch, t.bindLaunch = propose, bind
}

// HostGone names the agent process a member command would name and returns
// a check that reports proof it has ended, or nil when no process can be
// named. A wait uses it to end with the agent that started it.
func (t *Transport) HostGone() func() bool {
	if t.host == nil {
		return nil
	}
	id, err := ident.ResolveEnvironment(t.lookup)
	if err != nil {
		return nil
	}
	host := t.host(id, t.lookup)
	if host == nil {
		return nil
	}
	return func() bool { return proc.ProcessGone(host.PID, host.Start) }
}

func New(s spool.Client) *Transport {
	return &Transport{spool: s, lookup: os.LookupEnv}
}

// EnableMemberStart allows an identified member command to try starting the
// dispatcher. Allowances for startup and transport share the CLI entry time.
func (t *Transport) EnableMemberStart(entry time.Time, start func(context.Context) (bool, error)) {
	t.entry, t.startMember = entry, start
}

func (t *Transport) DispatcherRunning() bool {
	return t.spool.DispatcherRunning()
}

func (t *Transport) memberCommandContext(parent context.Context) (context.Context, context.CancelFunc) {
	if t.startMember == nil {
		return context.WithCancel(parent)
	}
	warm, cancelWarm := context.WithDeadline(parent, spool.Deadline(t.entry, false))
	if warm.Err() != nil {
		return warm, cancelWarm
	}
	cold, cancelCold := context.WithDeadline(parent, spool.Deadline(t.entry, true))
	// A restricted member can fail the home-state probe while still having
	// access to the rendezvous of a running dispatcher.
	spawned, _ := t.startMember(cold)
	if spawned {
		cancelWarm()
		return cold, cancelCold
	}
	cancelCold()
	return warm, cancelWarm
}

// Do returns enrollment and delivery outcomes through the command result.
func (t *Transport) Do(ctx context.Context, call cli.Call) (core.Result, error) {
	t.pending = spool.Reply{}
	var launch *domain.JoinLaunch
	if join, ok := call.Command.(domain.Join); ok && !call.Owner && t.joinLaunch != nil {
		if id, err := ident.ResolveEnvironment(t.lookup); err == nil {
			launch = t.joinLaunch(id, t.lookup, time.Now())
			join.Launch = launch
			call.Command = join
		}
	}
	req, err := request(call.Command)
	if err != nil {
		return core.Result{}, err
	}
	req.OpKey = call.OpKey
	req.OwnerAlerts = call.Owner && call.OwnerAlerts
	if strings.HasPrefix(req.Verb, "owner ") != call.Owner {
		return localFailure(core.ExitRefused, core.CodeOwnerOnly, "command and ingress disagree"), nil
	}
	var id ident.Identity
	var reply spool.Reply
	if call.Owner {
		if !ident.AllowOwner(t.lookup, true) {
			return localFailure(core.ExitRefused, core.CodeOwnerOnly, "owner commands require a separate owner shell"), nil
		}
		reply, err = t.spool.Owner(ctx, req)
	} else {
		var resolveErr error
		id, resolveErr = ident.ResolveEnvironment(t.lookup)
		if resolveErr != nil {
			return identityFailure(resolveErr), nil
		}
		// A wait only reads; a background wait that outlived its agent
		// must not speak for it.
		if t.host != nil && req.Verb != domain.VerbWait {
			req.Host = t.host(id, t.lookup)
		}
		var memberCtx context.Context
		var cancel context.CancelFunc
		if req.Verb == domain.VerbWait {
			// Each snapshot has a normal request budget. An observer uses
			// the running dispatcher without attempting to start one.
			memberCtx, cancel = context.WithDeadline(ctx, spool.Deadline(time.Now(), false))
		} else {
			memberCtx, cancel = t.memberCommandContext(ctx)
		}
		defer cancel()
		reply, err = t.spool.Member(memberCtx, id, req)
	}
	if err != nil && reply.Status != core.ExitUnavailable {
		return core.Result{}, err
	}
	result := reply.Result
	if result.Status == core.ExitOK {
		switch req.Verb {
		case "join", "who", "owner doctor":
			hintAcked := result.JoinHintAcked || reply.PriorRosterAck > 0
			result.Data, err = withWarnings(result.Data, id, req.Verb, hintAcked)
			if err != nil {
				return core.Result{}, err
			}
		}
		t.pending = reply
		if launch != nil {
			var bound domain.JoinResult
			// A replayed join returns the launch its first call bound. Without
			// the record the hooks keep the session's unbound launch.
			if json.Unmarshal(result.Data, &bound) == nil && bound.Launch != nil && sameProcess(bound.Launch.Ref, launch.Ref) {
				_ = t.bindLaunch(id, *bound.Launch)
			}
		}
	}
	return result, nil
}

// Acknowledge advances the roster cursor for the successfully rendered reply.
func (t *Transport) Acknowledge() error {
	reply := t.pending
	t.pending = spool.Reply{}
	return reply.Acknowledge()
}

// Whoami displays the local identity even if the dispatcher is unavailable.
// The normal membership check also distinguishes provisional and departed
// bindings from a live membership.
func (t *Transport) Whoami(stdout io.Writer, full bool) core.ExitCode {
	return t.whoami(stdout, false, full)
}

// WhoamiJSON writes the same identity diagnostic as a single JSON object.
func (t *Transport) WhoamiJSON(stdout io.Writer, full bool) core.ExitCode {
	return t.whoami(stdout, true, full)
}

func (t *Transport) whoami(stdout io.Writer, asJSON, full bool) core.ExitCode {
	id, err := ident.ResolveEnvironment(t.lookup)
	var result core.Result
	var reply spool.Reply
	if err != nil {
		result = identityFailure(err)
	} else {
		req := wire.Request{Verb: domain.VerbWho}
		if t.host != nil {
			req.Host = t.host(id, t.lookup)
		}
		if full {
			req.Args = json.RawMessage(`{"exit_tracking":true}`)
		}
		reply, _ = t.spool.Member(context.Background(), id, req)
		// An already-running older dispatcher accepts who without the diagnostic argument.
		if full && reply.Result.Status == core.ExitUsage {
			req.Args = nil
			reply, _ = t.spool.Member(context.Background(), id, req)
		}
		result = reply.Result
	}
	status, rules := writeWhoami(stdout, id, result, asJSON, full)
	if status == core.ExitOK && rules {
		// The rules a first command carries were printed with the roster
		// changes since the start: neither is owed again. Without them the
		// reply's roster was not shown, and nothing is acknowledged.
		_ = reply.Acknowledge()
	}
	return status
}

// writeWhoami also reports whether it wrote the member rules of a first command.
func writeWhoami(stdout io.Writer, id ident.Identity, result core.Result, asJSON, full bool) (core.ExitCode, bool) {
	state := "unknown"
	switch {
	case result.Status == core.ExitOK:
		state = "enrolled"
	case result.Code == core.CodeNotEnrolled:
		state = "not_enrolled"
	}
	var message struct {
		Message, Hint string
		Pending       []int64             `json:"pending"`
		OwedCount     *int                `json:"owed_count"`
		JoinHint      string              `json:"join_hint"`
		Changes       json.RawMessage     `json:"roster_changes"`
		ExitTracking  domain.ExitTracking `json:"exit_tracking"`
	}
	_ = json.Unmarshal(result.Data, &message)
	restoreHint := ""
	if full && state == "not_enrolled" && id.Vendor != "" && id.Key != "" {
		restoreHint = "To restore a previous membership, give the owner the displayed `<vendor>:<key>` privately for `crosshall owner restore` before joining."
	}
	displayKey := id.Key
	if displayKey != "" && !full {
		displayKey = "sha256:" + id.Digest()[:12]
		message.Message = strings.ReplaceAll(message.Message, id.Key, displayKey)
		message.Hint = strings.ReplaceAll(message.Hint, id.Key, displayKey)
	}
	warning := ""
	if id.UnknownAntigravityCLI() {
		warning = id.Warning()
	}
	var err error
	if asJSON {
		data := map[string]any{"v": 1, "ok": result.Status == core.ExitOK, "kind": "whoami", "enrollment": state}
		if warning != "" {
			data["warnings"] = []string{warning}
		}
		if id.Vendor != "" {
			data["vendor"], data["key"] = id.Vendor, displayKey
		}
		if result.Status != core.ExitOK {
			hint := message.Hint
			if restoreHint != "" {
				hint = restoreHint
				if message.Hint != "" {
					hint += " For a new membership, run `" + message.Hint + "`."
				}
			}
			data["code"], data["message"], data["hint"] = result.Code, message.Message, hint
		}
		if full && message.ExitTracking != "" {
			data["exit_tracking"] = message.ExitTracking
		}
		if message.OwedCount != nil {
			data["pending"], data["owed_count"] = message.Pending, *message.OwedCount
		}
		if message.JoinHint != "" {
			data["join_hint"], data["roster_changes"] = message.JoinHint, message.Changes
		}
		err = json.NewEncoder(stdout).Encode(data)
	} else if id.Vendor == "" {
		_, err = fmt.Fprintf(stdout, "%s: %s\n", result.Code, message.Message)
		if err == nil && message.Hint != "" {
			_, err = fmt.Fprintln(stdout, "next: "+message.Hint)
		}
	} else if state == "unknown" && result.Code != "" {
		_, err = fmt.Fprintf(stdout, "%s:%s %s (%s)\n", id.Vendor, displayKey, state, result.Code)
	} else {
		_, err = fmt.Fprintf(stdout, "%s:%s %s\n", id.Vendor, displayKey, state)
	}
	if err == nil && !asJSON && full && message.ExitTracking != "" {
		_, err = fmt.Fprintln(stdout, "Session exit: "+message.ExitTracking.Description())
	}
	if err == nil && !asJSON && restoreHint != "" {
		_, err = fmt.Fprintln(stdout, restoreHint)
	}
	if err == nil && !asJSON && warning != "" {
		_, err = fmt.Fprintln(stdout, warning)
	}
	if err == nil && !asJSON && message.JoinHint != "" {
		err = cli.PrintFirstRules(stdout, result.Data)
	}
	if err == nil && !asJSON && message.OwedCount != nil {
		err = cli.PrintInboxPointer(stdout, len(message.Pending), *message.OwedCount, false)
	}
	if err != nil {
		return core.ExitError, false
	}
	return result.Status, message.JoinHint != ""
}

func identityFailure(err error) core.Result {
	var e *ident.EnvironmentError
	if errors.As(err, &e) {
		r := localFailure(core.ExitNotEnrolled, e.Code, e.Message)
		r.Data, _ = json.Marshal(map[string]string{"message": e.Message, "hint": identityHint(e.Code)})
		return r
	}
	return localFailure(core.ExitError, "", err.Error())
}

// identityHint names the next command for a session that cannot be identified.
func identityHint(code core.Code) string {
	if code == core.CodeBindingAmbiguous {
		return "start a member shell with `crosshall run -- sh` so it has one identity"
	}
	return "start a member shell with `crosshall run -- sh`, then join your channel from the new shell"
}

func localFailure(status core.ExitCode, code core.Code, message string) core.Result {
	data, _ := json.Marshal(map[string]string{"message": message})
	return core.Result{Status: status, Code: code, Data: data}
}

func request(cmd domain.Command) (wire.Request, error) {
	// Integrated domain commands carry their wire verb and JSON field tags.
	if named, ok := cmd.(interface{ Verb() string }); ok {
		args, err := json.Marshal(cmd)
		return wire.Request{Verb: named.Verb(), Args: args}, err
	}
	// The original shared types also support these three commands.
	fields := map[string]string{}
	var verb string
	switch c := cmd.(type) {
	case domain.Join:
		verb = "join"
		fields["channel"], fields["alias"], fields["focus"] = c.Channel, c.Alias, c.Focus
	case domain.Who:
		verb = "who"
	case domain.CreateChannel:
		verb = "owner channel create"
		fields["name"], fields["topic"] = c.Name, c.Topic
	default:
		return wire.Request{}, fmt.Errorf("unsupported command %T", cmd)
	}
	for k, v := range fields {
		if v == "" && k != "name" {
			delete(fields, k)
		}
	}
	args, err := json.Marshal(fields)
	return wire.Request{Verb: verb, Args: args}, err
}

func withWarnings(data json.RawMessage, id ident.Identity, verb string, hintAcked bool) (json.RawMessage, error) {
	type vendor struct {
		Name    string  `json:"vendor"`
		Version *string `json:"vendor_version"`
	}
	var details struct {
		Hint     string   `json:"hint"`
		Members  []vendor `json:"members"`
		Vendors  []vendor `json:"vendors"`
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal(data, &details); err != nil {
		return nil, err
	}
	original := len(details.Warnings)
	add := func(identity ident.Identity, showUnknownAntigravity bool) {
		if identity.UnknownAntigravityCLI() && !showUnknownAntigravity {
			return
		}
		msg := identity.Warning()
		if verb == "join" && msg != "" {
			switch identity.Vendor {
			case "claude", "codex":
				name := "Claude Code"
				if identity.Vendor == "codex" {
					name = "Codex CLI"
				}
				if identity.Version == "" || identity.Version == "unknown" {
					msg = fmt.Sprintf("Crosshall could not detect the %s version to verify how it identifies sessions.", name)
				} else {
					msg = fmt.Sprintf("Crosshall has not verified how %s %s identifies sessions.", name, identity.Version)
				}
				msg = "Join succeeded. " + msg + " No action is needed for this warning."
			case "agy":
				if !identity.UnknownAntigravityCLI() {
					msg = fmt.Sprintf("Crosshall has not verified how %s %s identifies sessions", identity.Vendor, identity.Version)
				}
				msg += ". Run `crosshall whoami` to check your current enrollment."
			}
		}
		if msg != "" && !slices.Contains(details.Warnings, msg) {
			details.Warnings = append(details.Warnings, msg)
		}
	}
	add(id, verb == "owner doctor" || (verb == "join" && !hintAcked && details.Hint != ""))
	for _, row := range append(details.Members, details.Vendors...) {
		if row.Version != nil {
			add(ident.Identity{Vendor: row.Name, Version: *row.Version}, verb == "owner doctor")
		}
	}
	if len(details.Warnings) == original {
		return data, nil
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(data, &result); err != nil || result == nil {
		return nil, fmt.Errorf("expected an object for roster or diagnostics")
	}
	result["warnings"], _ = json.Marshal(details.Warnings)
	return json.Marshal(result)
}

// sameProcess compares the process part of two "pid-<pid>-<digest>" launch
// references.
func sameProcess(a, b string) bool {
	pa, _, okA := strings.Cut(strings.TrimPrefix(a, "pid-"), "-")
	pb, _, okB := strings.Cut(strings.TrimPrefix(b, "pid-"), "-")
	return okA && okB && strings.HasPrefix(a, "pid-") && pa == pb
}
