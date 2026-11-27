package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"github.com/BeomSeogKim/crosshall/internal/hook/manifests"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

func runOwnerViews(ctx context.Context, env Env, opts options, args []string, stdout, stderr io.Writer) core.ExitCode {
	o := outputs{json: opts.json, stdout: stdout, stderr: stderr}
	if args[0] == "doctor" {
		if len(args) != 1 {
			return o.usage("doctor takes no arguments")
		}
		if env.Doctor == nil {
			return o.unavailableFor(env, "owner doctor")
		}
		return renderDoctor(o, env.Doctor(ctx))
	}
	call, err := ownerViewCall(args)
	if err != nil {
		return o.usage(err.Error())
	}
	if _, changesDefault := call.Command.(domain.ChannelDefault); changesDefault {
		call.OpKey = opKey(env, opts)
	}
	if env.Start != nil {
		if err := env.Start(ctx); err != nil {
			return render(opts, call.Command, domain.Fail(core.ExitUnavailable, core.CodeUnsent,
				"dispatcher could not start: "+err.Error(), "crosshall owner start"), stdout, stderr)
		}
	}
	return send(ctx, env, opts, call, stdout, stderr)
}

func ownerViewCall(args []string) (Call, error) {
	call := Call{Owner: true}
	switch args[0] {
	case "who":
		if len(args) != 2 || args[1] != "--all" {
			return Call{}, fmt.Errorf("usage: crosshall owner who --all")
		}
		call.Command = domain.OwnerWho{}
	case "log":
		usage := fmt.Errorf("usage: crosshall owner log <channel> [--since <id>] [--limit <n>]")
		if len(args) < 2 || !domain.ValidName(strings.TrimPrefix(args[1], "#")) {
			return Call{}, usage
		}
		page, err := parseLog(args[2:])
		if err != nil && strings.HasPrefix(err.Error(), "usage:") {
			return Call{}, usage
		}
		if err != nil {
			return Call{}, err
		}
		call.Command = domain.OwnerLog{Channel: strings.TrimPrefix(args[1], "#"), Since: page.Since, Limit: page.Limit}
	case "channel":
		if len(args) >= 2 && args[1] == "auto" {
			if len(args) != 3 || (args[2] != "on" && args[2] != "off") {
				return Call{}, fmt.Errorf("usage: crosshall owner channel auto on|off")
			}
			call.Command = domain.ChannelAuto{On: args[2] == "on"}
			return call, nil
		}
		if len(args) < 3 {
			return Call{}, fmt.Errorf("usage: crosshall owner channel default <channel> [--repo <directory>]")
		}
		channel := strings.TrimPrefix(args[2], "#")
		if !domain.ValidName(channel) {
			return Call{}, fmt.Errorf("invalid channel name %q", args[2])
		}
		var path string
		if len(args) > 3 {
			value, last, found, err := flagValue(args, 3, "--repo")
			if !found || err != nil || value == "" || last != len(args)-1 {
				return Call{}, fmt.Errorf("usage: crosshall owner channel default <channel> [--repo <directory>]")
			}
			path = value
		}
		root, err := RepoDefaultRoot(path)
		if err != nil {
			return Call{}, err
		}
		call.Command = domain.ChannelDefault{Channel: channel, RepoRoot: root}
	}
	return call, nil
}

