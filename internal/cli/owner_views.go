package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
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
				if member.EvidenceStale {
					state += ", stale"
				}
				route := member.Route
				if member.Publish != nil {
					route += "; publish: " + publishWords(*member.Publish)
				}
				fmt.Fprintf(w, "  @%s [binding %d] %s, %s, %s; %s, %s",
					member.Alias, member.BindingID, state, member.Vendor, focus, route, ownerEvidenceText(member))
				if member.Observed != nil {
					fmt.Fprintf(w, "; last observed: %s", observedText(member.Observed))
				}
				if len(member.OpenRequests) > 0 {
					fmt.Fprint(w, "; open requests:")
					for _, id := range member.OpenRequests {
						fmt.Fprintf(w, " #%d", id)
					}
				}
				if member.DepartToken != "" {
					fmt.Fprintf(w, "; depart token: %s", member.DepartToken)
					hasToken = true
				}
				fmt.Fprintln(w)
			}
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
	for _, check := range result.Checks {
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
	if result.Hint != "" {
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
				status = fmt.Sprintf("off by the owner (on: crosshall owner adapter set %s --turn-end on)", a.Adapter)
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
				fmt.Fprintf(&output, "  publish notices on %s UTC: %d submitted, %d uncertain, %d not submitted; observed %d own turn, %d absorbed, %d unknown\n",
					c.Date, c.Submitted, c.Uncertain, c.NotSubmitted, c.OwnTurn, c.Absorbed, c.Unknown)
			}
			for _, m := range a.Publish.Members {
				fmt.Fprintf(&output, "  publish for @%s in #%s (binding %d): %s\n", m.Alias, m.Channel, m.BindingID, publishWords(m.State))
			}
		}
		if a.UnverifiedLaunches != nil {
			fmt.Fprintf(&output, "  %d live launches with unverified version or mode\n", *a.UnverifiedLaunches)
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
