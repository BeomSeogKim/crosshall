// Package cli parses command-line arguments and renders command results.
package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
)

const usage = `Usage: crosshall [--json] [--key <k>] <command>

Member commands:
  join [<channel>] [--as <alias>] [--focus "..."]   Join a channel
  who                                               List the members of your channel
  read [<id>...]                                    Read messages
  inbox                                             List unread messages and open obligations
  wait [--any | --work-only] [--existing] [--timeout <duration>]
                                                    Wait for messages (timeout exits 6)
                                                    --existing also wakes for already unread messages
                                                    --work-only accepts addressed requests and replies
  waiting                                           List your outstanding requests
  status                                            Show the channel ledger
  whoami [--full]                                   Show a short identity digest (--full prints the raw key)
  ask @a [@b ...] "..." [--due 30m] [--re <id>] [--new]
                                                    Ask members for an answer (or --all --each)
                                                    Use --new --re <id> for a funded handoff
  reply <id> "..." [--decline|--more-time]          Answer a request addressed to you
  note @a [@b ...] "..."                            Tell members something; no answer expected (or --all)
  log [--since <id>] [--limit <n>]                  Show the channel's history
  focus "..."                                       Say in one line what you are working on
  publish on|off                                    Turn notices to your session on or off;
                                                    on never overrides an owner's off
  leave                                             Leave your channel
  run -- <command> [args...]                        Run a command with its own identity

Owner commands (from a separate owner terminal):
  setup [--dry-run]                                 Preview or install user-level adapter hooks
  setup <vendor> [--apply]                           Set up a vendor, or apply without prompting
  setup status                                      Inspect installed hooks and adapter policy
  setup off                                         Disable adapter effects
  uninstall [--dry-run]                             Preview or remove hooks installed by Crosshall
  owner start                                       Start the dispatcher
  owner stop                                        Stop the dispatcher and wait for it to exit
  owner channel create <name> [--topic "..."] [--pool <n>]
                                                    Create a channel; each first join takes
                                                    one root token from its pool (default 8)
  owner who --all                                   List all channel bindings
  owner depart <binding-id> --seen <token>           Mark a binding departed
  owner log <channel> [--since <id>] [--limit <n>]  Show a channel's history
  owner channel default <channel> [--repo <dir>]     Set a repository's default channel
  owner doctor                                      Check local dispatcher state
  owner status [--all | --channel <channel>]         Show the owner ledger
  owner cancel <channel>/<id>                       Cancel an open request
  owner budget grant <channel>/x<id> <units>         Add exchange credit
  owner budget grant <channel>/@<alias> --roots <n>   Add root tokens
  owner budget grant <channel> --pool <n>            Add root tokens to a channel's join pool
  owner restore <binding-id> --new-key <vendor>:<key>  Restore a member's identity
  owner adapter set <adapter> [--enabled on|off] [--reminder on|off] [--publish on|off|default]
                                                    Change adapter policy
  owner publish hall on|off                         Turn notices on or off for the hall
  owner publish member <binding-id> on|off|default  Set notices for one member; the member
                                                    switch beats the adapter switch, which
                                                    beats the hall switch
  owner publish resume <binding-id>                 Lift a member's automatic publish suspension
  owner adapter evidence <file>                     Record a conformance run's results

Other:
  help     Show this help
  version  Print the build version (also --version)
`

// Call is one command handed to the transport.
type Call struct {
	Owner   bool
	Command domain.Command
	// OpKey is empty for commands without a durable effect.
	OpKey string
}

// Transport delivers a call to the dispatcher and returns its result.
// Unsent, unknown-outcome and enrollment failures come back as results with
// exit status 4 or 5; an error means a local failure (exit 1).
type Transport interface {
	Do(ctx context.Context, call Call) (core.Result, error)
}

