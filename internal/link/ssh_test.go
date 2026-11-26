//go:build testvendorhome

package link

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	fixtureTarget = "test-user@home"
	fixturePath   = "/fixture dir/cross'hall"
	fixtureServe  = `CROSSHALL_EXPERIMENTAL_LINK=1 '/fixture dir/cross'"'"'hall' 'link' 'serve'`
	fixtureEnroll = `CROSSHALL_EXPERIMENTAL_LINK=1 '/fixture dir/cross'"'"'hall' '--key' 'op-key' 'link' 'enroll' '--name' 'a device'"'"'s name'`
)

var fixtureSecret = bytes.Repeat([]byte{0xab}, 32)

func TestSSHRejectsUnsafeTarget(t *testing.T) {
	for _, target := range []string{"", "-oProxyCommand=echo", "home host", "user@host;pwd", "user@@host", "@home", "user@", "home\n"} {
		if err := ValidateTarget(target); err == nil {
			t.Errorf("accepted unsafe target %q", target)
		}
	}
	for _, target := range []string{"home", fixtureTarget, "2001:db8::4", "test-user@[2001:db8::4]"} {
		if err := ValidateTarget(target); err != nil {
			t.Errorf("rejected target %q: %v", target, err)
		}
	}
}

func TestSSHDiscoveryProbeAndEnrollment(t *testing.T) {
	e := installSSHFixture(t, "ok")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	path, err := DiscoverSSH(ctx, e.Target)
	if err != nil || path != fixturePath {
		t.Fatalf("discovery = %q, %v", path, err)
	}
	challenge, err := ProbeSSH(ctx, e)
	if err != nil || challenge.Hall != "test-hall" || challenge.Link != Supported() {
		t.Fatalf("probe = %+v, %v", challenge, err)
	}
	enrolled, err := EnrollSSH(ctx, e, "a device's name", fixtureSecret, "op-key")
	if err != nil || enrolled != (Enrollment{Hall: "test-hall", Device: "test-device", Rotated: true}) {
		t.Fatalf("enroll = %+v, %v", enrolled, err)
	}
}

func TestSSHRejectsDiscoveryWithExtraOutput(t *testing.T) {
	e := installSSHFixture(t, "banner")
	if _, err := DiscoverSSH(context.Background(), e.Target); err == nil {
		t.Fatal("discovery accepted a path followed by other output")
	}
	if _, err := ProbeSSH(context.Background(), e); !errors.Is(err, ErrStray) {
		t.Fatalf("probe error = %v, want ErrStray", err)
	}
}

func TestSSHEnrollmentDoesNotExposeEchoedSecret(t *testing.T) {
	e := installSSHFixture(t, "echo-enrollment")
	_, err := EnrollSSH(context.Background(), e, "a device's name", fixtureSecret, "op-key")
	if !errors.Is(err, ErrStray) || strings.Contains(err.Error(), "abababab") {
		t.Fatalf("enrollment error = %v, want redacted stray output", err)
	}
}

func TestSSHHandshakeDeadlineForStalledChild(t *testing.T) {
	for _, mode := range []string{"stall", "close-stdout"} {
		t.Run(mode, func(t *testing.T) {
			e := installSSHFixture(t, mode)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_, err := dialSSH(ctx, e, fixtureSpoke(), 150*time.Millisecond)
			if !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
				t.Fatalf("dial error = %v with caller context error %v, want handshake timeout", err, ctx.Err())
			}
		})
	}
}

func TestSSHConnectionOutlivesHandshakeDeadline(t *testing.T) {
	e := installSSHFixture(t, "ok")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	const limit = time.Second
	conn, err := dialSSH(ctx, e, fixtureSpoke(), limit)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cancel()
		if err := conn.Wait(); !errors.Is(err, context.Canceled) {
			t.Errorf("wait error = %v, want caller cancellation", err)
		}
	}()
	time.Sleep(limit)
	if err := conn.Writer.Write("ping", nil); err != nil {
		t.Fatal(err)
	}
	pong, err := conn.Reader.Read()
	if err != nil || pong.Type != "pong" {
		t.Fatalf("after handshake deadline read %+v, %v", pong, err)
	}
}

