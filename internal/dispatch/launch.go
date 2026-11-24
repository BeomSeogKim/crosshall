package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/telemetryconfig"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

const startupTimeout = 3 * time.Second

// LaunchConfig contains the resolved paths and host eligibility used by Start.
type LaunchConfig struct {
	Version          string
	StateRoot        string
	DefaultStateRoot string
	Rendezvous       string
	Owner            bool
}

// Start starts the local dispatcher if needed and waits for it to be ready.
// It reports whether this call launched a child, even if readiness fails.
// Owner must come from the explicit CLI owner gate, independent of startup.
func (cfg LaunchConfig) Start(ctx context.Context) (bool, error) {
	return cfg.start(ctx, syscall.Kill, startupTimeout)
}

func (cfg LaunchConfig) start(ctx context.Context, probe func(int, syscall.Signal) error, readyTimeout time.Duration) (bool, error) {
	state, err := cfg.openStartState(ctx)
	if err != nil {
		return false, err
	}
	defer state.Close()

	previous := readReady(cfg.Rendezvous)
	live, err := previous.liveness(state, probe)
	if live || (!cfg.Owner && err != nil) {
		// A sandboxed member can still send through the rendezvous when
		// the local startup probe is inconclusive.
		return false, nil
	}
	executable, err := os.Executable()
	if err != nil {
		return false, err
	}
	process, err := cfg.spawn(state, executable, time.Time{})
	if err != nil {
		return false, err
	}
	// Waiting also reaps the child if it exits while this process lives on.
	exited := make(chan struct{})
	go func() {
		_, _ = process.Wait()
		close(exited)
	}()

	wait, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		marker := readReady(cfg.Rendezvous)
		if (marker.PID == process.Pid || marker.Instance != previous.Instance) && marker.live(state) {
			return true, nil
		}
		select {
		case <-wait.Done():
			return true, startupError(wait.Err())
		case <-exited:
			exited = nil
			// A child gives way to a dispatcher that already holds the
			// lock; that one may still become ready. Otherwise none will.
			if held, err := lockState(state); err != nil || !held {
				return true, startupError(errExitedBeforeReady)
			}
		case <-ticker.C:
		}
	}
}

var errExitedBeforeReady = errors.New("the dispatcher exited before it was ready")

func (cfg LaunchConfig) openStartState(ctx context.Context) (*os.Root, error) {
	if err := CheckStateRoot(cfg.Version, cfg.StateRoot, cfg.DefaultStateRoot); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(cfg.StateRoot) || !filepath.IsAbs(cfg.Rendezvous) {
		return nil, errors.New("dispatcher paths must be absolute")
	}
	if cfg.Owner {
		if err := os.MkdirAll(cfg.StateRoot, 0700); err != nil {
			return nil, err
		}
		// Newly created components now have filesystem identities, including
		// names the filesystem considers equivalent despite different spelling.
		if err := CheckStateRoot(cfg.Version, cfg.StateRoot, cfg.DefaultStateRoot); err != nil {
			return nil, err
		}
	}
	state, err := spool.OpenServerRoot(cfg.StateRoot)
	if err != nil {
		return nil, err
	}
	if cfg.Owner {
		if err := prepareNamedSpool(state, "owner"); err != nil {
			state.Close()
			return nil, err
		}
	}
	return state, nil
}

func startupError(err error) error {
	return fmt.Errorf("dispatcher did not become ready; see dispatcher.log: %w", err)
}

func (cfg LaunchConfig) spawn(state *os.Root, executable string, deadline time.Time) (*os.Process, error) {
	logFile, err := spool.OpenServerFile(state, "dispatcher.log", os.O_WRONLY|os.O_APPEND|os.O_CREATE)
	if err != nil {
		return nil, err
	}
	defer logFile.Close()
	cmd := exec.Command(executable, dispatcherSubcommand())
	cmd.Dir = cfg.StateRoot
	cmd.Env = cfg.dispatcherEnv()
	if !deadline.IsZero() {
		now, err := monotonicNanos()
		if err != nil {
			return nil, err
		}
		cutoff := now + time.Until(deadline).Nanoseconds()
		cmd.Env = append(cmd.Env, lockCutoffEnv+"="+strconv.FormatInt(cutoff, 10))
	}
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd.Process, nil
}