func renderOwnerViews(cmd domain.Command, data []byte, w io.Writer) error {
	switch c := cmd.(type) {
	case domain.OwnerWho:
		var r domain.OwnerWhoResult
		if err := json.Unmarshal(data, &r); err != nil {
			return err
		}
		if len(r.Channels) == 0 {
			fmt.Fprintln(w, "No channels.")
		}
		if banner := notWokenBanner(r); banner != "" {
			fmt.Fprintln(w, banner)
		}
		for _, line := range approvalLines(r) {
			fmt.Fprintln(w, line)
		}
		for _, line := range takeoverLines(r.Takeovers) {
			fmt.Fprintln(w, line)
		}
		for _, line := range endedLines(r.Ended) {
			fmt.Fprintln(w, line)
		}
		hasToken := false
		for _, ch := range r.Channels {
			fmt.Fprintf(w, "#%s", ch.Name)
			if ch.Topic != "" {
				fmt.Fprintf(w, " — %s", ch.Topic)
			}
			fmt.Fprintln(w)
			for _, member := range ch.Members {
				focus := member.Focus
				if focus == "" {
					focus = "focus unset"
				}
				state := member.BindingState
				if member.HostAlive {
					state += ", alive"
				} else if member.EvidenceStale {
					state += ", stale"
				}
				route := member.Route
				switch member.Reach {
				case "notice":
					route = "the hall starts its turn"
				case "prompt":
					route = "sees work at its next prompt"
				case "pull":
					route = "pull only: sees work when it runs a crosshall command"
				}
				if member.Publish != nil {
					route += "; publish: " + publishWords(*member.Publish)
				} else if member.StartUnseen {
					route += "; no notice route: its session start was not seen"
				}
				fmt.Fprintf(w, "  @%s [binding %d] %s, %s, %s; %s, %s",
					member.Alias, member.BindingID, state, member.Vendor, focus, route, ownerEvidenceText(member))
				if member.Observed != nil {
					fmt.Fprintf(w, "; last observed: %s", observedText(member.Observed))
				}
				if member.ExitTracking != "" {
					fmt.Fprintf(w, "; %s", member.ExitTracking.Description())
				}
				if len(member.OpenRequests) > 0 {
					fmt.Fprint(w, "; open requests:")
					for _, id := range member.OpenRequests {
						fmt.Fprintf(w, " #%d", id)
					}
				}
				if stuck := member.UnreadNotWoken; stuck != nil {
					if stuck.Requests == 1 {
						fmt.Fprintf(w, "; 1 unread request, %s old", age(stuck.OldestAgeS))
					} else {
						fmt.Fprintf(w, "; %d unread requests, oldest %s", stuck.Requests, age(stuck.OldestAgeS))
					}
				}
				if member.DepartToken != "" {
					fmt.Fprintf(w, "; depart token: %s", member.DepartToken)
					hasToken = true
				}
				fmt.Fprintln(w)
			}
		}
		for _, build := range r.StoppedBuilds {
			name := manifests.DisplayName(build.Adapter)
			fmt.Fprintf(w, "Notices to %s %s are stopped since %s (notice %d", name, build.Version, clock(build.StoppedAtMS), build.Notice)
			if build.OriginAlias != "" {
				fmt.Fprintf(w, " to @%s, %s). After checking that session, turn them back on with: ", build.OriginAlias, build.Reason)
			} else {
				fmt.Fprintf(w, ", %s). The session that received it is gone; turn notices back on with: ", build.Reason)
			}
			fmt.Fprintf(w, "crosshall owner publish resume %d\n", build.ResumeBindingID)
		}
		if hasToken {
			fmt.Fprintln(w, "To depart a listed binding, run crosshall owner depart <binding-id> --seen <token>.")
		}
	case domain.OwnerLog:
		var r domain.LogResult
		if err := json.Unmarshal(data, &r); err != nil {
			return err
		}
		renderLog(w, r, c.Limit, "crosshall owner log "+r.Channel)
	case domain.ChannelDefault:
		var r domain.ChannelDefaultResult
		if err := json.Unmarshal(data, &r); err != nil {
			return err
		}
		fmt.Fprintf(w, "#%s is the default for %s.\n", r.Channel, r.RepoRoot)
		fmt.Fprintln(w, "Unjoined Claude Code and Codex CLI sessions that started here can join automatically on their next prompt in this repository.")
	case domain.ChannelAuto:
		var r domain.ChannelAutoResult
		if err := json.Unmarshal(data, &r); err != nil {
			return err
		}
		if r.On {
			fmt.Fprintln(w, "Automatic joining is on: a Claude Code or Codex CLI session that starts in a Git repository with no channel gets the repository's own channel.")
		} else {
			fmt.Fprintln(w, "Automatic joining is off: no channel is created for a repository. Repositories that already have a channel keep it.")
		}
	default:
		return fmt.Errorf("no owner display for %T", cmd)
	}
	return nil
}

func ownerEvidenceText(member domain.OwnerMember) string {
	evidence := member.LastEvidence
	if evidence == nil {
		return "last evidence: unknown (joined " + age(member.LastCommandAgoS) + " ago)"
	}
	switch evidence.Fact {
	case "unknown":
		return "last evidence: unknown for " + age(evidence.AgeS)
	case "command":
		return "last command " + age(evidence.AgeS) + " ago"
	}
	fact := factWords[evidence.Fact]
	if fact == "" {
		fact = evidence.Fact
	}
	return "last evidence: " + fact + " " + age(evidence.AgeS) + " ago"
}

type doctorCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

