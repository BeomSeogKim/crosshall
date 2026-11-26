package link

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const sshHandshakeTimeout = 10 * time.Second

const discoverCrosshall = `command -v crosshall || { test -x "$HOME/.local/bin/crosshall" && printf '%s\n' "$HOME/.local/bin/crosshall"; }`

type Endpoint struct {
	Target string `json:"target"`
	Path   string `json:"path"`
}

type Enrollment struct {
	Hall    string `json:"hall"`
	Device  string `json:"device"`
	Rotated bool   `json:"rotated"`
}

func ValidateTarget(target string) error {
	invalid := func(r rune) bool {
		return !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._%+-:@[]", r)
	}
	if target == "" || strings.HasPrefix(target, "-") || strings.HasPrefix(target, "@") || strings.HasSuffix(target, "@") ||
		strings.Count(target, "@") > 1 || strings.ContainsFunc(target, invalid) {
		return errors.New("link: specify an SSH host or alias, optionally with user@")
	}
	return nil
}

func validRemotePath(value string) bool {
	return path.IsAbs(value) && value != "/" && path.Clean(value) == value && utf8.ValidString(value) &&
		!strings.ContainsFunc(value, func(r rune) bool { return !unicode.IsPrint(r) })
}

func (e Endpoint) remoteCommand(args ...string) (string, error) {
	if !validRemotePath(e.Path) {
		return "", errors.New("link: the home crosshall executable must have an absolute path")
	}
	var command strings.Builder
	// Earlier homes require this setting before accepting link requests.
	command.WriteString("CROSSHALL_EXPERIMENTAL_LINK=1")
	for _, arg := range append([]string{e.Path}, args...) {
		command.WriteString(" '")
		command.WriteString(strings.ReplaceAll(arg, "'", "'\"'\"'"))
		command.WriteByte('\'')
	}
	return command.String(), nil
}

func DiscoverSSH(ctx context.Context, target string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, sshHandshakeTimeout)
	defer cancel()
	child, err := startSSH(ctx, target, discoverCrosshall, true)
	if err != nil {
		return "", err
	}
	_ = child.stdin.Close()
	const maxPath = 4096
	output, readErr := io.ReadAll(io.LimitReader(child.stdout, maxPath+1))
	if readErr != nil || len(output) > maxPath {
		child.close()
	}
	if waitErr := child.wait(); waitErr != nil && len(output) <= maxPath {
		return "", fmt.Errorf("link: discover crosshall on %s: %w", target, waitErr)
	}
	if readErr != nil {
		return "", fmt.Errorf("link: read crosshall path from %s: %w", target, readErr)
	}
	remotePath, newline := strings.CutSuffix(string(output), "\n")
	if !newline || len(output) > maxPath || !validRemotePath(remotePath) {
		return "", fmt.Errorf("link: %s did not print one absolute crosshall path; check the installation and shell startup output with ssh -T", target)
	}
	return remotePath, nil
}

func ProbeSSH(ctx context.Context, e Endpoint) (Challenge, error) {
	ctx, cancel := context.WithTimeout(ctx, sshHandshakeTimeout)
	defer cancel()
	remote, err := e.remoteCommand("link", "serve")
	if err != nil {
		return Challenge{}, err
	}
	child, err := startSSH(ctx, e.Target, remote, true)
	if err != nil {
		return Challenge{}, err
	}
	var challenge Challenge
	err = expect(NewReader(child.stdout), TypeChallenge, &challenge)
	if !errors.Is(err, io.EOF) {
		child.close()
	}
	waitErr := child.wait()
	if ctx.Err() != nil {
		err = ctx.Err()
	} else if errors.Is(err, io.EOF) && waitErr != nil {
		err = waitErr
	}
	if err != nil {
		return Challenge{}, fmt.Errorf("link: probe SSH home %s: %w", e.Target, err)
	}
	if challenge.Hall == "" || !ValidNonce(challenge.Nonce) || challenge.Link.Min < 1 || challenge.Link.Max < challenge.Link.Min {
		return Challenge{}, fmt.Errorf("link: %s sent an invalid home challenge", e.Target)
	}
	challenge.Release = Printable(challenge.Release)
	return challenge, nil
}

