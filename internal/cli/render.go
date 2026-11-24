package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
)

// render prints a result in plain or JSON form and returns its exit status.
func render(opts options, cmd domain.Command, result core.Result, stdout, stderr io.Writer) core.ExitCode {
	var trailer ledgerTrailer
	_ = json.Unmarshal(result.Data, &trailer)
	if result.Status != core.ExitOK {
		var refusal domain.Refusal
		_ = json.Unmarshal(result.Data, &refusal)
		if refusal.Message == "" {
			refusal.Message = string(result.Code)
		}
		if opts.json {
			// Keep any structured detail the refusal carries (for example
			// invalid_targets), then set the common fields.
			fields := map[string]any{}
			_ = json.Unmarshal(result.Data, &fields)
			fields["v"], fields["ok"], fields["code"] = 1, false, result.Code
			fields["message"], fields["hint"] = refusal.Message, refusal.Hint
			delete(fields, "pending")
			delete(fields, "roster_changes")
			if trailer.Pending != nil {
				fields["pending"] = trailer.Pending
			}
			if trailer.Changes != nil {
				fields["roster_changes"] = trailer.Changes
			}
			writeJSON(stdout, fields)
		} else {
			fmt.Fprintf(stderr, "crosshall: %s\n", refusal.Message)
			// The status line names the pickup itself when something is unread.
			readInTrailer := trailer.OwedCount != nil && len(trailer.Pending) > 0 && refusal.Hint == "crosshall read"
			if refusal.Hint != "" && !readInTrailer {
				fmt.Fprintf(stderr, "next: %s\n", refusal.Hint)
			} else if refusal.Hint == "" && trailer.Delivers {
				fmt.Fprintln(stderr, domain.HallDelivers)
			}
			printLedgerTrailer(stderr, cmd, trailer, false)
		}
		return result.Status
	}
	if _, ok := cmd.(domain.CreateChannel); ok {
		var channel domain.ChannelResult
		if json.Unmarshal(result.Data, &channel) == nil && channel.PoolIgnored {
			fmt.Fprintln(stderr, "crosshall: --pool was ignored.")
		}
	}
	if opts.json {
		fields := map[string]any{}
		if len(result.Data) > 0 {
			decoder := json.NewDecoder(bytes.NewReader(result.Data))
			decoder.UseNumber()
			if err := decoder.Decode(&fields); err != nil {
				fmt.Fprintf(stderr, "crosshall: malformed result: %v\n", err)
				return core.ExitError
			}
		}
		fields["v"], fields["ok"], fields["kind"] = 1, true, cmd.Verb()
		writeJSON(stdout, fields)
		return core.ExitOK
	}
	if err := renderPlain(cmd, result.Data, stdout); err != nil {
		fmt.Fprintf(stderr, "crosshall: malformed result: %v\n", err)
		return core.ExitError
	}
	printLedgerTrailer(stdout, cmd, trailer, true)
	return core.ExitOK
}

func writeJSON(w io.Writer, v any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	_, _ = w.Write(buf.Bytes())
}

func renderPlain(cmd domain.Command, data []byte, w io.Writer) error {
	switch cmd.(type) {
	case domain.AdapterSet:
		return renderAdapterPolicy(data, w)
	case domain.Publish, domain.OwnerPublish:
		return renderPublishSwitch(data, w)
	case domain.AdapterEvidence: // adapter-claude
		return renderAdapterEvidence(data, w)
	case domain.Restore:
		var r domain.RestoreResult
		if err := json.Unmarshal(data, &r); err != nil {
			return err
		}
		fmt.Fprintf(w, "Restored binding %d.\n", r.BindingID)
	case domain.OwnerDepart:
		var r domain.OwnerDepartResult
		if err := json.Unmarshal(data, &r); err != nil {
			return err
		}
		fmt.Fprintf(w, "@%s in #%s departed (binding %d).\n", r.Alias, r.Channel, r.BindingID)
	case domain.CreateChannel:
		var r domain.ChannelResult
		if err := json.Unmarshal(data, &r); err != nil {
			return err
		}
		fmt.Fprintf(w, "#%s created.\n", r.Channel)
	case domain.Join:
		var r domain.JoinResult
		if err := json.Unmarshal(data, &r); err != nil {
			return err
		}
		if r.Move != nil {
			fmt.Fprintf(w, "[crosshall] Work stays in #%s (%d unread, %d owed, %d waiting).\n",
				r.Move.Channel, r.Move.Unread, r.Move.Owed, r.Move.Waiting)
		}
		if r.Hint != "" {
			fmt.Fprintln(w, r.Hint)
		} else if r.Restored {
			fmt.Fprintf(w, "[crosshall] Already in #%s as @%s.\n", r.Channel, r.Alias)
		}
		if len(r.Members) > 0 {
			RenderRoster(w, r.Channel, r.Topic, r.Members, r.Omitted)
		}
		for _, m := range r.Changes {
			line := fmt.Sprintf("[crosshall] @%s %s", m.Alias, m.Change)
			if m.Change != "departed" {
				focus := m.Focus
				if focus == "" {
					focus = "focus unset"
				}
				line += fmt.Sprintf(" (%s, %s)", m.Vendor, focus)
			}
			fmt.Fprintln(w, line)
		}
	// ask-reply
	case domain.Ask:
		return renderAsk(data, w)
	case domain.Reply:
		return renderReply(data, w)
	// end ask-reply
	// note-log
	case domain.Note, domain.Log, domain.Leave, domain.Focus:
		return renderNoteLog(cmd, data, w)
	// end note-log
	case domain.OwnerWho, domain.OwnerLog, domain.ChannelDefault, domain.ChannelAuto:
		return renderOwnerViews(cmd, data, w)
	case domain.Who:
		var r domain.WhoResult
		if err := json.Unmarshal(data, &r); err != nil {
			return err
		}
		RenderRoster(w, r.Channel, r.Topic, r.Members, 0)
	default:
		return renderReadLedger(cmd, data, w)
	}
	return nil
}