func (cfg LaunchConfig) dispatcherEnv() []string {
	env := []string{"CROSSHALL_STATE_DIR=" + cfg.StateRoot}
	keys := []string{"HOME", "PATH", "TMPDIR", "XDG_STATE_HOME", telemetryconfig.TelemetryEnv, telemetryconfig.DoNotTrackEnv, notifyEnv,
		// notify-send reaches the desktop through the session bus.
		"DBUS_SESSION_BUS_ADDRESS", "XDG_RUNTIME_DIR"}
	if allowsTestClock(cfg.Version) {
		keys = append(keys, clockOffsetEnv)
	}
	for _, key := range keys {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	return appendTestDispatcherEnv(env)
}

// prepareDaemonProcess is called in the dedicated dispatcher process. The
// runtime can open its own close-on-exec descriptors during startup.
func prepareDaemonProcess() error {
	syscall.Umask(077)
	entries, err := os.ReadDir("/dev/fd")
	if err != nil {
		return fmt.Errorf("inspect inherited descriptors: %w", err)
	}
	for _, entry := range entries {
		fd, err := strconv.Atoi(entry.Name())
		if err != nil || fd < 3 {
			continue
		}
		flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_GETFD, 0)
		if errno == syscall.EBADF {
			continue
		}
		if errno != 0 {
			return fmt.Errorf("inspect descriptor %d: %w", fd, errno)
		}
		if flags&syscall.FD_CLOEXEC == 0 {
			if err := syscall.Close(fd); err != nil {
				return fmt.Errorf("close inherited descriptor %d: %w", fd, err)
			}
		}
	}
	return nil
}

type readyMarker struct {
	Instance string `json:"instance"`
	Version  int    `json:"v"`
	PID      int    `json:"pid"`
	Lease    bool   `json:"lease,omitempty"`
	Probe    bool   `json:"probe,omitempty"`
	// Hello lists the first-contact intents beyond lookup and join that
	// this dispatcher accepts, and the hook arguments it accepts beyond
	// those an older dispatcher knows. A hook sends one only if it is
	// listed here.
	Hello []string `json:"hello,omitempty"`
}

func (r readyMarker) same(o readyMarker) bool {
	return r.Instance == o.Instance && r.Version == o.Version && r.PID == o.PID &&
		r.Lease == o.Lease && r.Probe == o.Probe && slices.Equal(r.Hello, o.Hello)
}

func newReadyMarker(instance string) readyMarker {
	return readyMarker{Instance: instance, Version: wire.Version, PID: os.Getpid(), Lease: true, Probe: true,
		Hello: []string{wire.HelloStart, wire.HelloLaunchProof}}
}

func (r readyMarker) live(state *os.Root) bool {
	live, _ := r.liveness(state, syscall.Kill)
	return live
}

func (r readyMarker) liveness(state *os.Root, probe func(int, syscall.Signal) error) (bool, error) {
	held, err := lockState(state)
	if err != nil || !held || r.Instance == "" || r.Version != wire.Version || r.PID <= 0 {
		return false, err
	}
	err = probe(r.PID, 0)
	switch {
	case err == nil, errors.Is(err, syscall.EPERM), errors.Is(err, syscall.EACCES):
		return true, nil
	case errors.Is(err, syscall.ESRCH):
		return false, nil
	default:
		return false, err
	}
}

func readReady(rendezvous string) readyMarker {
	dir, err := spool.OpenServerRoot(rendezvous)
	if err != nil {
		return readyMarker{}
	}
	defer dir.Close()
	return readReadyAt(dir)
}

func readReadyAt(dir *os.Root) readyMarker {
	file, err := spool.OpenServerFile(dir, "ready", os.O_RDONLY)
	if err != nil {
		return readyMarker{}
	}
	defer file.Close()
	var marker readyMarker
	if err := json.NewDecoder(io.LimitReader(file, 4096)).Decode(&marker); err != nil {
		return readyMarker{}
	}
	return marker
}

// lockState reports whether a dispatcher holds the lock. A missing file or a
// lock that can be taken means none runs; any other failure is returned,
// because then it is unknown whether one runs.
func lockState(state *os.Root) (bool, error) {
	file, err := spool.OpenServerFile(state, "dispatcher.lock", os.O_RDONLY)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer file.Close()
	err = syscall.Flock(int(file.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return true, nil
	}
	return false, err
}

func prepareNamedSpool(root *os.Root, name string) error {
	dir, err := spool.OpenServerChild(root, name, true)
	if err != nil {
		return err
	}
	defer dir.Close()
	return spool.PrepareServerSpool(dir)
}
