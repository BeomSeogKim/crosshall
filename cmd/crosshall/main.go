// Command crosshall runs the local Crosshall CLI and, as a hidden
// subcommand, the dispatcher process.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/cli"
	"github.com/BeomSeogKim/crosshall/internal/client"
	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/dispatch"
	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

// Release builds set version with -ldflags "-X main.version=vX.Y.Z".
var version = "dev"

func main() {
	entry := time.Now()
	paths, pathErr := resolvePaths()
	if len(os.Args) == 2 && os.Args[1] == "__dispatch" {
		os.Exit(serve(paths, pathErr))
	}
	os.Exit(int(runCLI(entry, paths, pathErr)))
}

type statePaths struct {
	state, defaultState, rendezvous string
}

// resolvePaths selects the state root: CROSSHALL_STATE_DIR, else
// $XDG_STATE_HOME/crosshall, else ~/.local/state/crosshall.
func resolvePaths() (statePaths, error) {
	var p statePaths
	if xdg := os.Getenv("XDG_STATE_HOME"); filepath.IsAbs(xdg) {
		p.defaultState = filepath.Join(xdg, "crosshall")
	} else if home, err := os.UserHomeDir(); err == nil {
		p.defaultState = filepath.Join(home, ".local", "state", "crosshall")
	} else {
		return p, errors.New("cannot find the state directory: set HOME or CROSSHALL_STATE_DIR")
	}
	p.state = p.defaultState
	if dir := os.Getenv("CROSSHALL_STATE_DIR"); dir != "" {
		if !filepath.IsAbs(dir) {
			return p, errors.New("CROSSHALL_STATE_DIR must be an absolute path")
		}
		p.state = filepath.Clean(dir)
	}
	p.rendezvous = ident.Rendezvous(p.state, os.Geteuid())
	return p, nil
}

func (p statePaths) launch(owner bool) dispatch.LaunchConfig {
	return dispatch.LaunchConfig{
		Version: version, StateRoot: p.state, DefaultStateRoot: p.defaultState,
		Rendezvous: p.rendezvous, Owner: owner,
	}
}

func runCLI(entry time.Time, paths statePaths, pathErr error) core.ExitCode {
	args := os.Args[1:]
	env := cli.Env{
		Version:   version,
		OwnerGate: ident.OwnerGate,
		RunChild:  ident.RunChild,
	}
	if pathErr == nil && dispatch.CheckStateRoot(version, paths.state, paths.defaultState) == nil {
		transport := client.New(spool.Client{StateRoot: paths.state, Rendezvous: paths.rendezvous})
		env.Transport = transport
		env.Whoami = transport.Whoami
		env.WhoamiJSON = transport.WhoamiJSON
		env.Start = func(ctx context.Context) error {
			_, err := paths.launch(true).Start(ctx)
			return err
		}
	} else {
		reason := pathErr
		if reason == nil {
			reason = dispatch.CheckStateRoot(version, paths.state, paths.defaultState)
		}
		env.Unavailable = reason.Error()
	}
	// Owner commands may start the dispatcher, which takes up to three
	// seconds; member commands get the one-second allowance.
	spawned := len(args) > 0 && containsOwner(args)
	ctx, cancel := context.WithDeadline(context.Background(), spool.Deadline(entry, spawned))
	defer cancel()
	return cli.RunEnv(ctx, env, args, os.Stdout, os.Stderr)
}

func containsOwner(args []string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if a == "owner" {
			return true
		}
	}
	return false
}

func serve(paths statePaths, pathErr error) int {
	if pathErr != nil {
		fmt.Fprintln(os.Stderr, "dispatcher:", pathErr)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	resolver := ident.SQLResolver{StateRoot: paths.state, RendezvousRoot: paths.rendezvous, Clock: time.Now}
	cfg := dispatch.Config{
		LaunchConfig: paths.launch(false),
		Services: dispatch.Services{
			Resolve: func(ctx context.Context, tx store.Tx, a dispatch.Arrival, req wire.Request) (dispatch.Resolution, error) {
				r, err := resolver.Resolve(ctx, tx,
					ident.Ingress{Path: a.Path, Owner: a.Owner, Dir: a.Dir},
					ident.Proof{Generation: req.Generation, KeyDigest: req.KeyDigest, VendorVersion: req.VendorVersion})
				if refusal, ok := identityRefusal(err); ok {
					return dispatch.Resolution{Refusal: &refusal}, nil
				}
				if err != nil {
					return dispatch.Resolution{}, err
				}
				return dispatch.Resolution{Caller: r.Caller, Vendor: r.Vendor, Enroll: r.Enroll}, nil
			},
			Decode: func(req wire.Request, r dispatch.Resolution) (domain.Request, error) {
				cmd, err := domain.Decode(req.Verb, req.Args)
				if err != nil {
					return domain.Request{}, err
				}
				return domain.Request{OpKey: req.OpKey, Command: cmd, Vendor: r.Vendor, Enroll: r.Enroll, Ack: req.Ack}, nil
			},
			Handle: &domain.Service{Clock: time.Now},
			Hello: func(ctx context.Context, tx store.Tx, raw []byte) (core.Result, func() error, error) {
				var hello wire.Hello
				if err := json.Unmarshal(raw, &hello); err != nil {
					return domain.Fail(core.ExitUsage, "", "invalid first-contact request", ""), nil, nil
				}
				reg, err := ident.RegisterHello(ctx, tx, paths.rendezvous, hello, time.Now())
				if refusal, ok := identityRefusal(err); ok {
					return refusal, nil, nil
				}
				if err != nil {
					return core.Result{}, nil, err
				}
				return core.Result{Status: core.ExitOK, Data: json.RawMessage(`{}`)}, reg.Publish, nil
			},
		},
	}
	if err := dispatch.Serve(ctx, cfg); err != nil {
		fmt.Fprintln(os.Stderr, "dispatcher:", err)
		return 1
	}
	return 0
}

// identityRefusal maps an identity error to the result the caller sees.
func identityRefusal(err error) (core.Result, bool) {
	var e *ident.EnvironmentError
	if !errors.As(err, &e) {
		return core.Result{}, false
	}
	status := core.ExitNotEnrolled
	if e.Code == core.CodeOwnerOnly {
		status = core.ExitRefused
	}
	hint := "crosshall join <channel>"
	if e.Code == core.CodeKeyRetired {
		hint = "ask the owner to restore this session"
	}
	return domain.Fail(status, e.Code, e.Message, hint), true
}
