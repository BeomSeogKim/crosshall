package proc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// PublishPeer holds the kernel-verified process identity of a connected socket.
type PublishPeer struct {
	PID     int64
	StartMS int64
}

// DialVerifiedSocket connects to socketPath without writing, verifies the path
// is a user-owned Unix socket (not a symlink), reads the listener PID from the
// kernel, and checks that PID's OS start time against launchRef and
// startedHookAtMS. The caller owns the returned *net.UnixConn on success.
func DialVerifiedSocket(ctx context.Context, socketPath, launchRef string, startedHookAtMS int64) (*net.UnixConn, PublishPeer, error) {
	process, ok := LaunchProcess(launchRef)
	if !ok || startedHookAtMS <= 0 {
		return nil, PublishPeer{}, errors.New("invalid launch reference")
	}
	wantPID, err := strconv.Atoi(strings.TrimPrefix(process, "pid-"))
	if err != nil || wantPID <= 0 {
		return nil, PublishPeer{}, errors.New("invalid launch PID")
	}
	fi, err := os.Lstat(socketPath)
	if err != nil {
		return nil, PublishPeer{}, err
	}
	if fi.Mode()&os.ModeSymlink != 0 || fi.Mode()&os.ModeSocket == 0 {
		return nil, PublishPeer{}, fmt.Errorf("%q is not a Unix socket", socketPath)
	}
	stat, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return nil, PublishPeer{}, fmt.Errorf("%q is not owned by the current user", socketPath)
	}
	raw, err := (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
	if err != nil {
		return nil, PublishPeer{}, err
	}
	conn, ok := raw.(*net.UnixConn)
	if !ok {
		raw.Close()
		return nil, PublishPeer{}, errors.New("dialed connection is not a Unix socket")
	}
	sc, err := conn.SyscallConn()
	if err != nil {
		conn.Close()
		return nil, PublishPeer{}, err
	}
	var pid int
	var peerErr error
	if err := sc.Control(func(fd uintptr) {
		pid, peerErr = peerPID(fd)
	}); err != nil {
		conn.Close()
		return nil, PublishPeer{}, err
	}
	if peerErr != nil {
		conn.Close()
		return nil, PublishPeer{}, peerErr
	}
	if pid <= 0 || pid != wantPID {
		conn.Close()
		return nil, PublishPeer{}, fmt.Errorf("socket peer PID %d does not match launch PID %d", pid, wantPID)
	}
	startMS, err := processStartMS(pid)
	if err != nil {
		conn.Close()
		return nil, PublishPeer{}, err
	}
	if startMS > startedHookAtMS {
		conn.Close()
		return nil, PublishPeer{}, fmt.Errorf("socket peer PID %d started at %d after launch hook %d", pid, startMS, startedHookAtMS)
	}
	return conn, PublishPeer{PID: int64(pid), StartMS: startMS}, nil
}

// VerifySocket connects, verifies the listener's kernel PID and OS start time
// without writing, and closes the descriptor.
func VerifySocket(ctx context.Context, socketPath, launchRef string, startedHookAtMS int64) (PublishPeer, error) {
	conn, peer, err := DialVerifiedSocket(ctx, socketPath, launchRef, startedHookAtMS)
	if err != nil {
		return PublishPeer{}, err
	}
	_ = conn.Close()
	return peer, nil
}