// Env holds the parts the CLI needs from the other packages. Nil members make
// the matching commands report that they are unavailable in this build.
type Env struct {
	Version   string
	Transport Transport
	// CommandDeadline is the invocation budget for ordinary commands.
	CommandDeadline time.Time
	// OwnerGate returns a non-empty refusal message when this process may not
	// act as the owner.
	OwnerGate func() string
	// Start starts the dispatcher if it is not running.
	Start func(ctx context.Context) error
	// StartReady confirms a dispatcher response for the explicit owner start.
	StartReady func(ctx context.Context) error
	// Stop waits for the running dispatcher to release its lock.
	Stop func(ctx context.Context) (bool, error)
	// Whoami and WhoamiJSON display the raw key only on an explicit full request.
	Whoami     func(stdout io.Writer, full bool) core.ExitCode
	WhoamiJSON func(stdout io.Writer, full bool) core.ExitCode
	// RunChild runs a command with a fresh route identity.
	RunChild func(argv []string) core.ExitCode
	// NewKey returns a fresh operation key.
	NewKey func() string
	// Stdin supplies a message body given as "-". Nil means no input.
	Stdin io.Reader
	// Unavailable, when set, explains why commands that need the dispatcher
	// cannot run (for example a development build without CROSSHALL_STATE_DIR).
	Unavailable string
	// Doctor checks existing local state without launching the dispatcher.
	Doctor func(context.Context) core.Result
	// Setup manages the explicitly selected user-level adapter configuration.
	Setup func(SetupRequest) error
	// SetupTerminal reports whether standard input and output are both a
	// terminal, so setup may ask questions. Nil means no terminal.
	SetupTerminal func() bool
	// SetupAdapters lists the shipped hook adapters a setup may name.
	SetupAdapters []string
}

// Run executes the command line with the default environment.
func Run(args []string, stdout, stderr io.Writer, version string) core.ExitCode {
	return RunEnv(context.Background(), Env{Version: version}, args, stdout, stderr)
}

type options struct {
	json bool
	key  string
}

// RunEnv executes the command line and returns the exit status.
func RunEnv(ctx context.Context, env Env, args []string, stdout, stderr io.Writer) core.ExitCode {
	o := outputs{json: wantsJSON(args), stdout: stdout, stderr: stderr}
	opts, rest, err := globalFlags(args)
	if err != nil {
		return o.usage(err.Error())
	}
	if len(rest) == 0 {
		fmt.Fprint(stdout, usage)
		return core.ExitOK
	}
	verb, rest := rest[0], rest[1:]
	if verb != domain.VerbWait && !env.CommandDeadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, env.CommandDeadline)
		defer cancel()
	}
	switch verb {
	case "help", "--help", "-h":
		fmt.Fprint(stdout, usage)
		return core.ExitOK
	case "version", "--version":
		if len(rest) != 0 {
			return o.usage(verb + " takes no arguments")
		}
		fmt.Fprintln(stdout, env.Version)
		return core.ExitOK
	case "whoami":
		full := len(rest) == 1 && rest[0] == "--full"
		if len(rest) != 0 && !full {
			return o.usage("usage: crosshall whoami [--full]")
		}
		whoami := env.Whoami
		if opts.json {
			whoami = env.WhoamiJSON
		}
		if whoami == nil {
			return o.unavailableFor(env, "whoami")
		}
		return whoami(stdout, full)
	case "run":
		if len(rest) < 2 || rest[0] != "--" {
			return o.usage("usage: crosshall run -- <command> [args...]")
		}
		if env.RunChild == nil {
			return o.unavailable("run")
		}
		return env.RunChild(rest[1:])
	// read-ledger
	case domain.VerbRead, domain.VerbInbox, domain.VerbWaiting, domain.VerbStatus:
		return readLedgerMember(ctx, env, opts, verb, rest, stdout, stderr)
		// end read-ledger
	case domain.VerbWait:
		return runWait(ctx, env, opts, rest, stdout, stderr)
	case "join":
		cmd, err := parseJoin(rest)
		if err != nil {
			return o.usage(err.Error())
		}
		return send(ctx, env, opts, Call{Command: cmd, OpKey: opKey(env, opts)}, stdout, stderr)
	case domain.VerbPublish:
		if len(rest) != 1 || rest[0] != "on" && rest[0] != "off" {
			return o.usage("usage: crosshall publish on|off")
		}
		return send(ctx, env, opts, Call{Command: domain.Publish{On: rest[0] == "on"}, OpKey: opKey(env, opts)}, stdout, stderr)
	case "who":
		if len(rest) != 0 {
			return o.usage("who takes no arguments")
		}
		return send(ctx, env, opts, Call{Command: domain.Who{}}, stdout, stderr)
	// ask-reply
	case "ask":
		return runAsk(ctx, env, opts, rest, stdout, stderr)
	case "reply":
		return runReply(ctx, env, opts, rest, stdout, stderr)
	// end ask-reply
	// note-log
	case domain.VerbNote, domain.VerbLog, domain.VerbLeave, domain.VerbFocus:
		return runNoteLog(ctx, env, opts, verb, rest, stdout, stderr)
	// end note-log
	case "owner":
		return runOwner(ctx, env, opts, rest, stdout, stderr)
	case "setup", "uninstall":
		return runSetup(env, opts, verb, rest, stdout, stderr)
	}
	return o.usage(fmt.Sprintf("unknown command %q; run `crosshall help`", verb))
}

