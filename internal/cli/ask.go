package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
)

// bodySource collects a message body from exactly one of: a positional
// argument, "-" (standard input) or --file.
type bodySource struct {
	text, file string
	stdin, set bool
}

var errRepeatedBody = errors.New("give the text once: as an argument, as - for standard input, or with --file")

func (b *bodySource) add(kind, value string) error {
	if b.set {
		return errRepeatedBody
	}
	b.set = true
	switch kind {
	case "arg":
		b.text = value
	case "stdin":
		b.stdin = true
	case "file":
		b.file = value
	}
	return nil
}

func (b bodySource) read(env Env) (string, error) {
	var data []byte
	var err error
	switch {
	case b.stdin:
		if env.Stdin == nil {
			return "", fmt.Errorf("no standard input")
		}
		data, err = io.ReadAll(io.LimitReader(env.Stdin, domain.MaxBody+1))
	case b.file != "":
		var f *os.File
		if f, err = os.Open(b.file); err == nil {
			data, err = io.ReadAll(io.LimitReader(f, domain.MaxBody+1))
			f.Close()
		}
	default:
		return b.text, validBody(b.text)
	}
	if err != nil {
		return "", err
	}
	if err := validBody(string(data)); err != nil {
		return "", err
	}
	if len(data) > domain.MaxBody {
		return "", fmt.Errorf("the text is longer than %d bytes; point at a file instead of pasting it", domain.MaxBody)
	}
	// The bytes are kept exactly, so the operation key covers what was sent.
	return string(data), nil
}

// validBody refuses text that is not valid UTF-8: JSON encoding would
// silently replace the bad bytes, and two different inputs would look equal.
func validBody(s string) error {
	if !utf8.ValidString(s) {
		return fmt.Errorf("the text is not valid UTF-8")
	}
	return nil
}

// messageID reads "42" or "#42".
func messageID(s string) (int64, error) {
	n, err := strconv.ParseInt(strings.TrimPrefix(s, "#"), 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%q is not a message number", s)
	}
	return n, nil
}

func parseAsk(args []string) (domain.Ask, bodySource, error) {
	var cmd domain.Ask
	var body bodySource
	// Recipients can follow the text. Finish parsing them before explaining
	// whether an extra body was a bare name or a second piece of text.
	var bodyErr error
	addBody := func(kind, value string) {
		if err := body.add(kind, value); err != nil {
			bodyErr = err
		}
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			// Everything after -- is the text, even if it starts with @ or -.
			rest := args[i+1:]
			if len(rest) != 1 {
				return cmd, body, fmt.Errorf("give the text as one argument after --")
			}
			addBody("arg", rest[0])
			break
		}
		if v, j, ok, err := flagValue(args, i, "--due"); ok {
			if err != nil {
				return cmd, body, err
			}
			cmd.Due, i = v, j
			continue
		}
		if v, j, ok, err := flagValue(args, i, "--re"); ok {
			if err != nil {
				return cmd, body, err
			}
			if cmd.Re, err = messageID(v); err != nil {
				return cmd, body, err
			}
			i = j
			continue
		}
		if v, j, ok, err := flagValue(args, i, "--file"); ok {
			if err != nil {
				return cmd, body, err
			}
			addBody("file", v)
			i = j
			continue
		}
		switch {
		case a == "--all":
			cmd.All = true
		case a == "--each":
			cmd.Each = true
		case a == "--new":
			cmd.New = true
		case a == "-":
			addBody("stdin", "")
		case strings.HasPrefix(a, "@") && domain.ValidName(a[1:]):
			// Only a whole, valid @alias names a recipient; "@tax, ready?"
			// is text.
			cmd.To = append(cmd.To, a[1:])
		case strings.HasPrefix(a, "--"):
			return cmd, body, fmt.Errorf("unknown option %q for ask", a)
		default:
			addBody("arg", a)
		}
	}
	if bodyErr != nil {
		return cmd, body, bodyErr
	}
	switch {
	case len(cmd.To) == 0 && !cmd.All:
		return cmd, body, fmt.Errorf("usage: crosshall ask @alias [@alias ...] \"...\" (or --all --each)")
	case !body.set:
		return cmd, body, fmt.Errorf("the request has no text")
	}
	if _, err := domain.ParseDue(cmd.Due); err != nil {
		return cmd, body, err
	}
	return cmd, body, nil
}

func parseReply(args []string) (domain.Reply, bodySource, error) {
	var cmd domain.Reply
	var body bodySource
	for i := 0; i < len(args); i++ {
		a := args[i]
		if v, j, ok, err := flagValue(args, i, "--file"); ok {
			if err != nil {
				return cmd, body, err
			}
			if err := body.add("file", v); err != nil {
				return cmd, body, err
			}
			i = j
			continue
		}
		switch {
		case a == "--decline" || a == "--more-time":
			if cmd.Outcome != "" {
				return cmd, body, fmt.Errorf("--decline and --more-time cannot be combined")
			}
			cmd.Outcome = domain.OutcomeDecline
			if a == "--more-time" {
				cmd.Outcome = domain.OutcomeMoreTime
			}
		case a == "-":
			if err := body.add("stdin", ""); err != nil {
				return cmd, body, err
			}
		case strings.HasPrefix(a, "--"):
			return cmd, body, fmt.Errorf("unknown option %q for reply", a)
		case cmd.To == 0:
			id, err := messageID(a)
			if err != nil {
				return cmd, body, err
			}
			cmd.To = id
		default:
			if err := body.add("arg", a); err != nil {
				return cmd, body, err
			}
		}
	}
	switch {
	case cmd.To == 0:
		return cmd, body, fmt.Errorf("usage: crosshall reply <id> \"...\" [--decline | --more-time]")
	case !body.set && cmd.Outcome == "":
		return cmd, body, fmt.Errorf("the answer has no text")
	}
	return cmd, body, nil
}

