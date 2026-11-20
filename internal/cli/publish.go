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

// publishReason says briefly why a stored message is not published.
func publishReason(p domain.PublishStatus) string {
	switch p.State {
	case "off", "candidate":
		return "publish " + p.State + ": " + p.Reason
	}
	p.Lift = ""
	return "publish " + publishWords(p)
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
	if r.Switch == "resume" {
		action = "Publish suspension lifted"
	}
	if !r.Changed {
		action += ", unchanged"
	}
	fmt.Fprintf(w, "%s for %s.\n", action, subject)
	if r.Publish != nil {
		fmt.Fprintf(w, "  publish: %s\n", publishWords(*r.Publish))
	}
	return nil
}

// noticeText shows a publish notice's submission and observation; pickup is
// the request's own state.
func noticeText(n domain.LedgerNotice) string {
	text := fmt.Sprintf("notice %d ", n.ID)
	switch {
	case n.State == "reserved":
		text += "not yet submitted"
	case n.SubmittedAtMS == nil:
		text += n.State
	case n.State == "submitting":
		text += "being submitted since " + ledgerTime(*n.SubmittedAtMS)
	case n.State == "uncertain":
		text += "submission uncertain since " + ledgerTime(*n.SubmittedAtMS)
	default:
		text += n.State + " " + ledgerTime(*n.SubmittedAtMS)
	}
	if n.ObservedAtMS != nil {
		text += fmt.Sprintf(", observed %s (%s)", ledgerTime(*n.ObservedAtMS), strings.ReplaceAll(n.Observation, "_", " "))
	} else if n.SubmittedAtMS != nil {
		text += ", not observed"
	}
	return text
}