func runOwner(ctx context.Context, env Env, opts options, args []string, stdout, stderr io.Writer) core.ExitCode {
	o := outputs{json: opts.json, stdout: stdout, stderr: stderr}
	if len(args) == 0 {
		return o.usage("usage: crosshall owner <start|stop|channel create>")
	}
	if env.OwnerGate != nil {
		if msg := env.OwnerGate(); msg != "" {
			return render(opts, nil, domain.Refuse(core.CodeOwnerOnly, msg,
				"rerun this command from your separate owner terminal"), stdout, stderr)
		}
	}
	switch {
	case len(args) == 1 && args[0] == "start":
		start := env.StartReady
		if start == nil {
			start = env.Start
		}
		if start == nil {
			return o.unavailableFor(env, "owner start")
		}
		if err := start(ctx); err != nil {
			return o.fail(core.ExitError, CodeLocalError, "dispatcher could not start: "+err.Error(), "")
		}
		if opts.json {
			fmt.Fprintln(stdout, `{"v":1,"ok":true,"kind":"owner start"}`)
		} else {
			fmt.Fprintln(stdout, "dispatcher running")
		}
		return core.ExitOK
	case len(args) == 1 && args[0] == "stop":
		if env.Stop == nil {
			return o.unavailableFor(env, "owner stop")
		}
		stopped, err := env.Stop(ctx)
		if err != nil {
			return o.fail(core.ExitError, CodeLocalError, "could not stop the dispatcher: "+err.Error(), "crosshall owner doctor")
		}
		if opts.json {
			fmt.Fprintf(stdout, `{"v":1,"ok":true,"kind":"owner stop","stopped":%t}`+"\n", stopped)
		} else if stopped {
			fmt.Fprintln(stdout, "dispatcher stopped")
		} else {
			fmt.Fprintln(stdout, "no dispatcher is running")
		}
		return core.ExitOK
	case len(args) >= 2 && args[0] == "channel" && args[1] == "create":
		cmd, err := parseCreateChannel(args[2:])
		if err != nil {
			return o.usage(err.Error())
		}
		// Owner commands start the dispatcher on demand.
		if env.Start != nil {
			if err := env.Start(ctx); err != nil {
				return render(opts, cmd, domain.Fail(core.ExitUnavailable, core.CodeUnsent,
					"dispatcher could not start: "+err.Error(), "crosshall owner start"), stdout, stderr)
			}
		}
		return send(ctx, env, opts, Call{Owner: true, Command: cmd, OpKey: opKey(env, opts)}, stdout, stderr)
	case args[0] == "who" || args[0] == "log" || args[0] == "doctor" ||
		(len(args) >= 2 && args[0] == "channel" && args[1] == "default"):
		return runOwnerViews(ctx, env, opts, args, stdout, stderr)
	case len(args) >= 2 && args[0] == "adapter" && args[1] == "evidence": // adapter-claude
		return runAdapterEvidence(ctx, env, opts, args, stdout, stderr)
	case args[0] == "adapter":
		return runAdapterPolicy(ctx, env, opts, args, stdout, stderr)
	case args[0] == "publish":
		cmd, err := parseOwnerPublish(args[1:])
		if err != nil {
			return o.usage(err.Error())
		}
		return send(ctx, env, opts, Call{Owner: true, Command: cmd, OpKey: opKey(env, opts)}, stdout, stderr)
	// read-ledger
	case args[0] == "status" || args[0] == "cancel" || args[0] == "budget" || args[0] == "restore" || args[0] == "depart":
		return readLedgerOwner(ctx, env, opts, args, stdout, stderr)
		// end read-ledger
	}
	return o.usage(fmt.Sprintf("unknown owner command %q", strings.Join(args, " ")))
}

