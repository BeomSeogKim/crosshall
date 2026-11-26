//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/link"
)

const (
	linkFixturePeerEnv    = "CROSSHALL_E2E_LINK_FIXTURE_PEER"
	linkFixtureHall       = "fixture-hall"
	linkFixtureHome       = "fixture-home-release"
	linkFixtureSpoke      = "fixture-spoke"
	linkFixtureSpokeBuild = "fixture-spoke-release"
	linkFixtureSecret     = "fixture-only-link-proof"
)

func runLinkFixturePeer(mode string) int {
	versions := link.Supported()
	switch mode {
	case "compatible", "bad-home-proof":
	case "out-of-window":
		versions = link.Range{Min: 99, Max: 99}
	case "banner":
		fmt.Fprintln(os.Stdout, "fixture login banner")
		return 0
	default:
		fmt.Fprintln(os.Stderr, "unexpected link fixture peer mode", mode)
		return 2
	}

	var sawHello, sawAuth bool
	home := link.Home{
		Hall: linkFixtureHall, Release: linkFixtureHome, Link: versions,
		Caps: []string{"fixture-shared", "fixture-home-only"},
		Prove: func(device, homeNonce, spokeNonce string) (bool, string, error) {
			sawHello = true
			secret := linkFixtureSecret
			if mode == "bad-home-proof" {
				secret = "different-fixture-link-proof"
			}
			return device == linkFixtureSpoke, link.Proof([]byte(secret), link.RoleHome, linkFixtureHall, device, homeNonce, spokeNonce), nil
		},
		Verify: func(device, homeNonce, spokeNonce, proof string) (bool, error) {
			sawAuth = true
			return link.CheckProof([]byte(linkFixtureSecret), link.RoleSpoke, linkFixtureHall, device, homeNonce, spokeNonce, proof), nil
		},
	}
	got, err := home.Accept(link.NewReader(os.Stdin), link.NewWriter(os.Stdout))
	if mode == "out-of-window" {
		if !sawHello && errors.Is(err, io.EOF) {
			return 0
		}
		fmt.Fprintf(os.Stderr, "out-of-window fixture saw hello %t and error %v\n", sawHello, err)
		return 1
	}
	if mode == "bad-home-proof" {
		if sawHello && !sawAuth && errors.Is(err, io.EOF) {
			return 0
		}
		fmt.Fprintf(os.Stderr, "bad-home-proof fixture saw hello %t, auth %t and error %v\n", sawHello, sawAuth, err)
		return 1
	}
	if err != nil || got.Link != link.Version || got.Device != linkFixtureSpoke || got.PeerRelease != linkFixtureSpokeBuild ||
		!slices.Equal(got.Caps, []string{"fixture-shared"}) {
		fmt.Fprintf(os.Stderr, "unexpected fixture link session %+v: %v\n", got, err)
		return 1
	}
	return 0
}

func TestLinkFixtureHandshakeOverSSHStdio(t *testing.T) {
	peer, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"compatible", "out-of-window", "bad-home-proof", "banner"} {
		t.Run(mode, func(t *testing.T) {
			f := newLinkFixture(t, peer, binary, linkFixturePeerEnv+"="+mode)
			ctx, cancel := context.WithTimeout(t.Context(), fixtureEventTimeout)
			defer cancel()
			cmd := exec.CommandContext(ctx, f.ssh, "-T", "-o", "BatchMode=yes", linkFixtureTarget, peer, "link", "serve")
			cmd.Env = f.spoke.baseEnv()
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			defer stdin.Close()
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}

			spoke := link.Spoke{
				Target: linkFixtureTarget, Hall: linkFixtureHall, Device: linkFixtureSpoke, Secret: []byte(linkFixtureSecret),
				Release: linkFixtureSpokeBuild, Link: link.Supported(), Caps: []string{"fixture-spoke-only", "fixture-shared"},
			}
			got, handshakeErr := spoke.Connect(link.NewReader(stdout), link.NewWriter(stdin))
			_ = stdin.Close()
			if err := cmd.Wait(); err != nil {
				t.Fatalf("fixture home process: %v; stderr %q; spoke error %v", err, stderr.String(), handshakeErr)
			}
			switch mode {
			case "compatible":
				if handshakeErr != nil || got.Link != link.Version || got.PeerRelease != linkFixtureHome ||
					!slices.Equal(got.Caps, []string{"fixture-shared"}) {
					t.Fatalf("unexpected accepted link %+v: %v", got, handshakeErr)
				}
			case "out-of-window":
				if handshakeErr == nil || !strings.Contains(handshakeErr.Error(), linkFixtureHome+" (link 99)") ||
					!strings.Contains(handshakeErr.Error(), "no common link version: upgrade crosshall") {
					t.Fatalf("unexpected version refusal: %v", handshakeErr)
				}
			case "bad-home-proof":
				if handshakeErr == nil || !strings.Contains(handshakeErr.Error(), "did not prove that it holds this device's link") {
					t.Fatalf("unexpected home proof refusal: %v", handshakeErr)
				}
			case "banner":
				if !errors.Is(handshakeErr, link.ErrStray) {
					t.Fatalf("unexpected login banner result: %v", handshakeErr)
				}
			}
		})
	}
}