// RenderRoster writes the same plain-text member list for CLI and hook enrollment.
func RenderRoster(w io.Writer, channel, topic string, members []domain.Member, omitted int) {
	title := "#" + channel
	if topic != "" {
		title += " - " + topic
	}
	fmt.Fprintln(w, title)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	unreported := false
	for _, m := range members {
		focus := m.Focus
		if focus == "" {
			focus = "focus unset"
		}
		alias := "@" + m.Alias
		if m.Self {
			alias += " (you)"
		}
		// An adapter's latest literal fact replaces the last-command column.
		// Without one nothing reports working or idle; that is the normal
		// case, so the plain roster omits the state word.
		state := m.State + ", "
		if m.State == "unknown" {
			state = ""
		}
		activity := state + "last command " + age(m.LastCommandAgoS) + " ago"
		if m.Observed != nil {
			activity = observedText(m.Observed)
		} else if m.State == "unknown" {
			unreported = true
		}
		route := m.Route
		if m.Wake.Wake != "" {
			route = memberWakeText(m.Wake)
		} else if m.Publish != nil {
			route += "; publish: " + publishWords(*m.Publish)
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n", alias, m.Vendor, focus, activity, route)
	}
	_ = tw.Flush()
	if unreported {
		fmt.Fprintln(w, "  Working or idle is not shown: no vendor adapter reports it. The time is the member's last crosshall command.")
	}
	if omitted > 0 {
		fmt.Fprintf(w, "  ... %d more: crosshall who\n", omitted)
	}
}

// factWords names an adapter fact as the roster shows it.
var factWords = map[string]string{
	domain.FactPromptSubmitted:   "prompt submitted",
	domain.FactTurnFinishing:     "turn finished",
	domain.FactApprovalRequested: "approval requested",
	domain.FactSessionEnded:      "session ended",
}

// observedText is "<fact> <age> ago", or "unknown (last: ...)" past its cap,
// labelled when the launch's version and mode were not established. An open
// approval says that it waits on the member's owner.
func observedText(o *domain.Observed) string {
	if o.ApprovalOpen {
		// An open approval does not age out: only the owner can answer it.
		return "approval requested " + age(o.AgeS) + " ago, waits on its owner"
	}
	words := factWords[o.Fact]
	if words == "" {
		words = o.Fact
	}
	text := words + " " + age(o.AgeS) + " ago"
	label := ""
	if !o.Verified {
		label = ", unverified"
	}
	if !o.Current {
		return "unknown (last: " + text + label + ")"
	}
	if label != "" {
		return text + " (unverified)"
	}
	return text
}

// lateness formats how late a reply was; under a second it keeps milliseconds.
func lateness(ms int64) string {
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	return age(ms / 1000)
}

func age(seconds int64) string {
	switch {
	case seconds < 60:
		return fmt.Sprintf("%ds", max(seconds, 0))
	case seconds < 3600:
		return fmt.Sprintf("%dm", seconds/60)
	case seconds < 86400:
		return fmt.Sprintf("%dh", seconds/3600)
	}
	return fmt.Sprintf("%dd", seconds/86400)
}