func send(ctx context.Context, env Env, opts options, call Call, stdout, stderr io.Writer) core.ExitCode {
	o := outputs{json: opts.json, stdout: stdout, stderr: stderr}
	if env.Transport == nil {
		return o.unavailableFor(env, call.Command.Verb())
	}
	result, err := env.Transport.Do(ctx, call)
	if err != nil {
		return o.fail(core.ExitError, CodeLocalError, err.Error(), "")
	}
	// The roster cursor advances only after the whole result was written.
	return RenderAndAcknowledge(env.Transport, result, opts.json, stdout, stderr,
		func(out, diag io.Writer) core.ExitCode { return render(opts, call.Command, result, out, diag) })
}

func opKey(env Env, opts options) string {
	if opts.key != "" {
		return opts.key
	}
	if env.NewKey != nil {
		return env.NewKey()
	}
	return NewKey()
}

// NewKey returns 128 random bits in hex.
func NewKey() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

// globalFlags removes --json and --key from anywhere before a "--".
func globalFlags(args []string) (options, []string, error) {
	var opts options
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return opts, append(rest, args[i:]...), nil
		case a == "--json":
			opts.json = true
		case a == "--key":
			if i+1 >= len(args) || args[i+1] == "" {
				return opts, nil, fmt.Errorf("--key needs a value")
			}
			opts.key = args[i+1]
			i++
		case strings.HasPrefix(a, "--key="):
			opts.key = strings.TrimPrefix(a, "--key=")
			if opts.key == "" {
				return opts, nil, fmt.Errorf("--key needs a value")
			}
		default:
			rest = append(rest, a)
		}
	}
	return opts, rest, nil
}

// flagValue reads "--name value" or "--name=value" at args[i].
func flagValue(args []string, i int, name string) (string, int, bool, error) {
	a := args[i]
	if a == name {
		if i+1 >= len(args) {
			return "", i, true, fmt.Errorf("%s needs a value", name)
		}
		return args[i+1], i + 1, true, nil
	}
	if strings.HasPrefix(a, name+"=") {
		return strings.TrimPrefix(a, name+"="), i, true, nil
	}
	return "", i, false, nil
}