func renderDoctor(o outputs, r core.Result) core.ExitCode {
	var result struct {
		StateDir   string          `json:"state_dir"`
		Rendezvous string          `json:"rendezvous"`
		Checks     []doctorCheck   `json:"checks"`
		Hint       string          `json:"hint"`
		NotStarted bool            `json:"not_started"`
		Adapters   json.RawMessage `json:"adapters"`
		Warnings   []string        `json:"warnings"`
	}
	if err := json.Unmarshal(r.Data, &result); err != nil || len(result.Checks) == 0 {
		return o.fail(core.ExitError, CodeLocalError, "no diagnostic checks were returned", "")
	}
	if o.json {
		fields := map[string]any{"v": 1, "ok": r.Status == core.ExitOK, "kind": "owner doctor", "checks": result.Checks}
		if result.StateDir != "" {
			fields["state_dir"] = result.StateDir
		}
		if result.Rendezvous != "" {
			fields["rendezvous"] = result.Rendezvous
		}
		if result.NotStarted {
			fields["not_started"] = true
		}
		if len(result.Warnings) != 0 {
			fields["warnings"] = result.Warnings
		}
		if len(result.Adapters) != 0 {
			fields["adapters"] = result.Adapters
		}
		if r.Status != core.ExitOK {
			fields["code"], fields["message"], fields["hint"] = CodeLocalError, "one or more checks failed", result.Hint
		}
		if err := json.NewEncoder(o.stdout).Encode(fields); err != nil {
			return core.ExitError
		}
		return r.Status
	}
	if result.StateDir != "" {
		if _, err := fmt.Fprintf(o.stdout, "State directory: %s\n", result.StateDir); err != nil {
			return core.ExitError
		}
	}
	if result.Rendezvous != "" {
		if _, err := fmt.Fprintf(o.stdout, "Rendezvous directory: %s\n", result.Rendezvous); err != nil {
			return core.ExitError
		}
	}
	if result.NotStarted {
		if _, err := fmt.Fprintf(o.stdout, "Not started yet: this state directory has no hall. Start it with %s; ordinary crosshall commands and agent hooks start it too, but crosshall wait and crosshall owner doctor do not.\n", result.Hint); err != nil {
			return core.ExitError
		}
	}
	for _, check := range result.Checks {
		if result.NotStarted && !check.OK {
			continue
		}
		outcome := "FAIL"
		if check.OK {
			outcome = "PASS"
		}
		if _, err := fmt.Fprintf(o.stdout, "%s %s — %s\n", outcome, check.Name, check.Detail); err != nil {
			return core.ExitError
		}
	}
	if len(result.Adapters) != 0 {
		if err := renderDoctorAdapters(o.stdout, result.Adapters); err != nil {
			return core.ExitError
		}
	}
	for _, warning := range result.Warnings {
		if _, err := fmt.Fprintln(o.stdout, warning); err != nil {
			return core.ExitError
		}
	}
	if result.Hint != "" && !result.NotStarted {
		if _, err := fmt.Fprintf(o.stdout, "The dispatcher is not running; next: %s\n", result.Hint); err != nil {
			return core.ExitError
		}
	}
	return r.Status
}

