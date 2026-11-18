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
	"strconv"
	"syscall"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

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
	return cfg.start(ctx, syscall.Kill)
}

func (cfg LaunchConfig) start(ctx context.Context, probe func(int, syscall.Signal) error) (bool, error) {
	if err := CheckStateRoot(cfg.Version, cfg.StateRoot, cfg.DefaultStateRoot); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if !filepath.IsAbs(cfg.StateRoot) || !filepath.IsAbs(cfg.Rendezvous) {
		return false, errors.New("dispatcher paths must be absolute")
	}
	if cfg.Owner {
		if err := os.MkdirAll(cfg.StateRoot, 0700); err != nil {
			return false, err
		}
		// Newly created components now have filesystem identities, including
		// names the filesystem considers equivalent despite different spelling.
		if err := CheckStateRoot(cfg.Version, cfg.StateRoot, cfg.DefaultStateRoot); err != nil {
			return false, err
		}
	}
	state, err := spool.OpenServerRoot(cfg.StateRoot)
	if err != nil {
		return false, err
	}
	defer state.Close()
	if cfg.Owner {
		if err := prepareNamedSpool(state, "owner"); err != nil {
			return false, err
		}
	}
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
	process, err := cfg.spawn(state, executable)
	if err != nil {
		return false, err
	}
	defer process.Release()

	wait, cancel := context.WithTimeout(ctx, 3*time.Second)
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
			return true, fmt.Errorf("dispatcher did not become ready; see dispatcher.log: %w", wait.Err())
		case <-ticker.C:
		}
	}
}

func (cfg LaunchConfig) spawn(state *os.Root, executable string) (*os.Process, error) {
	logFile, err := spool.OpenServerFile(state, "dispatcher.log", os.O_WRONLY|os.O_APPEND|os.O_CREATE)
	if err != nil {
		return nil, err
	}
	defer logFile.Close()
	cmd := exec.Command(executable, "__dispatch")
	cmd.Dir = cfg.StateRoot
	cmd.Env = cfg.dispatcherEnv()
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd.Process, nil
}

func (cfg LaunchConfig) dispatcherEnv() []string {
	env := []string{"CROSSHALL_STATE_DIR=" + cfg.StateRoot}
	keys := []string{"HOME", "PATH", "TMPDIR", "XDG_STATE_HOME"}
	if allowsTestClock(cfg.Version) {
		keys = append(keys, clockOffsetEnv)
	}
	for _, key := range keys {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	return env
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
