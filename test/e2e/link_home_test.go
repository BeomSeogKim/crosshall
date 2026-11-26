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

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
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

	serve := exec.CommandContext(t.Context(), binary, "--json", "link", "serve")
	serve.Dir, serve.Env = w.home, owner.env()
	stdin, _ := serve.StdinPipe()
	stdout, _ := serve.StdoutPipe()
	var serveErr bytes.Buffer
	serve.Stderr = &serveErr
	if err := serve.Start(); err != nil {
		t.Fatal(err)
	}
	// One goroutine owns Wait; waited closes when the child is reaped.
	waited := make(chan struct{})
	var waitErr error
	go func() { waitErr = serve.Wait(); close(waited) }()
	t.Cleanup(func() {
		serve.Process.Kill()
		<-waited
	})
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
	select {
	case <-waited:
		if waitErr != nil || serveErr.Len() != 0 {
			t.Fatalf("link serve: %v, stderr %q", waitErr, serveErr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("link serve did not exit when the spoke hung up")
	}

	// A device with another secret gets no welcome, and stdout stays frames.
	bad := exec.CommandContext(t.Context(), binary, "--json", "link", "serve")
	bad.Dir, bad.Env = w.home, owner.env()
	bin, _ := bad.StdinPipe()
	bout, _ := bad.StdoutPipe()
	var badErr bytes.Buffer
	bad.Stderr = &badErr
	if err := bad.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { bad.Process.Kill(); bad.Wait() })
	spoke.Secret = bytes.Repeat([]byte{1}, 32)
	if _, err := spoke.Connect(link.NewReader(bout), link.NewWriter(bin)); err == nil || !strings.Contains(err.Error(), "did not prove") {
		t.Fatalf("wrong secret: %v", err)
	}
	bin.Close()
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

// servedLink is a running `crosshall link serve` with an authenticated
// spoke on its stdio. One goroutine owns Wait, so a failing test reaps
// the child predictably.
type servedLink struct {
	r      *link.Reader
	w      *link.Writer
	stdin  io.Closer
	done   chan struct{} // closed once the child is reaped
	stderr *bytes.Buffer
}

func serveLink(t *testing.T, w *world, owner *participant, spoke link.Spoke) *servedLink {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), binary, "link", "serve")
	cmd.Dir, cmd.Env = w.home, owner.env()
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	s := &servedLink{stdin: stdin, done: make(chan struct{}), stderr: &bytes.Buffer{}}
	cmd.Stderr = s.stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { cmd.Wait(); close(s.done) }()
	t.Cleanup(func() {
		cmd.Process.Kill()
		<-s.done
	})
	s.r, s.w = link.NewReader(stdout), link.NewWriter(stdin)
	if _, err := spoke.Connect(s.r, s.w); err != nil {
		t.Fatalf("connect: %v\n%s", err, s.stderr)
	}
	return s
}

func (s *servedLink) call(t *testing.T, c domain.LinkCall) core.Result {
	t.Helper()
	if err := s.w.Write("call", c); err != nil {
		t.Fatal(err)
	}
	f, err := s.r.Read()
	var r core.Result
	if err != nil || f.Type != "result" || json.Unmarshal(f.Body, &r) != nil {
		t.Fatalf("result %+v, %v\n%s", f, err, s.stderr)
	}
	return r
}

func enrollDevice(t *testing.T, w *world, owner *participant, name string, secret []byte) (hall, device string) {
	t.Helper()
	cmd := exec.Command(binary, "link", "enroll", "--name", name)
	cmd.Dir, cmd.Env = w.home, owner.env()
	cmd.Stdin = strings.NewReader(hex.EncodeToString(secret) + "\n")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	f, err := link.NewReader(bytes.NewReader(out)).Read()
	var e struct{ Hall, Device string }
	if err != nil || json.Unmarshal(f.Body, &e) != nil {
		t.Fatalf("enroll frame %+v, %v", f, err)
	}
	return e.Hall, e.Device
}

// A linked device's member joins and asks through the real link serve and
// dispatcher; refused join forms leave no trace; once the device enrolls
// again, the old open link is refused and closed.
func TestLinkServeForwardsMemberCommands(t *testing.T) {
	w := newWorld(t)
	owner := w.owner()
	owner.extra = []string{"CROSSHALL_EXPERIMENTAL_LINK=1"}
	owner.must("owner", "start")
	owner.must("owner", "channel", "create", "checkout")
	secret := bytes.Repeat([]byte{0x33}, 32)
	hall, device := enrollDevice(t, w, owner, "macbook", secret)
	spoke := link.Spoke{Target: "home", Hall: hall, Device: device, Secret: secret, Release: "test", Link: link.Supported()}
	s := serveLink(t, w, owner, spoke)

	base := domain.LinkCall{Vendor: "claude", Key: "k1", LeafGeneration: "g1", LeafEpoch: 1}
	join := base
	join.Command, join.Args, join.OpKey = "join", json.RawMessage(`{"channel":"checkout","alias":"remote"}`), "j1"
	// The frame's device and credential are ignored: the link supplies its own.
	join.Device, join.CredentialGen = "someone-else", 99
	r := s.call(t, join)
	if r.Status != core.ExitOK || r.Membership == nil {
		t.Fatalf("join %+v", r)
	}
	for name, args := range map[string]string{
		"launch":    `{"channel":"checkout","launch":{"ref":"x"}}`,
		"takeover":  `{"channel":"checkout","alias":"remote","takeover":true}`,
		"repo root": `{"channel":"checkout","repo_root":"/Users/someone/repo"}`,
	} {
		c := base
		c.Command, c.Args, c.OpKey, c.Key = "join", json.RawMessage(args), "x-"+name, "k-"+name
		if r := s.call(t, c); r.Code != domain.CodeNotOnADevice {
			t.Errorf("%s: %+v", name, r)
		}
	}
	who := owner.must("owner", "who", "--all")
	if !strings.Contains(who.stdout, "remote") || strings.Contains(who.stdout, "k-launch") {
		t.Fatalf("owner who:\n%s", who.stdout)
	}

	// Enrolling the device again retires this link's credential.
	enrollDevice(t, w, owner, "macbook", bytes.Repeat([]byte{0x44}, 32))
	c := base
	c.Command, c.Membership = "who", r.Membership
	if r := s.call(t, c); r.Code != domain.CodeLinkStale {
		t.Fatalf("old link after re-enrollment: %+v", r)
	}
	select {
	case <-s.done:
	case <-time.After(10 * time.Second):
		t.Fatal("link serve kept a stale link open")
	}
}