func runAsk(ctx context.Context, env Env, opts options, args []string, stdout, stderr io.Writer) core.ExitCode {
	o := outputs{json: opts.json, stdout: stdout, stderr: stderr}
	cmd, body, err := parseAsk(args)
	if errors.Is(err, errRepeatedBody) && !cmd.All && len(cmd.To) == 0 && domain.ValidName(body.text) {
		return o.fail(core.ExitUsage, CodeUsage,
			fmt.Sprintf("recipient names start with @. To address %s, use @%s.", body.text, body.text), "crosshall who")
	}
	if err != nil {
		return o.usage(err.Error())
	}
	if cmd.Body, err = body.read(env); err != nil {
		return o.usage(err.Error())
	}
	return send(ctx, env, opts, Call{Command: cmd, OpKey: opKey(env, opts)}, stdout, stderr)
}

func runReply(ctx context.Context, env Env, opts options, args []string, stdout, stderr io.Writer) core.ExitCode {
	o := outputs{json: opts.json, stdout: stdout, stderr: stderr}
	cmd, body, err := parseReply(args)
	if err != nil {
		return o.usage(err.Error())
	}
	if cmd.Body, err = body.read(env); err != nil {
		return o.usage(err.Error())
	}
	return send(ctx, env, opts, Call{Command: cmd, OpKey: opKey(env, opts)}, stdout, stderr)
}

// now is the clock used to decide whether a time is today.
var now = time.Now

// clock formats a time for plain output: HH:MM today, else with the date.
func clock(msTime int64) string {
	t := time.UnixMilli(msTime).Local()
	ty, tm, td := t.Date()
	ny, nm, nd := now().Local().Date()
	if ty == ny && tm == nm && td == nd {
		return t.Format("15:04")
	}
	return t.Format("Jan 2 15:04")
}

func renderAsk(data []byte, w io.Writer) error {
	var r domain.AskResult
	if err := json.Unmarshal(data, &r); err != nil {
		return err
	}
	ex := fmt.Sprintf("x%d", r.Exchange)
	if r.ParentRequestID != 0 {
		ex += fmt.Sprintf(" (new handoff for #%d)", r.ParentRequestID)
	} else if r.NewExchange {
		ex += " (new)"
	} else if r.ExchangeOpener != "" {
		opener := "@" + r.ExchangeOpener
		if r.ExchangeOpenerDeparted {
			opener += ", now departed"
		}
		ex += " (opened by " + opener + ")"
	}
	if r.Remaining == 0 {
		fmt.Fprintf(w, "#%d stored. due %s. exchange %s: 0 left; this request spent its last unit.\n",
			r.ID, clock(r.DueAtMS), ex)
	} else {
		fmt.Fprintf(w, "#%d stored. due %s. exchange %s: %d left.\n", r.ID, clock(r.DueAtMS), ex, r.Remaining)
	}
	if r.ExchangeOpener != "" {
		fmt.Fprintln(w, "Run crosshall status to see the exchange balance.")
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, d := range r.Recipients {
		fmt.Fprintf(tw, "  @%s\t%s\n", d.Alias, deliveryText(d))
	}
	return tw.Flush()
}

func deliveryText(d domain.Delivery) string {
	switch d.State {
	case "stored":
		return "stored -> " + d.Next
	case "picked_up":
		return "picked up " + clock(d.PickedUpAtMS)
	case "more_time":
		return "asked for more time " + clock(d.MoreTimeAtMS)
	case "replied", "declined", "cancelled":
		return d.State + " " + clock(d.SettledAtMS)
	}
	return d.State
}

func renderReply(data []byte, w io.Writer) error {
	var r domain.ReplyResult
	if err := json.Unmarshal(data, &r); err != nil {
		return err
	}
	defer func() {
		if r.Delivery.State != "" {
			fmt.Fprintf(w, "  @%s  %s\n", r.Delivery.Alias, deliveryText(r.Delivery))
		}
		if r.OriginalUnread {
			fmt.Fprintf(w, "To read the original request, run crosshall read %d.\n", r.Ask)
		}
	}()
	switch r.Outcome {
	case domain.OutcomeMoreTime:
		fmt.Fprintf(w, "#%d asks for more time on #%d (to @%s); it is still due %s.\n", r.ID, r.Ask, r.To, clock(r.DueAtMS))
		return nil
	case domain.OutcomeDecline:
		fmt.Fprintf(w, "#%d declines #%d (to @%s).", r.ID, r.Ask, r.To)
	default:
		fmt.Fprintf(w, "#%d answers #%d (to @%s).", r.ID, r.Ask, r.To)
	}
	if r.LateByMS > 0 {
		fmt.Fprintf(w, " Late by %s.", lateness(r.LateByMS))
	}
	fmt.Fprintln(w)
	return nil
}
