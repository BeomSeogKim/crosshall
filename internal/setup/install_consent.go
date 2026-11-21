package setup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"

	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

const installConsentHeading = "Saying yes turns these on for the listed agents:"

type installDisclosure struct {
	store.InstallConsent
	known, claude bool
}

func (s Service) attachInstallDisclosure(ctx context.Context, targets []installTarget) {
	var profile wire.HookProfile
	for _, target := range targets {
		if target.Spec.Adapter == "claude" {
			b := target.Build
			profile = wire.HookProfile{Adapter: "claude", VendorVersion: b.Version, Platform: b.Platform, ManifestRev: b.Revision}
			if b.Version == unknown {
				profile.VendorVersion = "unverified"
			}
			break
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
		targets[i].disclosure = installDisclosure{InstallConsent: consent, known: err == nil, claude: targets[i].Spec.Adapter == "claude"}
	}
}

func groupInstallDisclosure(plans []VendorPlan) installDisclosure {
	disclosure := plans[0].disclosure
	for _, plan := range plans {
		if plan.disclosure.claude {
			return plan.disclosure
		}
	}
	return disclosure
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
	fmt.Fprintln(&text, "- Prompt line. A joined session sees one line with the numbers of its unread messages at its next prompt. It costs no model request.")
	switch {
	case !d.known:
		fmt.Fprintln(&text, "- Notices and the turn-end check. crosshall could not read its settings, so it cannot say here whether they are on for these builds. After installing, crosshall owner doctor shows it.")
	case !d.claude || !d.PublishCovered && !d.PublishStopped && !d.TurnEndCovered:
		fmt.Fprintln(&text, "- Notices and the turn-end check are not available for these builds. A session sees new work at its next prompt.")
	default:
		fmt.Fprintln(&text, d.notices())
		if d.TurnEndCovered {
			fmt.Fprintln(&text, "- Turn-end check (Claude Code). A turn that ends while a request is unread continues once. Each continuation costs one model request. Off: only with the adapter, crosshall owner adapter set claude --enabled off")
		} else {
			fmt.Fprintln(&text, "- Turn-end check. Not available for these builds.")
		}
	}
	_, err := io.WriteString(out, text.String())
	return err
}

func (d installDisclosure) notices() string {
	const prefix = "- Notices (Claude Code, interactive sessions). "
	switch {
	case d.PublishStopped:
		return prefix + "Stopped for this version after a notice went wrong. crosshall owner who --all shows how to resume."
	case !d.PublishCovered:
		return "- Notices. Not available for these builds: a session sees new work at its next prompt."
	case d.PublishSwitch == "hall_off":
		return prefix + "Off by your setting; installing does not change it. On: crosshall owner publish hall on"
	case d.PublishSwitch == "adapter_off":
		return prefix + "Off by your setting for Claude Code; installing does not change it. On: crosshall owner adapter set claude --publish on"
	}
	off := "crosshall owner publish hall off"
	if d.PublishSwitch == "adapter_on" {
		off = "crosshall owner adapter set claude --publish off"
	}
	line := prefix + "When work arrives for an idle session, the hall starts a turn in it. Each notice costs that session one model turn. Off: " + off
	if d.PublishProbation {
		line += " This version is new to crosshall: the first three notices are checked, and a bad one stops notices until you resume them."
	}
	return line
}
