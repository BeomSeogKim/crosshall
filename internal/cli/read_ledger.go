package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
)

func readLedgerMember(ctx context.Context, env Env, opts options, verb string, args []string, stdout, stderr io.Writer) core.ExitCode {
	o := outputs{json: opts.json, stdout: stdout, stderr: stderr}
	call := Call{}
	switch verb {
	case domain.VerbRead:
		ids := make([]int64, 0, len(args))
		for _, arg := range args {
			id, err := parsePositive(strings.TrimPrefix(arg, "#"))
			if err != nil {
				return o.usage("read expects message IDs")
			}
			ids = append(ids, id)
		}
		call = Call{Command: domain.Read{IDs: ids}, OpKey: opKey(env, opts)}
	case domain.VerbInbox, domain.VerbWaiting, domain.VerbStatus:
		if len(args) != 0 {
			return o.usage(verb + " takes no arguments")
		}
		switch verb {
		case domain.VerbInbox:
			call.Command = domain.Inbox{}
		case domain.VerbWaiting:
			call.Command = domain.Waiting{}
		case domain.VerbStatus:
			call.Command = domain.Status{}
		}
	}
	return send(ctx, env, opts, call, stdout, stderr)
}

func readLedgerOwner(ctx context.Context, env Env, opts options, args []string, stdout, stderr io.Writer) core.ExitCode {
	o := outputs{json: opts.json, stdout: stdout, stderr: stderr}
	call, err := ledgerOwnerCall(args)
	if err != nil {
		return o.usage(err.Error())
	}
	if _, ok := call.Command.(domain.OwnerStatus); !ok {
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

func ledgerOwnerCall(args []string) (Call, error) {
	call := Call{Owner: true}
	switch args[0] {
	case "restore":
		return restoreCall(args[1:])
	case "depart":
		usage := fmt.Errorf("usage: crosshall owner depart <binding-id> --seen <token> (get the binding ID and token from crosshall owner who --all)")
		if len(args) < 3 {
			return Call{}, usage
		}
		binding, err := parsePositive(args[1])
		token, last, found, flagErr := flagValue(args, 2, "--seen")
		if err != nil || flagErr != nil || !found || last != len(args)-1 || !domain.ValidDepartureToken(token) {
			return Call{}, usage
		}
		call.Command = domain.OwnerDepart{BindingID: binding, Seen: token}
		return call, nil
	case "status":
		if len(args) == 1 || (len(args) == 2 && args[1] == "--all") {
			call.Command = domain.OwnerStatus{}
			return call, nil
		}
		value, last, found, err := flagValue(args, 1, "--channel")
		if name := strings.TrimPrefix(value, "#"); found && err == nil && last == len(args)-1 && domain.ValidName(name) {
			call.Command = domain.OwnerStatus{Channel: name}
			return call, nil
		}
		return Call{}, fmt.Errorf("usage: crosshall owner status [--all | --channel <channel>]")
	case "cancel":
		if len(args) == 2 {
			ch, target, ok := ledgerTarget(args[1])
			id, err := parsePositive(strings.TrimPrefix(target, "#"))
			if ok && err == nil {
				call.Command = domain.Cancel{Channel: ch, ID: id}
				return call, nil
			}
		}
		return Call{}, fmt.Errorf("usage: crosshall owner cancel <channel>/<request-id>")
	case "budget":
		if len(args) >= 4 && args[1] == "grant" {
			ch, target, ok := ledgerTarget(args[2])
			if ok && len(args) == 4 && strings.HasPrefix(target, "x") {
				id, idErr := parsePositive(target[1:])
				units, unitsErr := parsePositive(args[3])
				if idErr == nil && unitsErr == nil {
					call.Command = domain.BudgetGrant{Channel: ch, ExchangeID: id, Units: units}
					return call, nil
				}
			}
			if alias, member := strings.CutPrefix(target, "@"); ok && member && domain.ValidName(alias) {
				value, last, found, err := flagValue(args, 3, "--roots")
				roots, countErr := parsePositive(value)
				if found && err == nil && countErr == nil && last == len(args)-1 {
					call.Command = domain.BudgetGrant{Channel: ch, Alias: alias, Roots: roots}
					return call, nil
				}
			}
		}
		return Call{}, fmt.Errorf("usage: crosshall owner budget grant <channel>/x<id> <units> | <channel>/@<alias> --roots <count>")
	}
	return Call{}, fmt.Errorf("unknown owner command %q", args[0])
}

func ledgerTarget(arg string) (string, string, bool) {
	ch, target, found := strings.Cut(arg, "/")
	ch = strings.TrimPrefix(ch, "#")
	return ch, target, found && domain.ValidName(ch) && target != ""
}

func parsePositive(arg string) (int64, error) {
	n, err := strconv.ParseInt(arg, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("expected a positive integer")
	}
	return n, nil
}

func renderReadLedger(cmd domain.Command, data []byte, w io.Writer) error {
	switch cmd.(type) {
	case domain.Read:
		var r domain.ReadResult
		if err := json.Unmarshal(data, &r); err != nil {
			return err
		}
		if len(r.Messages) == 0 {
			fmt.Fprintln(w, "No unread messages.")
		}
		for _, message := range r.Messages {
			printReadMessage(w, message)
		}
	case domain.Inbox, domain.Wait:
		var r waitResult
		if err := json.Unmarshal(data, &r); err != nil {
			return err
		}
		switch r.Reason {
		case "existing":
			fmt.Fprintln(w, "Already in your inbox")
		case "arrived":
			fmt.Fprintln(w, "Arrived during the wait")
		}
		if len(r.Unread) == 0 && len(r.Owed) == 0 && len(r.Cancelled) == 0 {
			fmt.Fprintln(w, "Inbox is empty. Run crosshall wait --existing --work-only --timeout 30s.")
		}
		printInboxItems(w, "Unread", r.Unread, false)
		printInboxItems(w, "Cancelled by the owner (no answer owed)", r.Cancelled, false)
		printInboxItems(w, "Open requests you have read", r.Owed, true)
	case domain.Waiting:
		var r domain.WaitingResult
		if err := json.Unmarshal(data, &r); err != nil {
			return err
		}
		if len(r.Requests) == 0 {
			fmt.Fprintln(w, "No outstanding requests.")
		}
		for _, req := range r.Requests {
			printLedgerRequest(w, req, false, r.GeneratedAtMS)
		}
	case domain.Status, domain.OwnerStatus:
		var r domain.StatusResult
		if err := json.Unmarshal(data, &r); err != nil {
			return err
		}
		if len(r.Channels) == 0 {
			fmt.Fprintln(w, "No channels.")
		}
		for _, ch := range r.Channels {
			fmt.Fprintf(w, "#%s\n", ch.Name)
			for _, binding := range ch.Bindings {
				fmt.Fprintf(w, "  @%s (%s, binding %d)\n", binding.Alias, binding.State, binding.ID)
			}
			for _, req := range ch.Requests {
				printLedgerRequest(w, req, true, r.GeneratedAtMS)
			}
			for _, ex := range ch.Exchanges {
				fmt.Fprintf(w, "  x%d: %d remaining (%d spent of %d)\n", ex.ID, ex.Remaining, ex.Spent, ex.Granted)
			}
			for _, event := range ch.BudgetExhaustions {
				fmt.Fprintf(w, "  x%d exhausted at %s (episode %d)\n", event.ExchangeID, ledgerTime(event.AtMS), event.Episode)
			}
		}
		if s := r.Self; s != nil {
			parts := make([]string, 0, len(s.Exchanges))
			for _, ex := range s.Exchanges {
				parts = append(parts, fmt.Sprintf("x%d (%d remaining)", ex.ID, ex.Remaining))
			}
			taking := "no exchange"
			if len(parts) > 0 {
				taking = strings.Join(parts, ", ")
			}
			fmt.Fprintf(w, "You (@%s): %d root token(s) to open a new exchange; taking part in %s.\n", s.Alias, s.RootTokens, taking)
		}
	case domain.Cancel:
		var r domain.CancelResult
		if err := json.Unmarshal(data, &r); err != nil {
			return err
		}
		if r.Cancelled == 0 {
			fmt.Fprintf(w, "#%d had no open obligations.\n", r.ID)
		} else {
			fmt.Fprintf(w, "#%d: cancelled %d open obligation(s).\n", r.ID, r.Cancelled)
		}
	case domain.BudgetGrant:
		var r domain.BudgetGrantResult
		if err := json.Unmarshal(data, &r); err != nil {
			return err
		}
		if r.ExchangeID != 0 {
			fmt.Fprintf(w, "#%s/x%d: added %d units, %d remaining.\n", r.Channel, r.ExchangeID, r.Units, r.Remaining)
		} else {
			fmt.Fprintf(w, "#%s/@%s: added %d root token(s), %d available.\n", r.Channel, r.Alias, r.Roots, r.RootTokens)
		}
	default:
		return fmt.Errorf("no plain form for %T", cmd)
	}
	return nil
}

func printReadMessage(w io.Writer, m domain.ReadMessage) {
	fmt.Fprintf(w, "#%d %s", m.ID, m.Kind)
	if m.Outcome != "" {
		fmt.Fprintf(w, " (%s)", strings.ReplaceAll(m.Outcome, "_", " "))
	}
	fmt.Fprintf(w, " from @%s in #%s", m.Sender, m.Channel)
	if m.InReplyTo != nil {
		fmt.Fprintf(w, " (to #%d)", *m.InReplyTo)
	}
	if m.DueAtMS != nil {
		fmt.Fprintf(w, ", due %s", ledgerTime(*m.DueAtMS))
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, m.Body)
	switch m.ObligationState {
	case "open":
		fmt.Fprintf(w, "Reply with crosshall reply %d \"result\", or use --more-time or --decline.\n", m.ID)
	case "cancelled":
		fmt.Fprintln(w, "Cancelled by the owner; no answer is owed.")
	}
	if len(m.ReplyIDs) != 0 {
		fmt.Fprintf(w, "Replies: %s\n", ledgerIDs(m.ReplyIDs))
	}
	for _, reply := range m.Replies {
		printReadMessage(w, reply)
	}
}

func printInboxItems(w io.Writer, title string, items []domain.InboxItem, showDue bool) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintln(w, title)
	for _, item := range items {
		fmt.Fprintf(w, "  #%d %s from @%s", item.ID, item.Kind, item.Sender)
		if showDue && item.DueAtMS != nil {
			fmt.Fprintf(w, ", due %s", ledgerTime(*item.DueAtMS))
		}
		fmt.Fprintln(w)
	}
}

// printLedgerRequest prints a request and its recipients. nowMS is when the
// ledger was generated: it gives the pickup age and the observed fact's age.
func printLedgerRequest(w io.Writer, r domain.LedgerRequest, full bool, nowMS int64) {
	fmt.Fprintf(w, "#%d from @%s, due %s (x%d)\n", r.ID, r.Sender, ledgerTime(r.DueAtMS), r.ExchangeID)
	for _, recipient := range r.Recipients {
		state := recipient.State
		if at := recipient.PickedUpAtMS; state == "open" && at != nil {
			state = "picked up " + ledgerTime(*at)
			if nowMS != 0 {
				state += " (" + age((nowMS-*at)/1000) + " ago)"
			}
		} else if state == "open" {
			state = "stored"
		}
		if recipient.ReplyUnread {
			state += ", unread reply"
		}
		if recipient.State == "open" && recipient.BindingState == "departed" {
			state += ", recipient departed"
		}
		fmt.Fprintf(w, "  @%s %s", recipient.Alias, state)
		if at := recipient.OverdueSinceMS; at != nil {
			fmt.Fprintf(w, ", overdue since %s", ledgerTime(*at))
		}
		if at := recipient.WasOverdueSinceMS; at != nil {
			fmt.Fprintf(w, ", was overdue since %s", ledgerTime(*at))
		}
		if late := recipient.LateByMS; late != nil {
			fmt.Fprintf(w, ", late by %s", lateness(*late))
		}
		if at := recipient.MoreTimeAtMS; at != nil {
			fmt.Fprintf(w, ", more time requested at %s", ledgerTime(*at))
		}
		if o := recipient.Observed; o != nil && nowMS != 0 {
			seen := o.At(nowMS)
			fmt.Fprintf(w, "; last observed: %s", observedText(&seen))
		}
		fmt.Fprintln(w)
		if full {
			for _, receipt := range recipient.Receipts {
				fmt.Fprintf(w, "    %s %s (%s)\n", receipt.Stage, ledgerTime(receipt.AtMS), receipt.Evidence)
			}
		}
	}
}

// ledgerTime uses the same local form as ask and reply.
func ledgerTime(ms int64) string { return clock(ms) }

func ledgerIDs(ids []int64) string {
	values := make([]string, len(ids))
	for i, id := range ids {
		values[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(values, " ")
}

type ledgerTrailer struct {
	Pending   []int64         `json:"pending"`
	OwedCount *int            `json:"owed_count"`
	Changes   []domain.Member `json:"roster_changes"`
	// JoinHint is repeated by the first command of a member a hook enrolled.
	JoinHint string `json:"join_hint"`
}

func printLedgerTrailer(w io.Writer, cmd domain.Command, trailer ledgerTrailer, success bool) {
	if _, isJoin := cmd.(domain.Join); !isJoin {
		if trailer.JoinHint != "" {
			fmt.Fprintln(w, trailer.JoinHint)
		}
		for _, m := range trailer.Changes {
			if m.Change == "departed" {
				fmt.Fprintf(w, "[crosshall] @%s departed\n", m.Alias)
				continue
			}
			if m.Change == "presence" && m.Observed != nil {
				fmt.Fprintf(w, "[crosshall] @%s %s\n", m.Alias, observedText(m.Observed))
				continue
			}
			focus := m.Focus
			if focus == "" {
				focus = "focus unset"
			}
			fmt.Fprintf(w, "[crosshall] @%s %s (%s, %s)\n", m.Alias, m.Change, m.Vendor, focus)
		}
	}
	_, inbox := cmd.(domain.Inbox)
	if !inbox && trailer.OwedCount != nil {
		_, left := cmd.(domain.Leave)
		_ = PrintInboxPointer(w, len(trailer.Pending), *trailer.OwedCount, left && success)
	}
}

// PrintInboxPointer is shared with whoami, which renders its own identity result.
func PrintInboxPointer(w io.Writer, unread, owed int, left bool) error {
	if left {
		_, err := fmt.Fprintf(w, "[crosshall] %d unread, %d owed. After rejoining, run crosshall inbox.\n", unread, owed)
		return err
	}
	_, err := fmt.Fprintf(w, "[crosshall] %d unread, %d owed. Run crosshall inbox.\n", unread, owed)
	return err
}
