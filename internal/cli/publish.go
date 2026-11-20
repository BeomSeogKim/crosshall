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