func parseJoin(args []string) (domain.Join, error) {
	var cmd domain.Join
	for i := 0; i < len(args); i++ {
		if v, j, ok, err := flagValue(args, i, "--as"); ok {
			if err != nil {
				return cmd, err
			}
			cmd.Alias, i = strings.TrimPrefix(v, "@"), j
			if !domain.ValidName(cmd.Alias) {
				return cmd, fmt.Errorf("invalid alias %q: use 1-32 of a-z, 0-9 and -, starting with a letter or digit", v)
			}
			continue
		}
		if v, j, ok, err := flagValue(args, i, "--focus"); ok {
			if err != nil {
				return cmd, err
			}
			cmd.Focus, i = v, j
			continue
		}
		if strings.HasPrefix(args[i], "-") {
			return cmd, fmt.Errorf("unknown option %q for join", args[i])
		}
		if cmd.Channel != "" {
			return cmd, fmt.Errorf("join takes one channel")
		}
		cmd.Channel = strings.TrimPrefix(args[i], "#")
		if !domain.ValidName(cmd.Channel) {
			return cmd, fmt.Errorf("invalid channel name %q", args[i])
		}
	}
	if cmd.Channel == "" {
		root, err := RepoRoot("")
		if err != nil {
			return cmd, fmt.Errorf("%w; or specify a channel with crosshall join <channel>", err)
		}
		cmd.RepoRoot = root
	}
	return cmd, nil
}

func parseCreateChannel(args []string) (domain.CreateChannel, error) {
	var cmd domain.CreateChannel
	for i := 0; i < len(args); i++ {
		if v, j, ok, err := flagValue(args, i, "--topic"); ok {
			if err != nil {
				return cmd, err
			}
			cmd.Topic, i = v, j
			continue
		}
		if v, j, ok, err := flagValue(args, i, "--pool"); ok {
			n, convErr := strconv.ParseInt(v, 10, 64)
			if err != nil || convErr != nil || n < 0 {
				return cmd, fmt.Errorf("--pool needs a count of 0 or more")
			}
			cmd.Pool, i = &n, j
			continue
		}
		if strings.HasPrefix(args[i], "-") {
			return cmd, fmt.Errorf("unknown option %q for channel create", args[i])
		}
		if cmd.Name != "" {
			return cmd, fmt.Errorf("channel create takes one name")
		}
		cmd.Name = strings.TrimPrefix(args[i], "#")
	}
	if !domain.ValidName(cmd.Name) {
		return cmd, fmt.Errorf("usage: crosshall owner channel create <name> [--topic \"...\"] [--pool <n>]; a name is 1-32 of a-z, 0-9 and -")
	}
	return cmd, nil
}

// Codes for failures detected by the CLI itself, before or without a result
// from the dispatcher.
const (
	CodeUsage      core.Code = "usage"
	CodeLocalError core.Code = "local_error"
)

// outputs reports a local failure as text on stderr, or as one JSON object
// on stdout in --json mode, and returns its exit status.
type outputs struct {
	json           bool
	stdout, stderr io.Writer
}

func (o outputs) fail(status core.ExitCode, code core.Code, msg, hint string) core.ExitCode {
	if o.json {
		writeJSON(o.stdout, map[string]any{"v": 1, "ok": false, "code": code, "message": msg, "hint": hint})
		return status
	}
	fmt.Fprintf(o.stderr, "crosshall: %s\n", msg)
	if hint != "" {
		fmt.Fprintf(o.stderr, "next: %s\n", hint)
	}
	return status
}

func (o outputs) usage(msg string) core.ExitCode {
	return o.fail(core.ExitUsage, CodeUsage, msg, "crosshall help")
}

func (o outputs) unavailableFor(env Env, what string) core.ExitCode {
	if env.Unavailable != "" {
		return o.fail(core.ExitError, CodeLocalError, env.Unavailable, "")
	}
	return o.unavailable(what)
}

func (o outputs) unavailable(what string) core.ExitCode {
	return o.fail(core.ExitError, CodeLocalError, what+" is not available in this build", "")
}

// wantsJSON reports whether --json appears before any "--", so that even an
// argument error can be reported as JSON.
func wantsJSON(args []string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if a == "--json" {
			return true
		}
	}
	return false
}