func EnrollSSH(ctx context.Context, e Endpoint, name string, secret []byte, opKey string) (Enrollment, error) {
	if len(secret) < 32 || len(secret) > 2047 {
		return Enrollment{}, errors.New("link: invalid enrollment secret length")
	}
	ctx, cancel := context.WithTimeout(ctx, sshHandshakeTimeout)
	defer cancel()
	var args []string
	if opKey != "" {
		args = append(args, "--key", opKey)
	}
	remote, err := e.remoteCommand(append(args, "link", "enroll", "--name", name)...)
	if err != nil {
		return Enrollment{}, err
	}
	// Until enrollment returns, either output stream could echo the secret.
	child, err := startSSH(ctx, e.Target, remote, false)
	if err != nil {
		return Enrollment{}, err
	}
	_, writeErr := io.WriteString(child.stdin, hex.EncodeToString(secret)+"\n")
	_ = child.stdin.Close()
	var enrolled Enrollment
	readErr := writeErr
	if writeErr == nil {
		reader := NewReader(child.stdout)
		readErr = expect(reader, "enrolled", &enrolled)
		if readErr == nil {
			if _, err := reader.Read(); !errors.Is(err, io.EOF) {
				readErr = errors.New("extra output after the enrollment frame")
			}
		}
	}
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		child.close()
	}
	waitErr := child.wait()
	if ctx.Err() != nil {
		return Enrollment{}, fmt.Errorf("link: enroll on %s: %w", e.Target, ctx.Err())
	}
	if errors.Is(readErr, ErrStray) {
		return Enrollment{}, fmt.Errorf("link: enroll on %s: %w; check ssh -T and shell startup output", e.Target, ErrStray)
	}
	if errors.Is(readErr, io.EOF) && waitErr != nil {
		return Enrollment{}, fmt.Errorf("link: enroll on %s: %w", e.Target, waitErr)
	}
	if readErr != nil || enrolled.Hall == "" || enrolled.Device == "" {
		return Enrollment{}, fmt.Errorf("link: %s did not return exactly one valid enrollment frame", e.Target)
	}
	if waitErr != nil {
		return Enrollment{}, fmt.Errorf("link: enroll on %s: %w", e.Target, waitErr)
	}
	return enrolled, nil
}

type SSHConnection struct {
	Session
	Reader *Reader
	Writer *Writer
	child  *sshChild
}

// Wait belongs to one caller, after use of the frame streams ends.
func (c *SSHConnection) Wait() error { return c.child.wait() }

// Close interrupts stream operations and leaves the caller responsible for Wait.
func (c *SSHConnection) Close() { c.child.close() }

func DialSSH(ctx context.Context, e Endpoint, spoke Spoke) (*SSHConnection, error) {
	return dialSSH(ctx, e, spoke, sshHandshakeTimeout)
}

func dialSSH(ctx context.Context, e Endpoint, spoke Spoke, timeout time.Duration) (*SSHConnection, error) {
	remote, err := e.remoteCommand("link", "serve")
	if err != nil {
		return nil, err
	}
	child, err := startSSH(ctx, e.Target, remote, true)
	if err != nil {
		return nil, err
	}
	// The child follows the connection's context. Only the handshake gets
	// this additional deadline so a healthy idle link can stay open.
	handshake, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	interrupted := make(chan struct{})
	stop := context.AfterFunc(handshake, func() {
		child.close()
		close(interrupted)
	})
	r, w := NewReader(child.stdout), NewWriter(child.sent)
	spoke.Target = e.Target
	session, err := spoke.Connect(r, w)
	var waitErr error
	if err != nil {
		if !errors.Is(err, io.EOF) {
			child.close()
		}
		waitErr = child.wait()
	}
	if !stop() {
		<-interrupted
	}
	if handshake.Err() != nil {
		if err == nil {
			child.close()
			_ = child.wait()
		}
		return nil, fmt.Errorf("link: SSH handshake with %s: %w", e.Target, handshake.Err())
	}
	if err != nil {
		if errors.Is(err, io.EOF) && waitErr != nil {
			err = waitErr
		}
		return nil, fmt.Errorf("link: SSH handshake with %s: %w", e.Target, err)
	}
	return &SSHConnection{Session: session, Reader: r, Writer: w, child: child}, nil
}

