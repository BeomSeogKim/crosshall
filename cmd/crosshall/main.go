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
	defaultRoot, err := dispatch.DefaultStateRoot()
	if err != nil {
		return p, err
	}
	p.defaultState = defaultRoot
	p.state = p.defaultState
	if dir := os.Getenv("CROSSHALL_STATE_DIR"); dir != "" {
		if !filepath.IsAbs(dir) {
			return p, errors.New("CROSSHALL_STATE_DIR must be an absolute path")
		}
		p.state = filepath.Clean(dir)
	}
	p.rendezvous = ident.Rendezvous(p.state, os.Geteuid())
	return p, dispatch.CheckStateRoot(version, p.state, p.defaultState)
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
		Stdin:     os.Stdin,
	}
	if pathErr == nil {
		transport := client.New(spool.Client{StateRoot: paths.state, Rendezvous: paths.rendezvous})
		transport.EnableMemberStart(entry, paths.launch(false).Start)
		env.Transport = transport
		env.Doctor = paths.launch(false).Doctor
		env.Whoami = transport.Whoami
		env.WhoamiJSON = transport.WhoamiJSON
		env.Start = func(ctx context.Context) error {
			_, err := paths.launch(true).Start(ctx)
			return err
		}
	} else {
		env.Unavailable = pathErr.Error()
	}
	// The owner startup path and cold member calls may use this outer limit.
	// The transport keeps identified member calls within their own allowance.
	ctx, cancel := context.WithDeadline(context.Background(), spool.Deadline(entry, true))
	defer cancel()
	return cli.RunEnv(ctx, env, args, os.Stdout, os.Stderr)
}

func serve(paths statePaths, pathErr error) int {
	if pathErr != nil {
		fmt.Fprintln(os.Stderr, "dispatcher:", pathErr)
		return 1
	}
	clock, err := dispatch.DomainClock(version)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dispatcher:", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	resolver := ident.SQLResolver{StateRoot: paths.state, RendezvousRoot: paths.rendezvous, Clock: clock}
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
			Handle: &domain.Service{Clock: clock, RestoreKey: ident.RestoreKey},
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
