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
				fields["roster_changes"] = trailer.Changes
			}
			writeJSON(stdout, fields)
		} else {
			fmt.Fprintf(stderr, "crosshall: %s\n", refusal.Message)
			if refusal.Hint != "" {
				fmt.Fprintf(stderr, "next: %s\n", refusal.Hint)
			}
			printLedgerTrailer(stderr, cmd, trailer)
		}
		return result.Status
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
	printLedgerTrailer(stdout, cmd, trailer)
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
		if r.Hint != "" {
			fmt.Fprintln(w, r.Hint)
		} else if r.Restored {
			fmt.Fprintf(w, "[crosshall] Already in #%s as @%s.\n", r.Channel, r.Alias)
		}
		if len(r.Members) > 0 {
			renderRoster(w, r.Channel, r.Topic, r.Members, r.Omitted)
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
	case domain.Who:
		var r domain.WhoResult
		if err := json.Unmarshal(data, &r); err != nil {
			return err
		}
		renderRoster(w, r.Channel, r.Topic, r.Members, 0)
	default:
		return renderReadLedger(cmd, data, w)
	}
	return nil
}

func renderRoster(w io.Writer, channel, topic string, members []domain.Member, omitted int) {
	title := "#" + channel
	if topic != "" {
		title += " - " + topic
	}
	fmt.Fprintln(w, title)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, m := range members {
		focus := m.Focus
		if focus == "" {
			focus = "focus unset"
		}
		alias := "@" + m.Alias
		if m.Self {
			alias += " (you)"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s, last command %s ago\t%s\n",
			alias, m.Vendor, focus, m.State, age(m.LastCommandAgoS), m.Route)
	}
	_ = tw.Flush()
	if omitted > 0 {
		fmt.Fprintf(w, "  ... %d more: crosshall who\n", omitted)
	}
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