type sshChild struct {
	ctx    context.Context
	cmd    *exec.Cmd
	stdin  *os.File // pipes, so a frame can be bounded by a deadline
	stdout *os.File
	sent   *countingWriter
	stderr sshDiagnostics
}

// countingWriter counts the bytes that reached the pipe.
type countingWriter struct {
	w io.Writer
	n int
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += n
	return n, err
}

// SendFrame writes one frame by the deadline. sent is false only when no
// byte of it reached the link, so the frame certainly did not leave.
func (c *SSHConnection) SendFrame(typ string, body any, deadline time.Time) (sent bool, err error) {
	return sendFrame(c.child.stdin, c.child.sent, c.Writer, typ, body, deadline)
}

// ReceiveFrame reads one frame by the deadline.
func (c *SSHConnection) ReceiveFrame(deadline time.Time) (Frame, error) {
	return receiveFrame(c.child.stdout, c.Reader, deadline)
}

func sendFrame(pipe *os.File, count *countingWriter, w *Writer, typ string, body any, deadline time.Time) (bool, error) {
	if err := pipe.SetWriteDeadline(deadline); err != nil {
		return false, err
	}
	count.n = 0
	err := w.Write(typ, body)
	return count.n > 0, err
}

func receiveFrame(pipe *os.File, r *Reader, deadline time.Time) (Frame, error) {
	if err := pipe.SetReadDeadline(deadline); err != nil {
		return Frame{}, err
	}
	return r.Read()
}

func startSSH(ctx context.Context, target, remote string, diagnostics bool) (*sshChild, error) {
	if err := ValidateTarget(target); err != nil {
		return nil, err
	}
	executable := "ssh"
	if override := sshTestOverride(); override != "" {
		if !filepath.IsAbs(override) {
			return nil, errors.New("link: CROSSHALL_LINK_SSH must be an absolute executable path")
		}
		executable = override
	}
	cmd := exec.CommandContext(ctx, executable, "-T", "-o", "BatchMode=yes", "-o", "ForwardAgent=no",
		"-o", "ClearAllForwardings=yes", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3", "--", target, remote)
	child := &sshChild{ctx: ctx, cmd: cmd}
	if diagnostics {
		cmd.Stderr = &child.stderr
	}
	stdinRead, stdin, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	stdout, stdoutWrite, err := os.Pipe()
	if err != nil {
		_ = stdinRead.Close()
		_ = stdin.Close()
		return nil, err
	}
	cmd.Stdin, cmd.Stdout = stdinRead, stdoutWrite
	child.stdin, child.stdout, child.sent = stdin, stdout, &countingWriter{w: stdin}
	cmd.Cancel = func() error {
		_ = stdin.Close()
		_ = stdout.Close()
		return cmd.Process.Kill()
	}
	cmd.WaitDelay = 250 * time.Millisecond
	err = cmd.Start()
	// The child holds its own ends now.
	_ = stdinRead.Close()
	_ = stdoutWrite.Close()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("link: start SSH to %s: %w", target, err)
	}
	return child, nil
}

func (c *sshChild) close() { _ = c.cmd.Cancel() }

func (c *sshChild) wait() error {
	err := c.cmd.Wait()
	// The parent ends are this process's own pipes: cmd.Wait closes only
	// the child's.
	_ = c.stdin.Close()
	_ = c.stdout.Close()
	if c.ctx.Err() != nil {
		return c.ctx.Err()
	}
	if err != nil {
		if diagnostic := Printable(strings.TrimSpace(string(c.stderr))); diagnostic != "" {
			return fmt.Errorf("%w: %s", err, diagnostic)
		}
	}
	return err
}

type sshDiagnostics []byte

func (d *sshDiagnostics) Write(p []byte) (int, error) {
	const limit = 1024
	*d = append(*d, p[:min(len(p), limit-len(*d))]...)
	return len(p), nil
}
