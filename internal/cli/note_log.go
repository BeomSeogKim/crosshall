package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
)

func parseNote(args []string) (domain.Note, bodySource, error) {
	var cmd domain.Note
	var body bodySource
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			rest := args[i+1:]
			if len(rest) != 1 {
				return cmd, body, fmt.Errorf("give the text as one argument after --")
			}
			if err := body.add("arg", rest[0]); err != nil {
				return cmd, body, err
			}
			break
		}
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
		case a == "--all":
			cmd.All = true
		case a == "-":
			if err := body.add("stdin", ""); err != nil {
				return cmd, body, err
			}
		case strings.HasPrefix(a, "@") && domain.ValidName(a[1:]):
			cmd.To = append(cmd.To, a[1:])
		case strings.HasPrefix(a, "--"):
			return cmd, body, fmt.Errorf("unknown option %q for note", a)
		default:
			if err := body.add("arg", a); err != nil {
				return cmd, body, err
			}
		}
	}
	switch {
	case len(cmd.To) == 0 && !cmd.All:
		return cmd, body, fmt.Errorf("usage: crosshall note @alias [@alias ...] \"...\" (or --all)")
	case len(cmd.To) > 0 && cmd.All:
		return cmd, body, fmt.Errorf("address members by @alias or with --all, not both")
	case !body.set:
		return cmd, body, fmt.Errorf("the note has no text")
	}
	return cmd, body, nil
}

func parseLog(args []string) (domain.Log, error) {
	var cmd domain.Log
	for i := 0; i < len(args); i++ {
		if v, j, ok, err := flagValue(args, i, "--since"); ok {
			if err != nil {
				return cmd, err
			}
			n, err := strconv.ParseInt(strings.TrimPrefix(v, "#"), 10, 64)
			if err != nil || n < 0 {
				return cmd, fmt.Errorf("--since takes a message number")
			}
			cmd.Since, i = &n, j
			continue
		}
		if v, j, ok, err := flagValue(args, i, "--limit"); ok {
			if err != nil {
				return cmd, err
			}
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > domain.MaxLogLimit {
				return cmd, fmt.Errorf("--limit takes 1 to %d", domain.MaxLogLimit)
			}
			cmd.Limit, i = n, j
			continue
		}
		return cmd, fmt.Errorf("usage: crosshall log [--since <id>] [--limit <n>]")
	}
	return cmd, nil
}

func runNoteLog(ctx context.Context, env Env, opts options, verb string, args []string, stdout, stderr io.Writer) core.ExitCode {
	o := outputs{json: opts.json, stdout: stdout, stderr: stderr}
	var call Call
	switch verb {
	case domain.VerbNote:
		cmd, body, err := parseNote(args)
		if err != nil {
			return o.usage(err.Error())
		}
		if cmd.Body, err = body.read(env); err != nil {
			return o.usage(err.Error())
		}
		call = Call{Command: cmd, OpKey: opKey(env, opts)}
	case domain.VerbLog:
		cmd, err := parseLog(args)
		if err != nil {
			return o.usage(err.Error())
		}
		call = Call{Command: cmd}
	case domain.VerbLeave:
		if len(args) != 0 {
			return o.usage("leave takes no arguments")
		}
		call = Call{Command: domain.Leave{}, OpKey: opKey(env, opts)}
	case domain.VerbFocus:
		if len(args) != 1 {
			return o.usage(`usage: crosshall focus "one line on what you are working on"`)
		}
		call = Call{Command: domain.Focus{Text: args[0]}, OpKey: opKey(env, opts)}
	}
	return send(ctx, env, opts, call, stdout, stderr)
}

func renderNoteLog(cmd domain.Command, data []byte, w io.Writer) error {
	switch c := cmd.(type) {
	case domain.Note:
		var r domain.NoteResult
		if err := json.Unmarshal(data, &r); err != nil {
			return err
		}
		fmt.Fprintf(w, "#%d note stored; nobody owes an answer.\n", r.ID)
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, d := range r.Recipients {
			fmt.Fprintf(tw, "  @%s\t%s\n", d.Alias, deliveryText(d))
		}
		return tw.Flush()
	case domain.Log:
		var r domain.LogResult
		if err := json.Unmarshal(data, &r); err != nil {
			return err
		}
		renderLog(w, r, c.Limit)
	case domain.Leave:
		var r domain.LeaveResult
		if err := json.Unmarshal(data, &r); err != nil {
			return err
		}
		fmt.Fprintf(w, "Left #%s as @%s.", r.Channel, r.Alias)
		if r.Owed > 0 {
			fmt.Fprintf(w, " %d request(s) you owe stay open and visible to the owner.", r.Owed)
		}
		if r.Waiting > 0 {
			fmt.Fprintf(w, " %d answer(s) owed to you stay open.", r.Waiting)
		}
		fmt.Fprintf(w, "\nTo come back: crosshall join %s\n", r.Channel)
	case domain.Focus:
		var r domain.FocusResult
		if err := json.Unmarshal(data, &r); err != nil {
			return err
		}
		if r.Changed {
			fmt.Fprintf(w, "@%s focus: %s\n", r.Alias, r.Focus)
		} else {
			fmt.Fprintf(w, "@%s focus unchanged: %s\n", r.Alias, r.Focus)
		}
	default:
		return fmt.Errorf("no plain form for %T", cmd)
	}
	return nil
}

func renderLog(w io.Writer, r domain.LogResult, limit int) {
	if len(r.Messages) == 0 {
		fmt.Fprintf(w, "#%s: no messages.\n", r.Channel)
	}
	for _, e := range r.Messages {
		head := fmt.Sprintf("#%d %s %s from @%s", e.ID, clock(e.CreatedAtMS), e.Kind, e.Sender)
		if e.Outcome != "" && e.Outcome != domain.OutcomeAnswer {
			head = fmt.Sprintf("#%d %s %s (%s) from @%s", e.ID, clock(e.CreatedAtMS), e.Kind,
				strings.ReplaceAll(e.Outcome, "_", " "), e.Sender)
		}
		switch {
		case e.All:
			head += " to all"
		case len(e.To) > 0:
			head += " to @" + strings.Join(e.To, " @")
		}
		if e.InReplyTo != nil {
			head += fmt.Sprintf(", re #%d", *e.InReplyTo)
		}
		if e.DueAtMS != nil {
			head += ", due " + clock(*e.DueAtMS)
		}
		fmt.Fprintln(w, head)
		for _, line := range strings.Split(strings.TrimRight(e.Body, "\n"), "\n") {
			fmt.Fprintln(w, "  "+line)
		}
	}
	more := ""
	if limit != 0 && limit != domain.DefaultLogLimit {
		more = " --limit " + strconv.Itoa(limit)
	}
	switch {
	case r.Next != nil && r.Later > 0:
		fmt.Fprintf(w, "... %d later: crosshall log --since %d%s\n", r.Later, *r.Next, more)
	case r.Next != nil && r.Earlier > 0:
		fmt.Fprintf(w, "... %d earlier: crosshall log --since %d%s\n", r.Earlier, *r.Next, more)
	}
}
