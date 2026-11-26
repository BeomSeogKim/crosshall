package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/link"
)

var testSecret = bytes.Repeat([]byte{7}, 32)

// homeTransport answers the link commands as a dispatcher whose hall knows
// one device.
type homeTransport struct{ calls []Call }

func (h *homeTransport) Do(_ context.Context, call Call) (core.Result, error) {
	h.calls = append(h.calls, call)
	switch c := call.Command.(type) {
	case domain.LinkHall:
		return domain.OK(domain.LinkHallResult{Hall: "hall-1"})
	case domain.LinkVerify:
		known := c.Device == "dev-1"
		return domain.OK(domain.LinkVerifyResult{Known: known,
			OK: known && link.CheckProof(testSecret, "hall-1", c.Device, c.Nonce, c.Proof)})
	case domain.LinkEnroll:
		return domain.OK(domain.LinkEnrollResult{Hall: "hall-1", Device: "dev-1"})
	}
	return domain.Fail(core.ExitUsage, "", "unexpected", ""), nil
}

// serve runs `crosshall link serve` with a spoke on the other end of its
// stdio, and returns what the spoke saw, stderr and the exit status.
func serve(t *testing.T, env Env, spoke func(*link.Reader, *link.Writer)) (string, core.ExitCode) {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	env.Stdin = inR
	var errOut bytes.Buffer
	done := make(chan core.ExitCode, 1)
	go func() {
		code := RunEnv(context.Background(), env, []string{"link", "serve"}, outW, &errOut)
		outW.Close()
		done <- code
	}()
	spoke(link.NewReader(outR), link.NewWriter(inW))
	inW.Close()
	io.Copy(io.Discard, outR)
	code := <-done
	return errOut.String(), code
}

func spokeConfig(device string) link.Spoke {
	return link.Spoke{Target: "mini", Hall: "hall-1", Device: device, Secret: testSecret,
		Release: "test", Link: link.Supported()}
}

func TestLinkServeShakesHandsThenAnswersPings(t *testing.T) {
	ht := &homeTransport{}
	stderr, code := serve(t, Env{Version: "v9", Transport: ht, ExperimentalLink: true}, func(r *link.Reader, w *link.Writer) {
		s, err := spokeConfig("dev-1").Connect(r, w)
		if err != nil || s.Link != link.Version || s.PeerRelease != "v9" {
			t.Errorf("connect %+v, %v", s, err)
			return
		}
		if err := w.Write(linkPing, map[string]int{"n": 3}); err != nil {
			t.Error(err)
		}
		f, err := r.Read()
		if err != nil || f.Type != linkPong || string(f.Body) != `{"n":3}` {
			t.Errorf("pong %+v, %v", f, err)
		}
	})
	if code != core.ExitOK || stderr != "" {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	for _, c := range ht.calls {
		if !c.Owner || c.OpKey != "" {
			t.Fatalf("call %+v: want an owner read", c)
		}
	}
}

// A refused spoke gets a refusal frame; the reason goes to stderr, and
// nothing but frames reaches stdout.
func TestLinkServeRefusesAnUnknownDevice(t *testing.T) {
	stderr, code := serve(t, Env{Transport: &homeTransport{}, ExperimentalLink: true}, func(r *link.Reader, w *link.Writer) {
		_, err := spokeConfig("dev-2").Connect(r, w)
		if err == nil || !strings.Contains(err.Error(), "not linked") {
			t.Errorf("connect: %v", err)
		}
	})
	if code == core.ExitOK || !strings.Contains(stderr, "not linked") {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
}

func TestLinkServeStopsAtAFrameItDoesNotKnow(t *testing.T) {
	stderr, code := serve(t, Env{Transport: &homeTransport{}, ExperimentalLink: true}, func(r *link.Reader, w *link.Writer) {
		if _, err := spokeConfig("dev-1").Connect(r, w); err != nil {
			t.Error(err)
		}
		w.Write("wake", map[string]string{})
	})
	if code == core.ExitOK || !strings.Contains(stderr, `no "wake" frame`) {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
}

func TestLinkEnrollReadsTheSecretFromStdinAndAnswersWithAFrame(t *testing.T) {
	ht := &homeTransport{}
	secret := strings.Repeat("ab", 32)
	env := Env{Transport: ht, ExperimentalLink: true, Stdin: strings.NewReader(secret + "\n")}
	stdout, stderr, code := run(t, env, "link", "enroll", "--name", "macbook")
	if code != core.ExitOK || stderr != "" {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	want := domain.LinkEnroll{Name: "macbook", Secret: secret}
	if len(ht.calls) != 1 || ht.calls[0].Command != want || !ht.calls[0].Owner || len(ht.calls[0].OpKey) != 32 {
		t.Fatalf("calls %+v", ht.calls)
	}
	f, err := link.NewReader(strings.NewReader(stdout)).Read()
	var got domain.LinkEnrollResult
	if err != nil || f.Type != linkEnrolled || json.Unmarshal(f.Body, &got) != nil || got.Device != "dev-1" {
		t.Fatalf("frame %+v, %v", f, err)
	}
}

func TestLinkIsOptInAndOwnerOnly(t *testing.T) {
	ht := &homeTransport{}
	if _, stderr, code := run(t, Env{Transport: ht}, "link", "serve"); code != core.ExitUsage ||
		!strings.Contains(stderr, "CROSSHALL_EXPERIMENTAL_LINK=1") {
		t.Fatalf("without the opt-in: exit %d, stderr %q", code, stderr)
	}
	gated := Env{Transport: ht, ExperimentalLink: true, OwnerGate: func() string { return "inside an agent session" }}
	if _, _, code := run(t, gated, "link", "serve"); code != core.ExitRefused {
		t.Fatalf("inside a session: exit %d", code)
	}
	if len(ht.calls) != 0 {
		t.Fatalf("calls %+v", ht.calls)
	}
}
