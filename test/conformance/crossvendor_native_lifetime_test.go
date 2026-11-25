//go:build conformance

package conformance

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/proc"
	"github.com/BeomSeogKim/crosshall/internal/testreap"
)

const nativeFixtureWatchEnv = "CROSSHALL_CONFORMANCE_NATIVE_WATCH"

func armNativeFixture(t *testing.T) (root, entry string) {
	t.Helper()
	bin, err := os.Executable()
	codexMust(t, err)
	readLife, writeLife, err := os.Pipe()
	codexMust(t, err)
	defer readLife.Close()
	watch := exec.Command(bin)
	watch.Env = []string{nativeFixtureWatchEnv + "=1", "PATH=/usr/bin:/bin"}
	watch.Stdin, watch.Stderr = readLife, os.Stderr
	watch.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	ready, err := watch.StdoutPipe()
	if err != nil {
		writeLife.Close()
		t.Fatal(err)
	}
	if err := watch.Start(); err != nil {
		writeLife.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		codexMust(t, writeLife.Close())
		codexMust(t, watch.Wait())
	})
	_, err = fmt.Fscan(bufio.NewReader(ready), &root, &entry)
	codexMust(t, err)
	return root, entry
}

func watchNativeFixture() (err error) {
	if runtime.GOOS != "darwin" {
		return errors.New("native fixture watchdog requires macOS")
	}
	// The parent can die before it reads stdout; cleanup still owns the root.
	signal.Ignore(syscall.SIGPIPE)
	physicalTmp, err := filepath.EvalSymlinks("/tmp")
	if err != nil {
		return err
	}
	root, err := os.MkdirTemp(physicalTmp, "xv.")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(root)) }()
	rv := ident.Rendezvous(filepath.Join(root, "state"), os.Geteuid())
	if err := os.Mkdir(rv, 0700); err != nil {
		return fmt.Errorf("reserve fixture rendezvous: %w", err)
	}
	info, err := os.Lstat(rv)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, removeNativeRendezvous(rv, info)) }()
	_, socket := nativeCodexControlSocket(root, physicalTmp)
	for _, path := range []string{socket, socket + ".lock"} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("expected unused fixture socket path %s: %v", path, err)
		}
	}
	entry := testreap.Key + "=" + rand.Text()
	if _, err = fmt.Fprintln(os.Stdout, root, entry); err == nil {
		_, err = io.Copy(io.Discard, os.Stdin)
	}
	// Fence the stock hook commands and the state directory before stopping
	// the sessions. Otherwise an in-flight hook can start another dispatcher.
	fenceErr := os.Chmod(root, 0)
	reapErr := testreap.EndDarwinFixture(entry)
	return errors.Join(err, fenceErr, reapErr, os.Chmod(root, 0700), removeNativeSocket(socket))
}

func removeNativeRendezvous(path string, owned os.FileInfo) error {
	current, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !os.SameFile(current, owned) {
		return fmt.Errorf("fixture rendezvous changed: %s", path)
	}
	return os.RemoveAll(path)
}

func removeNativeSocket(socket string) error {
	var result error
	for _, path := range []string{socket, socket + ".lock"} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			result = errors.Join(result, err)
		}
	}
	return result
}

const nativeFixtureProbeEnv = "CROSSHALL_CONFORMANCE_NATIVE_KILL_PROBE"

type nativeFixtureProbe struct {
	Root, Entry string
	PID         int64
	Start       int64
}

func TestNativeFixtureWatchdogReapsKilledRun(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("native fixture watchdog requires macOS")
	}
	bin, err := os.Executable()
	codexMust(t, err)
	probe := func(ctx context.Context, mode string, env ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, bin, "-test.run", "^TestNativeFixtureWatchdogReapsKilledRun$")
		cmd.Env = append(env, nativeFixtureProbeEnv+"="+mode)
		cmd.Stderr = os.Stderr
		return cmd
	}
	const hold = 30 * time.Second
	switch os.Getenv(nativeFixtureProbeEnv) {
	case "worker":
		time.Sleep(hold)
		return
	case "runner":
		root, entry := armNativeFixture(t)
		worker := probe(t.Context(), "worker", entry)
		codexMust(t, worker.Start())
		start, err := proc.StartOf(worker.Process.Pid)
		codexMust(t, err)
		codexMust(t, json.NewEncoder(os.Stdout).Encode(nativeFixtureProbe{root, entry, int64(worker.Process.Pid), start.Identity}))
		codexMust(t, worker.Wait())
		return
	}

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	runner := probe(ctx, "runner")
	stdout, err := runner.StdoutPipe()
	codexMust(t, err)
	codexMust(t, runner.Start())
	var receipt nativeFixtureProbe
	if err := json.NewDecoder(stdout).Decode(&receipt); err != nil {
		_ = runner.Process.Kill()
		_ = runner.Wait()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if proc.ProcessAlive(receipt.PID, receipt.Start) {
			_ = syscall.Kill(int(receipt.PID), syscall.SIGKILL)
		}
	})
	nearMatch := probe(t.Context(), "worker", receipt.Entry+"-other")
	codexMust(t, nearMatch.Start())
	t.Cleanup(func() { _ = nearMatch.Process.Kill(); _ = nearMatch.Wait() })
	nearStart, err := proc.StartOf(nearMatch.Process.Pid)
	codexMust(t, err)
	codexMust(t, runner.Process.Kill())
	var killed *exec.ExitError
	if err := runner.Wait(); !errors.As(err, &killed) || killed.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatalf("fixture runner did not die from SIGKILL: %v", err)
	}

	rv := ident.Rendezvous(filepath.Join(receipt.Root, "state"), os.Geteuid())
	for deadline := time.Now().Add(7 * time.Second); time.Now().Before(deadline); time.Sleep(40 * time.Millisecond) {
		_, rootErr := os.Lstat(receipt.Root)
		_, rvErr := os.Lstat(rv)
		if errors.Is(rootErr, os.ErrNotExist) && errors.Is(rvErr, os.ErrNotExist) && proc.ProcessGone(receipt.PID, receipt.Start) {
			if !proc.ProcessAlive(int64(nearMatch.Process.Pid), nearStart.Identity) {
				t.Fatal("fixture watchdog stopped a process with a different token")
			}
			return
		}
	}
	t.Fatal("SIGKILL left the fixture worker, temporary home or rendezvous behind")
}