func renderDoctorAdapters(w io.Writer, raw json.RawMessage) error {
	var adapters []struct {
		Adapter, Executable, Version, Trust string
		InstallState                        string `json:"install_state"`
		Intended, Installed, Drifted        int
		Policy                              *struct {
			Enabled, Reminder bool
			TurnEnd           string `json:"turn_end"`
			Epoch             uint64
		}
		Reminder *struct {
			Provenance         string
			ReleaseState       string `json:"release_state"`
			ExtraModelRequests int    `json:"extra_model_requests"`
			Evidence           []struct {
				Version, Platform, Mode, Level, Reason string
				ManifestRev                            string `json:"manifest_rev"`
			}
		}
		Evidence []struct {
			Version, Platform, Mode, Capability, Level string
			ManifestRev                                string `json:"manifest_rev"`
		}
		UnverifiedLaunches *int `json:"unverified_launches"`
		UnseenStarts       *int `json:"unseen_starts"`
		TurnEndNotice      *struct {
			Available                         bool
			Date                              string
			TrustedFrom                       string `json:"trusted_from,omitempty"`
			CommittedClaims                   int    `json:"committed_claims"`
			ExtraModelRequestsPerContinuation int    `json:"extra_model_requests_per_continuation"`
		} `json:"turn_end_notice"`
		Publish *struct {
			State        string
			Reason       string
			ActiveRoutes int `json:"active_routes"`
			TotalRoutes  int `json:"total_routes"`
			Counters     *store.DoctorPublishCounters
			Members      []store.DoctorPublishMember
			Builds       []store.DoctorPublishBuild
		} `json:"publish"`
		Hooks *struct {
			Date            string
			Counts          map[string]int
			P95MS           int `json:"p95_ms"`
			RecordedStarts  int `json:"recorded_starts"`
			HelperTimeouts  int `json:"helper_timeouts"`
			MissedDeadlines int `json:"missed_deadlines"`
		}
	}
	if err := json.Unmarshal(raw, &adapters); err != nil {
		return err
	}
	var output strings.Builder
	for _, a := range adapters {
		state := cmp.Or(a.InstallState, "unknown")
		if state == "unknown" {
			fmt.Fprintf(&output, "%s adapter — installation unknown, %d hooks intended\n", a.Adapter, a.Intended)
		} else {
			fmt.Fprintf(&output, "%s adapter — %s, %d/%d installed, %d drifted\n", a.Adapter, state, a.Installed, a.Intended, a.Drifted)
		}
		fmt.Fprintf(&output, "  executable %q, cached version %q, hook trust %s\n",
			cmp.Or(a.Executable, "unknown"), cmp.Or(a.Version, "unverified"), cmp.Or(a.Trust, "unknown"))
		if a.Policy == nil {
			fmt.Fprintln(&output, "  policy unknown")
		} else {
			policy, reminder := "off", "off"
			if a.Policy.Enabled {
				policy = "on"
			}
			if a.Policy.Reminder {
				reminder = "on"
			}
			fmt.Fprintf(&output, "  policy %s, reminder %s, epoch %d\n", policy, reminder, a.Policy.Epoch)
		}
		if a.Reminder != nil {
			fmt.Fprintf(&output, "  reply reminder %s (no opt-in)\n", a.Reminder.ReleaseState)
			for _, evidence := range a.Reminder.Evidence {
				fmt.Fprintf(&output, "  reply reminder evidence %s", evidence.Level)
				if evidence.Version != "" {
					fmt.Fprintf(&output, " for %q %q", evidence.Version, evidence.Platform)
					if evidence.Mode != "" {
						fmt.Fprintf(&output, " %q", evidence.Mode)
					}
					fmt.Fprintf(&output, " %q", evidence.ManifestRev)
				}
				if evidence.Reason != "" {
					fmt.Fprintf(&output, " — %s", evidence.Reason)
				}
				fmt.Fprintln(&output)
			}
			fmt.Fprintf(&output, "  extra model requests per reminder %d — %s\n", a.Reminder.ExtraModelRequests, a.Reminder.Provenance)
		}
		if notice := a.TurnEndNotice; notice == nil {
			fmt.Fprintln(&output, "  turn-end notice status unknown")
		} else {
			status := "unavailable"
			if a.Policy != nil && a.Policy.TurnEnd == "off" {
				status = fmt.Sprintf("off by the owner (undo: crosshall owner adapter set %s --turn-end default)", a.Adapter)
			} else if notice.Available {
				status = "available"
				if notice.TrustedFrom != "" {
					status += fmt.Sprintf(" (trusted from %s evidence)", notice.TrustedFrom)
				}
			}
			fmt.Fprintf(&output, "  turn-end notice %s, committed claims %d on %s UTC, %d extra model request per continuation\n",
				status, notice.CommittedClaims, notice.Date, notice.ExtraModelRequestsPerContinuation)
		}
		if a.Publish != nil {
			fmt.Fprintf(&output, "  publish: %s (%s, %d active / %d total routes)\n",
				a.Publish.State, a.Publish.Reason, a.Publish.ActiveRoutes, a.Publish.TotalRoutes)
			for _, build := range a.Publish.Builds {
				words := build.State
				switch build.State {
				case "trusted":
					words = fmt.Sprintf("trusted %d of %d", build.Confirmed, store.PublishBuildConfirmations)
					if build.HeldUnobserved != nil {
						words += fmt.Sprintf(", %d held unobserved", *build.HeldUnobserved)
					}
				case "stopped":
					words = fmt.Sprintf("stopped at %s (notice %d, %s)", clock(build.StoppedAtMS), build.Notice, build.Reason)
				case "candidate":
					words = fmt.Sprintf("candidate (%s)", build.Reason)
				}
				fmt.Fprintf(&output, "  publish build %s (%s %s %s): %s\n", build.Version, build.Platform, build.Mode, build.ManifestRev, words)
			}
			if c := a.Publish.Counters; c != nil {
				fmt.Fprintf(&output, "  publish notices on %s UTC: %d submitted, %d uncertain, %d not submitted; observed %d own turn, %d absorbed, %d unknown%s\n",
					c.Date, c.Submitted, c.Uncertain, c.NotSubmitted, c.OwnTurn, c.Absorbed, c.Unknown, unknownCauseWords(c.UnknownCauses))
			}
			for _, m := range a.Publish.Members {
				fmt.Fprintf(&output, "  publish for @%s in #%s (binding %d): %s\n", m.Alias, m.Channel, m.BindingID, publishWords(m.State))
			}
		}
		if a.UnverifiedLaunches != nil {
			fmt.Fprintf(&output, "  %d live launches with unverified version or mode\n", *a.UnverifiedLaunches)
		}
		if a.UnseenStarts != nil && *a.UnseenStarts > 0 {
			fmt.Fprintf(&output, "  live members without a notice route because their session start was not seen: %d\n", *a.UnseenStarts)
		}
		if len(a.Evidence) == 0 {
			fmt.Fprintln(&output, "  capability evidence unknown")
		}
		for _, e := range a.Evidence {
			fmt.Fprintf(&output, "  evidence %q %q %q %q — %s %s\n", e.Version, e.Platform, e.Mode, e.ManifestRev, e.Capability, e.Level)
		}
		if a.Hooks == nil {
			fmt.Fprintln(&output, "  hook counters unknown")
		} else {
			fmt.Fprintf(&output, "  hooks %s UTC — p95 %d ms", a.Hooks.Date, a.Hooks.P95MS)
			for _, outcome := range slices.Sorted(maps.Keys(a.Hooks.Counts)) {
				fmt.Fprintf(&output, " %s=%d", outcome, a.Hooks.Counts[outcome])
			}
			fmt.Fprintln(&output)
			fmt.Fprintf(&output, "  recorded starts %d, helper timeouts %d, missed deadlines %d, vendor timeouts unknown\n",
				a.Hooks.RecordedStarts, a.Hooks.HelperTimeouts, a.Hooks.MissedDeadlines)
		}
	}
	_, err := io.WriteString(w, output.String())
	return err
}

