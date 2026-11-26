package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/BeomSeogKim/crosshall/internal/domain"
)

// publishWords is a member's publish state as who, doctor and the send
// result show it.
func publishWords(p domain.PublishStatus) string {
	if p.Reason == "" && p.Notice == 0 && p.AtMS == 0 && p.UntilMS == 0 {
		return p.State // an agent's view: the state word only
	}
	switch p.State {
	case "held":
		return fmt.Sprintf("held: notice %d unobserved since %s", p.Notice, clock(p.AtMS))
	case "capped":
		return "capped until " + clock(p.UntilMS)
	case "suspended":
		words := "suspended: " + p.Reason
		if p.AtMS != 0 {
			words += " at " + clock(p.AtMS)
		}
		if p.UntilMS != 0 {
			words += " until " + clock(p.UntilMS)
		}
		if p.Lift != "" {
			words += "; " + p.Lift
		}
		return words
	}
	if p.Reason != "" {
		return fmt.Sprintf("%s (%s)", p.State, p.Reason)
	}
	return p.State
}

func renderPublishSwitch(data []byte, w io.Writer) error {
	var r domain.PublishSwitchResult
	if err := json.Unmarshal(data, &r); err != nil {
		return err
	}
	subject := "the hall"
	if !r.Hall {
		subject = fmt.Sprintf("@%s in #%s (binding %d)", r.Alias, r.Channel, r.BindingID)
	}
	action := "Publish switch " + r.Switch
	if !r.Changed {
		action += ", unchanged"
	}
	if r.Switch == "resume" {
		action = "Resume recorded" // the state line below says what follows
	}
	fmt.Fprintf(w, "%s for %s.\n", action, subject)
	if r.BuildStopLifted {
		fmt.Fprintln(w, "  lifted the publish stop for this build across all members")
	}
	if r.ReleasedNotice != 0 {
		fmt.Fprintf(w, "  released notice %d, which had no receipt\n", r.ReleasedNotice)
	}
	if r.Publish != nil {
		fmt.Fprintf(w, "  publish: %s\n", publishWords(*r.Publish))
	}
	return nil
}

// noticeText shows a publish notice's submission and observation; pickup is
// the request's own state. Submitted is the recorded write, not the fence.
func noticeText(label string, n domain.LedgerNotice) string {
	text := fmt.Sprintf("%s %d ", label, n.ID)
	switch {
	case n.State == "reserved":
		text += "not yet submitted"
	case n.State == "submitting" && n.SubmittingAtMS != nil:
		text += "being submitted since " + ledgerTime(*n.SubmittingAtMS)
	case n.ResolvedAtMS == nil:
		text += strings.ReplaceAll(n.State, "_", " ")
	case n.State == "uncertain":
		text += "submission uncertain since " + ledgerTime(*n.ResolvedAtMS)
	default:
		text += strings.ReplaceAll(n.State, "_", " ") + " " + ledgerTime(*n.ResolvedAtMS)
	}
	if n.ObservedAtMS != nil {
		text += fmt.Sprintf(", observed %s (%s)", ledgerTime(*n.ObservedAtMS), strings.ReplaceAll(n.Observation, "_", " "))
	} else if n.State == "submitted" || n.State == "uncertain" {
		text += ", not observed"
	}
	return text
}

// wakeText says what will tell a member about new work: a notice, its
// running wait, or nothing and why. note marks a message that is a note.
func wakeText(w domain.Wake, note bool) string {
	switch w.Wake {
	case domain.WakeNotice:
		return "notice queued"
	case domain.WakeWait:
		return "its wait is running"
	}
	text := "not woken: " + whyText(w, note)
	switch w.SeenAt {
	case domain.SeenAtTurnEnd:
		text += "; it sees this when its turn ends"
	case domain.SeenAtPrompt:
		text += "; it sees this at its next prompt"
	case domain.SeenAtCommand:
		text += "; it sees this at its next crosshall command"
	default:
		if w.Why == domain.WhyTurnOpen {
			text += "; the hall checks again after its turn"
		}
	}
	return text
}

// whyText is the fixed sentence of a why word.
func whyText(w domain.Wake, note bool) string {
	until := ""
	if w.UntilMS != 0 {
		until = " until " + clock(w.UntilMS)
	}
	switch w.Why {
	case domain.WhyNoRoute:
		if w.Adapter != "" {
			return "no notice route for " + w.Adapter
		}
		return "no notice route"
	case domain.WhyUnproven:
		return "notices not proven for its build"
	case domain.WhyDeviceOffline:
		return "device offline"
	case domain.WhySuspended:
		return "notices suspended" + until
	case domain.WhyOff:
		return "notices off"
	case domain.WhyEnded:
		return "its session ended (it may be closed)"
	case domain.WhyCapped:
		return "notice limit" + until
	case domain.WhyHeld:
		return "an earlier notice is unobserved"
	case domain.WhyTurnOpen:
		return "a turn is running"
	case domain.WhyApproval:
		return "its turn waits on its owner's approval (asking again cannot help)"
	case domain.WhyKind:
		if note {
			return "a note starts no turn"
		}
		return "this message starts no turn"
	}
	return w.Why
}

// memberWakeText is a member's reach as who shows it.
func memberWakeText(w domain.Wake) string {
	switch w.Wake {
	case domain.WakeNotice:
		return "woken by a notice"
	case domain.WakeWait:
		return "woken by its wait"
	}
	return "not woken: " + whyText(w, false)
}

// notWokenLine is the one next: line after a request or a final reply that
// nothing will wake some recipient for, or "" when every one is woken or
// sees it when its running turn ends.
func notWokenLine(ds []domain.Delivery) string {
	var aliases []string
	for _, d := range ds {
		if d.State == "stored" && d.Wake.NeedsManualWake() {
			aliases = append(aliases, "@"+d.Alias)
		}
	}
	if len(aliases) == 0 {
		return ""
	}
	who := aliases[len(aliases)-1]
	if len(aliases) > 1 {
		who = strings.Join(aliases[:len(aliases)-1], ", ") + " or " + who
	}
	return "next: nothing will wake " + who + ". Ask a member the hall can wake (crosshall who), or tell your user."
}
