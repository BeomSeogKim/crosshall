package proc

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

func testSocketDir(t *testing.T) string {
	t.Helper()
	// Unix socket paths are limited to ~104 bytes on macOS.
	dir, err := os.MkdirTemp("", "ch-peer-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestDialVerifiedSocketMatchesSelfAndWritesNothing(t *testing.T) {
	sock := filepath.Join(testSocketDir(t), "self.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	accepted := make(chan []byte, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		b, _ := io.ReadAll(conn)
		accepted <- b
	}()

	start, err := processStartMS(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	ref := "pid-" + strconv.Itoa(os.Getpid()) + "-ab"
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	conn, peer, err := DialVerifiedSocket(ctx, sock, ref, time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("DialVerifiedSocket: %v", err)
	}
	if peer.PID != int64(os.Getpid()) || peer.StartMS != start {
		_ = conn.Close()
		t.Fatalf("peer = %+v, want pid=%d start=%d", peer, os.Getpid(), start)
	}
	_ = conn.Close()

	select {
	case b := <-accepted:
		if len(b) != 0 {
			t.Fatalf("DialVerifiedSocket wrote %d bytes: %q", len(b), b)
		}
	case <-time.After(time.Second):
		t.Fatal("listener did not observe clean close")
	}
}

func TestDialVerifiedSocketRejectsOtherProcessListener(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("requires python3")
	}
	dir := testSocketDir(t)
	sock := filepath.Join(dir, "other.sock")
	ready := filepath.Join(dir, "ready")
	script := `
import os, socket, sys, time
s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
s.bind(sys.argv[1])
s.listen(1)
open(sys.argv[2], "w").write("ok")
time.sleep(10)
`
	cmd := exec.Command("python3", "-c", script, sock, ready)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Expect our own PID, but the listener belongs to the python3 child.
	ref := "pid-" + strconv.Itoa(os.Getpid()) + "-ab"
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, _, err := DialVerifiedSocket(ctx, sock, ref, time.Now().UnixMilli()); err == nil {
		t.Fatal("DialVerifiedSocket accepted a listener owned by another process")
	}
}

func TestDialVerifiedSocketRejectsStaleSocketAndMissingPath(t *testing.T) {
	dir := testSocketDir(t)
	ref := "pid-" + strconv.Itoa(os.Getpid()) + "-ab"
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	missing := filepath.Join(dir, "missing.sock")
	if _, _, err := DialVerifiedSocket(ctx, missing, ref, time.Now().UnixMilli()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing socket err = %v, want ENOENT", err)
	}

	// Create a stale socket file with no listener by binding a raw AF_UNIX socket and closing it.
	stale := filepath.Join(dir, "stale.sock")
	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Bind(fd, &syscall.SockaddrUnix{Name: stale}); err != nil {
		_ = syscall.Close(fd)
		t.Fatal(err)
	}
	_ = syscall.Close(fd)

	if _, _, err := DialVerifiedSocket(ctx, stale, ref, time.Now().UnixMilli()); !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("stale socket err = %v, want ECONNREFUSED", err)
	}
}

func TestDialVerifiedSocketRejectsReusedPIDAndZombie(t *testing.T) {
	sock := filepath.Join(testSocketDir(t), "self.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	start, err := processStartMS(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	ref := "pid-" + strconv.Itoa(os.Getpid()) + "-ab"
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	// Reused PID: process started after the launch's first hook.
	if _, _, err := DialVerifiedSocket(ctx, sock, ref, start-1_000); err == nil {
		t.Fatal("DialVerifiedSocket accepted a reused PID born after the launch hook")
	}

	// Symlink to socket is refused.
	link := filepath.Join(filepath.Dir(sock), "link.sock")
	if err := os.Symlink(sock, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := DialVerifiedSocket(ctx, link, ref, time.Now().UnixMilli()); err == nil {
		t.Fatal("DialVerifiedSocket accepted a symlink")
	}

	// Zombie process: processStartMS refuses an exited, unreaped child.
	child := exec.Command("/bin/sleep", "60")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := processStartMS(child.Process.Pid); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("zombie process was not rejected by processStartMS")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
