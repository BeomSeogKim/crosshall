package setup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"slices"
	"strings"

	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/hook"
	"github.com/BeomSeogKim/crosshall/internal/hook/manifests"
	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

const installConsentHeading = "Saying yes turns these on for the listed agents:"

type installDisclosure struct {
	store.InstallConsent
	known, evidenced, prompt, unknownBuild bool
	// sharedStop names the CLIs whose shared hooks file holds another
	// group's Stop hooks.
	sharedStop []string
}

func (s Service) attachInstallDisclosure(ctx context.Context, targets []installTarget) {
	var profile wire.HookProfile
	for i, target := range targets {
		// Notices and the turn-end check are disclosed for an adapter with
		// built-in capability evidence, read for its build.
		evidenced := slices.ContainsFunc(domain.BuiltinCapabilityEvidence, func(e domain.BuiltinEvidence) bool {
			return e.Vendor == target.Spec.Adapter
		})
		targets[i].disclosure.evidenced = evidenced
		targets[i].disclosure.prompt = hook.HasPromptLine(target.Spec.Adapter, target.Build.Revision)
		if sharesStop(target) {
			targets[i].disclosure.sharedStop = []string{ident.Identity{Vendor: target.Spec.Adapter}.CLIName()}
		}
		if evidenced {
			b := target.Build
			profile = wire.HookProfile{Adapter: target.Spec.Adapter, VendorVersion: b.Version, Platform: b.Platform, ManifestRev: b.Revision}
			if b.Version == "" || b.Version == unknown {
				profile.VendorVersion = "unverified"
				targets[i].disclosure.unknownBuild = true
			}
		}
	}
	var files fs.FS
	state, err := spool.OpenServerRoot(s.StateRoot)
	if err == nil {
		defer state.Close()
		files = spool.ServerFiles{Root: state}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return
	}
	consent, err := store.InspectInstallConsent(ctx, files, profile, domain.BuiltinCapabilityEvidence)
	for i := range targets {
		targets[i].disclosure.InstallConsent = consent
		targets[i].disclosure.known = err == nil
	}
}

func groupInstallDisclosure(plans []VendorPlan) installDisclosure {
	var disclosure installDisclosure
	for _, plan := range plans {
		disclosure.include(plan.disclosure)
	}
	return disclosure
}

func (d *installDisclosure) include(other installDisclosure) {
	prompt := d.prompt || other.prompt
	shared := append(slices.Clone(d.sharedStop), other.sharedStop...)
	if other.evidenced || !d.evidenced && !d.known {
		*d = other
	}
	d.prompt, d.sharedStop = prompt, shared
}

// sharesStop reports whether crosshall installs a Stop hook into a hooks
// file where another group already has one. Antigravity runs no later Stop
// hook once one continues the turn (#328), so such a group can miss a Stop.
func sharesStop(target installTarget) bool {
	if installerFor(target.Spec.Adapter).hookGroup() == "" || !slices.Contains(target.Spec.Events, "Stop") {
		return false
	}
	doc, err := parseConfigJSON(target.Current.Bytes)
	if err != nil || doc == nil {
		return false
	}
	for _, group := range doc.fields {
		if group.key == installerFor(target.Spec.Adapter).hookGroup() {
			continue
		}
		if stop := group.value.field("Stop"); stop != nil && stop.kind == '[' && len(stop.items) > 0 {
			return true
		}
	}
	return false
}