// notWokenBanner names, above the channels, the members holding unread
// requests that nothing will tell them about, so the owner finds stuck
// work without reading each member's line.
// approvalLines names each member whose turn waits on the owner's own
// approval: no message to the member can move it.
func approvalLines(r domain.OwnerWhoResult) []string {
	var lines []string
	for _, ch := range r.Channels {
		for _, m := range ch.Members {
			if m.Observed == nil || !m.Observed.ApprovalOpen {
				continue
			}
			line := fmt.Sprintf("@%s in #%s waits on your approval (requested %s ago)", m.Alias, ch.Name, age(m.Observed.AgeS))
			if !m.Observed.Current {
				line = fmt.Sprintf("@%s in #%s asked you for approval %s ago, no answer reported", m.Alias, ch.Name, age(m.Observed.AgeS))
			}
			if len(m.OpenRequests) > 0 {
				line += "; open requests:"
				for _, id := range m.OpenRequests {
					line += fmt.Sprintf(" #%d", id)
				}
			}
			lines = append(lines, line+".")
		}
	}
	return lines
}

func notWokenBanner(r domain.OwnerWhoResult) string {
	total := 0
	var members []string
	for _, ch := range r.Channels {
		for _, m := range ch.Members {
			stuck := m.UnreadNotWoken
			if stuck == nil {
				continue
			}
			total += stuck.Requests
			name := fmt.Sprintf("@%s %d", m.Alias, stuck.Requests)
			if stuck.Requests > 1 {
				name += " (oldest " + age(stuck.OldestAgeS) + ")"
			}
			members = append(members, name)
		}
	}
	switch total {
	case 0:
		return ""
	case 1:
		return "1 request waits on a member the hall cannot wake: " + members[0] + "."
	}
	return fmt.Sprintf("%d requests wait on members the hall cannot wake: %s.", total, strings.Join(members, ", "))
}

