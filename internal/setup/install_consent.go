package setup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"

	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/hook"
	"github.com/BeomSeogKim/crosshall/internal/hook/manifests"
	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

const installConsentHeading = "Saying yes turns these on for the listed agents:"

type installDisclosure struct {
	known, inspectFailed, autoJoinOff, prompt bool
	adapters                                  []installAdapterDisclosure
}

type installAdapterDisclosure struct {
	store.InstallConsent
	adapter, name, publishMode   string
	unknownBuild, turnEndShipped bool
}

func (s Service) attachInstallDisclosure(ctx context.Context, targets []installTarget) {
	var files fs.FS
	state, openErr := spool.OpenServerRoot(s.StateRoot)
	if openErr == nil {
		defer state.Close()
		files = spool.ServerFiles{Root: state}
	}
	for i, target := range targets {
		d := &targets[i].disclosure
		*d = installDisclosure{prompt: hook.HasPromptLine(target.Spec.Adapter, target.Build.Revision)}
		if openErr != nil && !errors.Is(openErr, fs.ErrNotExist) {
			d.inspectFailed = true
			continue
		}
		adapter := installAdapterDisclosure{adapter: target.Spec.Adapter, name: manifests.DisplayName(target.Spec.Adapter)}
		for _, row := range domain.BuiltinCapabilityEvidence {
			if row.Vendor != adapter.adapter {
				continue
			}
			switch row.Capability {
			case "publish":
				adapter.publishMode = row.Mode
			case "turn_end":
				adapter.turnEndShipped = true
			}
		}
		b := target.Build
		profile := wire.HookProfile{Adapter: adapter.adapter, VendorVersion: b.Version, Platform: b.Platform, ManifestRev: b.Revision, Mode: adapter.publishMode}
		if b.Version == "" || b.Version == unknown {
			adapter.unknownBuild = true
			profile.VendorVersion = "unverified"
		}
		consent, err := store.InspectInstallConsent(ctx, files, profile, domain.BuiltinCapabilityEvidence)
		d.known, d.inspectFailed, d.autoJoinOff = err == nil, err != nil, consent.AutoJoinOff
		if err == nil && (adapter.publishMode != "" || adapter.turnEndShipped) {
			adapter.InstallConsent = consent
			d.adapters = append(d.adapters, adapter)
		}
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
	d.prompt = d.prompt || other.prompt
	d.inspectFailed = d.inspectFailed || other.inspectFailed
	if other.known {
		d.known, d.autoJoinOff = true, other.autoJoinOff
	}
	d.adapters = append(d.adapters, other.adapters...)
}

func (d installDisclosure) write(out io.Writer) error {
	var text strings.Builder
	known := d.known && !d.inspectFailed
	fmt.Fprintln(&text, installConsentHeading)
	switch {
	case !known:
		fmt.Fprintln(&text, "- Joining. A new session joins its repository's channel when it starts, in repositories that have one. Whether a repository without a channel gets one depends on your setting (crosshall owner channel auto on|off).")
	case d.autoJoinOff:
		fmt.Fprintln(&text, "- Joining. A new session joins its repository's channel when it starts, in repositories that have one. You turned automatic joining off, so a repository without a channel gets none. On: crosshall owner channel auto on")
	default:
		fmt.Fprintln(&text, "- Joining. A new session joins its repository's channel when it starts. A Git repository with no channel gets one, named after its directory. Off for repositories without a channel: crosshall owner channel auto off. A repository that has a channel keeps joining its sessions while the agent's hooks are on.")
	}
	if d.prompt {
		fmt.Fprintln(&text, "- Prompt line (Claude Code, Codex CLI). A joined session sees one line with the numbers of its unread messages at its next prompt, and crosshall read picks them up. The line itself costs no model request.")
	}
	pickup := "its next crosshall read."
	if d.prompt {
		pickup = "its next prompt or " + pickup
	}
	if !known {
		fmt.Fprintln(&text, "- Notices and the turn-end check. crosshall could not read its settings, so it cannot say here whether they are on for these builds. After installing, crosshall owner doctor shows it.")
	} else if !d.writeAdapterEffects(&text, pickup) {
		fmt.Fprintln(&text, "- Notices and the turn-end check are not available for these builds. A session sees new work at "+pickup)
	}
	_, err := io.WriteString(out, text.String())
	return err
}

func (d installDisclosure) writeAdapterEffects(text *strings.Builder, pickup string) bool {
	wrote := false
	for _, adapter := range d.adapters {
		notices := adapter.publishMode != ""
		turnEnd := adapter.TurnEndInteractive || adapter.TurnEndPrint || adapter.turnEndShipped && adapter.unknownBuild
		if !turnEnd && !(notices && (adapter.unknownBuild || adapter.PublishCovered || adapter.PublishStopped)) {
			continue
		}
		if notices {
			fmt.Fprintln(text, adapter.notices(pickup))
		}
		if turnEnd || adapter.turnEndShipped {
			fmt.Fprintln(text, adapter.turnEnd())
		}
		wrote = true
	}
	return wrote
}

func (d installAdapterDisclosure) notices(pickup string) string {
	mode := d.publishMode
	switch mode {
	case "interactive":
		mode = "interactive sessions"
	case "daemon":
		mode = "managed daemon sessions"
	}
	prefix := "- Notices (" + d.name + ", " + mode + "). "
	switch {
	case d.PublishStopped:
		return prefix + "Stopped for this version after a notice went wrong. crosshall owner who --all shows how to resume."
	case !d.PublishCovered && !d.unknownBuild:
		return prefix + "Not available for this build. A session sees new work at " + pickup
	case d.PublishSwitch == "hall_off":
		return prefix + "Off by your setting; installing does not change it. On: crosshall owner publish hall on"
	case d.PublishSwitch == "adapter_off":
		return prefix + "Off by your setting for " + d.name + "; installing does not change it. On: crosshall owner adapter set " + d.adapter + " --publish on"
	}
	off := "crosshall owner publish hall off"
	if d.PublishSwitch == "adapter_on" {
		off = "crosshall owner adapter set " + d.adapter + " --publish off"
	}
	if d.unknownBuild {
		return prefix + "crosshall could not read this " + d.name + "'s version, so it cannot say here whether notices are available for it. If they are, the hall starts a turn in an idle session when work arrives, and each notice costs that session one model turn. Off: " + off
	}
	line := prefix + "When work arrives for an idle session, the hall starts a turn in it. Each notice costs that session one model turn. Off: " + off
	if d.PublishProbation {
		line += " This version is new to crosshall: the first three notices are checked, and a bad one stops notices until you resume them."
	}
	return line
}

func (d installAdapterDisclosure) turnEnd() string {
	prefix := "- Turn-end check (" + d.name
	off := "Off: crosshall owner adapter set " + d.adapter + " --turn-end off"
	covered := d.unknownBuild && d.turnEndShipped || d.TurnEndInteractive || d.TurnEndPrint
	if covered && d.TurnEndSwitch == "off" {
		// The owner's switch does not depend on the build; an uncovered
		// build stays "not available" whatever the switch says.
		return prefix + "). Off by your setting; installing does not change it. On: crosshall owner adapter set " + d.adapter + " --turn-end on"
	}
	if d.unknownBuild && d.turnEndShipped {
		return prefix + "). crosshall could not read this " + d.name + "'s version, so it cannot say here whether the check is available for it. If it is, a turn that ends while a request waits that is unread, unanswered and not yet announced to the session continues once, and each continuation costs one model request. " + off
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
		return prefix + "). Not available for this build."
	}
	return prefix + ", " + mode + "). When a turn ends while a request waits that is unread, unanswered and not yet announced to the session, the turn continues once. Each continuation costs one model request. " + off
}
