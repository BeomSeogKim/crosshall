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
	"strings"
	"syscall"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/cli"
	"github.com/BeomSeogKim/crosshall/internal/client"
	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/dispatch"
	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/hook"
	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/proc"
	"github.com/BeomSeogKim/crosshall/internal/setup"
	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/telemetryconfig"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

// Release builds set version with -ldflags "-X main.version=vX.Y.Z".
var version = "dev"

func main() {
	entry := time.Now()
	if handled, code := dispatch.RunTestDispatcherGuardian(os.Args[1:]); handled {
		os.Exit(code)
	}
	if len(os.Args) > 1 && os.Args[1] == "hook" {
		runHook(entry, os.Args[2:])
		return
	}
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
		Version:         version,
		CommandDeadline: spool.Deadline(entry, true),
		OwnerGate:       ident.OwnerGate,
		RunChild:        ident.RunChild,
		Stdin:           os.Stdin,
		SetupTerminal:   func() bool { return isTerminal(os.Stdin) && isTerminal(os.Stdout) },
	}
	for _, spec := range hook.InstallSpecs() {
		env.SetupAdapters = append(env.SetupAdapters, spec.Adapter)
	}
	if pathErr == nil {
		transport := client.New(spool.Client{StateRoot: paths.state, Rendezvous: paths.rendezvous})
		transport.EnableMemberStart(entry, paths.launch(false).Start)
		transport.EnableJoinLaunch(hook.JoinLaunch, func(id ident.Identity, l domain.JoinLaunch) error {
			return hook.BindJoinLaunch(paths.rendezvous, id, l)
		})
		transport.EnableHost(hook.MemberHost)
		env.Transport = transport
		env.Doctor = paths.launch(false).Doctor
		env.Whoami = transport.Whoami
		env.WhoamiJSON = transport.WhoamiJSON
		env.Start = func(ctx context.Context) error {
			_, err := paths.launch(true).Start(ctx)
			return err
		}
		env.StartReady = paths.launch(true).StartReady
		env.Stop = paths.launch(true).Stop
		env.TelemetryEndpoint = telemetryconfig.Endpoint
		env.TelemetryCurl = telemetryconfig.CurlPath
		env.TelemetryStateRoot = paths.state
		env.Setup = func(req cli.SetupRequest) error {
			executable, err := os.Executable()
			if err != nil {
				return err
			}
			executable, err = filepath.EvalSymlinks(executable)
			if err != nil {
				return err
			}
			dispatcherReady := false
			service := setup.Service{StateRoot: paths.state, Executable: executable,
				SetPolicy: func(ctx context.Context, adapter string, enabled bool) error {
					// Give each policy update its own budget after the interactive plan.
					ctx, cancel := context.WithDeadline(ctx, spool.Deadline(time.Now(), true))
					defer cancel()
					if err := env.Start(ctx); err != nil {
						return err
					}
					cmd := domain.AdapterSet{AdapterPolicyArgs: wire.AdapterPolicyArgs{Adapter: adapter, Enabled: &enabled}}
					result, err := transport.Do(ctx, cli.Call{Owner: true, Command: cmd, OpKey: wire.NewID()})
					if err != nil {
						return err
					}
					if result.Status != core.ExitOK {
						var detail struct{ Message string }
						_ = json.Unmarshal(result.Data, &detail)
						return fmt.Errorf("dispatcher did not confirm the policy update (%s): %s", result.Code, detail.Message)
					}
					dispatcherReady = true
					if !enabled {
						return nil
					}
					// The plan said so: installing turns automatic joining on, once.
					auto := domain.ChannelAuto{On: true, Initial: true}
					result, err = transport.Do(ctx, cli.Call{Owner: true, Command: auto, OpKey: wire.NewID()})
					if err != nil {
						return err
					}
					if result.Status != core.ExitOK {
						return fmt.Errorf("dispatcher did not confirm automatic joining (%s)", result.Code)
					}
					return nil
				}}
			if req.Offer && req.Terminal && os.Getenv("CROSSHALL_INSTALL_PATH_CONSENT") == "1" {
				req.PathConsent = writeInstallerPathConsent
			}
			installed := false
			req.Installed = func() { installed = true }
			runErr := service.Run(context.Background(), setup.Request(req))
			if !req.Offer || !req.Terminal || os.Getenv("CROSSHALL_INSTALL_READY_RECEIPT") != "1" {
				return runErr
			}
			// The installer reads each line on its own: a ready dispatcher
			// does not prove that any agent's hooks were installed.
			var lines []string
			if dispatcherReady {
				lines = append(lines, "ready")
			}
			if installed {
				lines = append(lines, "installed")
			}
			if len(lines) == 0 {
				return runErr
			}
			return errors.Join(runErr, writeInstallerReceipt(4, strings.Join(lines, "\n")))
		}
	} else {
		env.Unavailable = pathErr.Error()
	}
	return cli.RunEnv(context.Background(), env, args, os.Stdout, os.Stderr)
}

func writeInstallerPathConsent(accepted bool) error {
	answer := "no"
	if accepted {
		answer = "yes"
	}
	return writeInstallerReceipt(3, answer)
}

func writeInstallerReceipt(fd uintptr, value string) error {
	receipt := os.NewFile(fd, "installer receipt")
	if receipt == nil {
		return fmt.Errorf("installer receipt descriptor %d is missing", fd)
	}
	_, err := fmt.Fprintln(receipt, value)
	return errors.Join(err, receipt.Close())
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
					ident.Proof{Generation: req.Generation, KeyDigest: req.KeyDigest, VendorVersion: req.VendorVersion,
						Join: req.Verb == domain.VerbJoin})
				if refusal, ok := identityRefusal(err); ok {
					return dispatch.Resolution{Refusal: &refusal}, nil
				}
				if err != nil {
					return dispatch.Resolution{}, err
				}
				return dispatch.Resolution{Caller: r.Caller, Vendor: r.Vendor, SessionKey: r.Key, Enroll: r.Enroll,
					AfterCommit: r.AfterCommit}, nil
			},
			Decode: func(req wire.Request, r dispatch.Resolution) (domain.Request, error) {
				cmd, err := domain.Decode(req.Verb, req.Args)
				if err != nil {
					return domain.Request{}, err
				}
				return domain.Request{OpKey: req.OpKey, Command: cmd, Vendor: r.Vendor, SessionKey: r.SessionKey, Enroll: r.Enroll, Ack: req.Ack,
					Host: req.Host}, nil
			},
			Handle: &domain.Service{Clock: clock, RestoreKey: ident.RestoreKey, LaunchAlive: ident.LaunchAlive,
				LaunchProcess: ident.LaunchProcess, LaunchPIDNamespace: proc.PIDNamespace, ProcessGone: proc.ProcessGone, ProcessAlive: proc.ProcessAlive,
				HasPromptLine: hook.HasPromptLine, HasSessionEnd: hook.HasSessionEnd,
				LeafReceipts: func(vendor, key, fileID, generation string) (uint64, uint64) {
					return ident.LeafReceipts(paths.rendezvous, ident.Identity{Vendor: vendor, Key: key}, fileID, generation)
				}},
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
	switch e.Code {
	case core.CodeKeyRetired:
		hint = "ask the owner to restore this session"
	case core.CodeKeyCleared:
		hint = "crosshall join <channel> makes this conversation a new member"
	}
	return domain.Fail(status, e.Code, e.Message, hint), true
}