// unknownCauseWords lists the causes of the day's unknown receipts that
// occurred, in a fixed order; empty when there were none.
func unknownCauseWords(c store.DoctorUnknownCauses) string {
	var parts []string
	for _, cause := range []struct {
		n    int
		name string
	}{
		{c.OpenPlaceholder, "open placeholder"},
		{c.UnfinishedTurn, "unfinished turn"},
		{c.ActivitySinceStop, "activity since stop"},
		{c.NoTurnButFacts, "no turn but facts"},
		{c.NotRecorded, "cause not recorded"},
	} {
		if cause.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", cause.n, cause.name))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

// takeoverLines reports each open takeover request with the command that
// grants it, or, once granted, how long it waits for the session.
func takeoverLines(takeovers []domain.OwnerTakeoverLine) []string {
	var lines []string
	for _, t := range takeovers {
		end := "session ended " + clock(t.EndedAtMS)
		if !t.Verified {
			end += " (unverified)"
		}
		if t.LastSeenMS != 0 {
			end += ", last seen " + clock(t.LastSeenMS)
		}
		owes := ""
		if len(t.Owes) > 0 {
			owes = "; owes"
			for _, id := range t.Owes {
				owes += fmt.Sprintf(" #%d", id)
			}
		}
		line := fmt.Sprintf("takeover %d: #%s/@%s (binding %d), %s%s. Asked by %s session sha256:%s at %s.",
			t.ID, t.Channel, t.Alias, t.BindingID, end, owes, t.Vendor, t.Session, clock(t.AskedAtMS))
		if t.Granted {
			line += fmt.Sprintf(" Granted; it completes when that session runs crosshall join %s --as %s --takeover again before %s.",
				t.Channel, t.Alias, clock(t.ExpiresAtMS))
		} else {
			line += fmt.Sprintf(" crosshall owner takeover %d", t.ID)
		}
		lines = append(lines, line)
	}
	return lines
}

// endedLines reports the live members whose session ended over 12 hours
// ago: one line for those with no open request, with the command that
// departs them, and one per member that still has some.
func endedHow(m domain.EndedMember) string {
	if m.NoStart {
		return "no session start recorded, last seen"
	}
	return "session ended"
}

func endedLines(ended []domain.EndedMember) []string {
	var free []string
	var lines []string
	for _, m := range ended {
		if len(m.OpenRequests) == 0 {
			name := fmt.Sprintf("@%s in #%s", m.Alias, m.Channel)
			if m.NoStart {
				name += " (no session start recorded)"
			}
			free = append(free, name)
			continue
		}
		line := fmt.Sprintf("@%s in #%s: %s %s, over 12 hours ago; open requests:", m.Alias, m.Channel, endedHow(m), clock(m.EndedAtMS))
		for _, id := range m.OpenRequests {
			line += fmt.Sprintf(" #%d", id)
		}
		lines = append(lines, line+".")
	}
	if len(free) > 0 {
		lines = append([]string{fmt.Sprintf("Sessions ended, or with no recorded start silent, over 12 hours ago; no open requests: %s. crosshall owner depart --ended departs them.",
			strings.Join(free, ", "))}, lines...)
	}
	return lines
}

// startOwedLine asks the started dispatcher who owes replies, for the one
// line owner start prints. An older dispatcher, or any failure, prints none.
func startOwedLine(ctx context.Context, env Env) string {
	if env.Transport == nil {
		return ""
	}
	result, err := env.Transport.Do(ctx, Call{Owner: true, Command: domain.OwnerOwed{}})
	if err != nil || result.Status != core.ExitOK {
		return ""
	}
	var r domain.OwnerOwedResult
	if json.Unmarshal(result.Data, &r) != nil {
		return ""
	}
	return owedLine(r.Members)
}

// owedLine sums up the members that owe replies at a start: how many get
// a resume notice when their turn is free, and who nothing can wake.
func owedLine(members []domain.OwingMember) string {
	if len(members) == 0 {
		return ""
	}
	owed, woken := 0, 0
	var cannot []string
	for _, m := range members {
		owed += m.Owed
		if m.CanResume() {
			woken++
		} else {
			cannot = append(cannot, fmt.Sprintf("@%s in #%s: %s", m.Alias, m.Channel, whyText(m.Wake, false)))
		}
	}
	who, what := "members owe", "requests"
	if len(members) == 1 {
		who = "member owes"
	}
	if owed == 1 {
		what = "request"
	}
	line := fmt.Sprintf("%d %s %d open %s", len(members), who, owed, what)
	var parts []string
	if woken > 0 {
		parts = append(parts, fmt.Sprintf("%d can be woken and get one resume notice when their turn is free", woken))
	}
	if len(cannot) > 0 {
		parts = append(parts, fmt.Sprintf("%d cannot be woken (%s)", len(cannot), strings.Join(cannot, ", ")))
	}
	return line + ": " + strings.Join(parts, "; ") + "."
}