func fixtureSpoke() Spoke {
	return Spoke{Hall: "test-hall", Device: "test-device", Secret: fixtureSecret, Link: Supported()}
}

func installSSHFixture(t *testing.T, mode string) Endpoint {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(t.TempDir(), "ssh")
	script := "#!/bin/sh\nexec '" + strings.ReplaceAll(self, "'", "'\"'\"'") + "' -test.run '^TestSSHFixturePeer$' -- \"$@\"\n"
	if err := os.WriteFile(shim, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CROSSHALL_LINK_SSH", shim)
	t.Setenv("CROSSHALL_SSH_FIXTURE", mode)
	return Endpoint{Target: fixtureTarget, Path: fixturePath}
}

func TestSSHFixturePeer(t *testing.T) {
	separator := slices.Index(os.Args, "--")
	if separator == -1 {
		return
	}
	args := os.Args[separator+1:]
	want := []string{"-T", "-o", "BatchMode=yes", "-o", "ForwardAgent=no", "-o", "ClearAllForwardings=yes", "-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3", "--", fixtureTarget}
	if len(args) != len(want)+1 || !slices.Equal(args[:len(want)], want) {
		failSSHFixture("unexpected ssh arguments")
	}
	mode := os.Getenv("CROSSHALL_SSH_FIXTURE")
	r, w := NewReader(os.Stdin), NewWriter(os.Stdout)
	switch args[len(want)] {
	case discoverCrosshall:
		fmt.Fprintln(os.Stdout, fixturePath)
		if mode == "banner" {
			fmt.Fprintln(os.Stdout, "fixture banner")
		}
	case fixtureEnroll:
		input, err := io.ReadAll(os.Stdin)
		if err != nil || string(input) != hex.EncodeToString(fixtureSecret)+"\n" {
			failSSHFixture("invalid enrollment stdin")
		}
		if mode == "echo-enrollment" {
			_, _ = os.Stderr.Write(input)
			_, _ = os.Stdout.Write(input)
			os.Exit(1)
		}
		if err := w.Write("enrolled", Enrollment{Hall: "test-hall", Device: "test-device", Rotated: true}); err != nil {
			failSSHFixture("failed to write enrollment")
		}
	case fixtureServe:
		switch mode {
		case "stall", "close-stdout":
			if mode == "close-stdout" {
				_ = os.Stdout.Close()
			}
			time.Sleep(5 * time.Second)
			os.Exit(0)
		case "banner":
			fmt.Fprintln(os.Stdout, "fixture banner")
			os.Exit(0)
		}
		home := Home{
			Hall: "test-hall",
			Link: Supported(),
			Prove: func(device, homeNonce, spokeNonce string) (bool, string, error) {
				return device == "test-device", Proof(fixtureSecret, RoleHome, "test-hall", device, homeNonce, spokeNonce), nil
			},
			Verify: func(device, homeNonce, spokeNonce, proof string) (bool, error) {
				return CheckProof(fixtureSecret, RoleSpoke, "test-hall", device, homeNonce, spokeNonce, proof), nil
			},
		}
		if _, err := home.Accept(r, w); err != nil {
			failSSHFixture("handshake failed")
		}
		ping, err := r.Read()
		if err != nil || ping.Type != "ping" {
			failSSHFixture("expected ping after handshake")
		}
		if err := w.Write("pong", nil); err != nil {
			failSSHFixture("failed to write pong")
		}
		_, _ = r.Read()
	default:
		failSSHFixture("unexpected remote shell command")
	}
	os.Exit(0)
}

func failSSHFixture(message string) {
	fmt.Fprintln(os.Stderr, message)
	os.Exit(2)
}
