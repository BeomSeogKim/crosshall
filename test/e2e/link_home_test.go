//go:build e2e

package e2e

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/link"
)

// The home side of a device link through the real binary and dispatcher
// (#400): enroll a device, then run the handshake and a ping against
// `crosshall link serve` on its stdio.
func TestLinkHomeEnrollsADeviceAndServesItsLink(t *testing.T) {
	w := newWorld(t)
	owner := w.owner()
	owner.extra = []string{"CROSSHALL_EXPERIMENTAL_LINK=1"}
	owner.must("owner", "start")
	secret := bytes.Repeat([]byte{0x5a}, 32)

	enroll := exec.Command(binary, "link", "enroll", "--name", "macbook")
	enroll.Dir, enroll.Env = w.home, owner.env()
	enroll.Stdin = strings.NewReader(hex.EncodeToString(secret) + "\n")
	var out, errOut bytes.Buffer
	enroll.Stdout, enroll.Stderr = &out, &errOut
	if err := enroll.Run(); err != nil {
		t.Fatalf("link enroll: %v\n%s", err, errOut.String())
	}
	r := link.NewReader(&out)
	f, err := r.Read()
	var enrolled struct{ Hall, Device string }
	if err != nil || f.Type != "enrolled" || json.Unmarshal(f.Body, &enrolled) != nil || enrolled.Device == "" {
		t.Fatalf("enroll frame %+v, %v", f, err)
	}
	if _, err := r.Read(); err != io.EOF {
		t.Fatalf("link enroll wrote more than one frame: %v", err)
	}

	serve := exec.Command(binary, "--json", "link", "serve")
	serve.Dir, serve.Env = w.home, owner.env()
	stdin, _ := serve.StdinPipe()
	stdout, _ := serve.StdoutPipe()
	var serveErr bytes.Buffer
	serve.Stderr = &serveErr
	if err := serve.Start(); err != nil {
		t.Fatal(err)
	}
	spoke := link.Spoke{Target: "home", Hall: enrolled.Hall, Device: enrolled.Device, Secret: secret,
		Release: "test", Link: link.Supported()}
	lr, lw := link.NewReader(stdout), link.NewWriter(stdin)
	s, err := spoke.Connect(lr, lw)
	if err != nil {
		t.Fatalf("connect: %v\n%s", err, serveErr.String())
	}
	if s.Link != link.Version {
		t.Fatalf("session %+v", s)
	}
	if err := lw.Write("ping", map[string]int{"n": 1}); err != nil {
		t.Fatal(err)
	}
	if f, err := lr.Read(); err != nil || f.Type != "pong" {
		t.Fatalf("pong %+v, %v", f, err)
	}
	stdin.Close()
	done := make(chan error, 1)
	go func() { done <- serve.Wait() }()
	select {
	case err := <-done:
		if err != nil || serveErr.Len() != 0 {
			t.Fatalf("link serve: %v, stderr %q", err, serveErr.String())
		}
	case <-time.After(10 * time.Second):
		serve.Process.Kill()
		t.Fatal("link serve did not exit when the spoke hung up")
	}

	// A device with another secret gets no welcome, and stdout stays frames.
	bad := exec.Command(binary, "--json", "link", "serve")
	bad.Dir, bad.Env = w.home, owner.env()
	bin, _ := bad.StdinPipe()
	bout, _ := bad.StdoutPipe()
	var badErr bytes.Buffer
	bad.Stderr = &badErr
	if err := bad.Start(); err != nil {
		t.Fatal(err)
	}
	spoke.Secret = bytes.Repeat([]byte{1}, 32)
	if _, err := spoke.Connect(link.NewReader(bout), link.NewWriter(bin)); err == nil || !strings.Contains(err.Error(), "did not prove") {
		t.Fatalf("wrong secret: %v", err)
	}
	bin.Close()
	bad.Wait()
}

// Without the opt-in, link refuses on stderr and writes nothing to stdout,
// with --json too.
func TestLinkNeedsTheOptIn(t *testing.T) {
	w := newWorld(t)
	cmd := exec.Command(binary, "--json", "link", "serve")
	cmd.Dir, cmd.Env = w.home, w.owner().env()
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err == nil || out.Len() != 0 || !strings.Contains(errOut.String(), "CROSSHALL_EXPERIMENTAL_LINK=1") {
		t.Fatalf("err %v, stdout %q, stderr %q", err, out.String(), errOut.String())
	}
}