func (d installDisclosure) write(out io.Writer) error {
	var text strings.Builder
	fmt.Fprintln(&text, installConsentHeading)
	switch {
	case !d.known:
		fmt.Fprintln(&text, "- Joining. A new session joins its repository's channel when it starts, in repositories that have one. Whether a repository without a channel gets one depends on your setting (crosshall owner channel auto on|off).")
	case d.AutoJoinOff:
		fmt.Fprintln(&text, "- Joining. A new session joins its repository's channel when it starts, in repositories that have one. You turned automatic joining off, so a repository without a channel gets none. On: crosshall owner channel auto on")
	default:
		fmt.Fprintln(&text, "- Joining. A new session joins its repository's channel when it starts. A Git repository with no channel gets one, named after its directory. Off for repositories without a channel: crosshall owner channel auto off. A repository that has a channel keeps joining its sessions while the agent's hooks are on.")
	}
	if d.prompt {
		fmt.Fprintln(&text, "- Prompt line ("+strings.Join(promptLineAgents(), ", ")+"). A joined session sees one line with the numbers of its unread messages at its next prompt, and crosshall read picks them up. The line itself costs no model request.")
	}
	pickup := "its next crosshall read."
	if d.prompt {
		pickup = "its next prompt or " + pickup
	}
	switch {
	case !d.known:
		fmt.Fprintln(&text, "- Notices and the turn-end check. crosshall could not read its settings, so it cannot say here whether they are on for these builds. After installing, crosshall owner doctor shows it.")
	case !d.evidenced || !d.unknownBuild && !d.PublishCovered && !d.PublishStopped && !d.TurnEndInteractive && !d.TurnEndPrint:
		fmt.Fprintln(&text, "- Notices and the turn-end check are not available for these builds. A session sees new work at "+pickup)
	default:
		fmt.Fprintln(&text, d.notices(pickup))
		fmt.Fprintln(&text, d.turnEnd())
	}
	if len(d.sharedStop) > 0 {
		fmt.Fprintln(&text, "- Other Stop hooks ("+strings.Join(d.sharedStop, ", ")+"). Another hook group in the same hooks file has Stop hooks. When the turn-end check continues a turn, this CLI may skip the other Stop hooks for that Stop; they run again at the turn's next Stop.")
	}
	_, err := io.WriteString(out, text.String())
	return err
}

func (d installDisclosure) notices(pickup string) string {
	const prefix = "- Notices (Claude Code, interactive sessions). "
	switch {
	case d.PublishStopped:
		return prefix + "Stopped for this version after a notice went wrong. crosshall owner who --all shows how to resume."
	case !d.PublishCovered && !d.unknownBuild:
		return "- Notices. Not available for these builds: a session sees new work at " + pickup
	case d.PublishSwitch == "hall_off":
		return prefix + "Off by your setting; installing does not change it. On: crosshall owner publish hall on"
	case d.PublishSwitch == "adapter_off":
		return prefix + "Off by your setting for Claude Code; installing does not change it. On: crosshall owner adapter set claude --publish on"
	}
	off := "crosshall owner publish hall off"
	if d.PublishSwitch == "adapter_on" {
		off = "crosshall owner adapter set claude --publish off"
	}
	if d.unknownBuild {
		return prefix + "crosshall could not read this Claude Code's version, so it cannot say here whether notices are available for it. If they are, the hall starts a turn in an idle session when work arrives, and each notice costs that session one model turn. Off: " + off
	}
	line := prefix + "When work arrives for an idle session, the hall starts a turn in it. Each notice costs that session one model turn. Off: " + off
	if d.PublishProbation {
		line += " This version is new to crosshall: the first three notices are checked, and a bad one stops notices until you resume them."
	}
	return line
}

func (d installDisclosure) turnEnd() string {
	const off = "Off: crosshall owner adapter set claude --turn-end off"
	covered := d.unknownBuild || d.TurnEndInteractive || d.TurnEndPrint
	if covered && d.TurnEndSwitch == "off" {
		// The owner's switch does not depend on the build; an uncovered
		// build stays "not available" whatever the switch says.
		return "- Turn-end check (Claude Code). Off by your setting; installing does not change it. On: crosshall owner adapter set claude --turn-end on"
	}
	if d.unknownBuild {
		return "- Turn-end check (Claude Code). crosshall could not read this Claude Code's version, so it cannot say here whether the check is available for it. If it is, a turn that ends while a request waits that is unread, unanswered and not yet announced to the session continues once, and each continuation costs one model request. " + off
	}
	var mode string
	switch {
	case d.TurnEndInteractive && d.TurnEndPrint:
		mode = "interactive sessions and print mode"
	case d.TurnEndInteractive:
		mode = "interactive sessions"
	case d.TurnEndPrint:
		mode = "print mode"
	default:
		return "- Turn-end check. Not available for these builds."
	}
	return "- Turn-end check (Claude Code, " + mode + "). When a turn ends while a request waits that is unread, unanswered and not yet announced to the session, the turn continues once. Each continuation costs one model request. " + off
}

// promptLineAgents names, in setup order, the agents whose hooks show the
// prompt line.
func promptLineAgents() []string {
	var names []string
	for _, adapter := range manifests.SetupOrder() {
		if caps, _ := manifests.CapabilitiesOf(adapter); caps.PromptLine {
			names = append(names, ident.Identity{Vendor: adapter}.CLIName())
		}
	}
	return names
}
