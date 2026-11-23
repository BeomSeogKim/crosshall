package client

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/cli"
	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

// A member command names its host; a wait does not: a background wait that
// outlived its agent must not speak for it.
func TestHostIsNamedByMemberCommandsButNotByWait(t *testing.T) {
	for _, c := range []struct {
		command domain.Command
		named   bool
	}{{domain.Who{}, true}, {domain.Wait{}, false}} {
		transport := New(spool.Client{StateRoot: t.TempDir(), Rendezvous: t.TempDir()})
		transport.lookup = func(name string) (string, bool) {
			if name == ident.RouteVariable {
				return strings.Repeat("ab", 16), true
			}
			return "", false
		}
		named := false
		transport.EnableHost(func(ident.Identity, func(string) (string, bool)) *wire.Host {
			named = true
			return &wire.Host{PID: 2, Start: 1}
		})
		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		_, _ = transport.Do(ctx, cli.Call{Command: c.command})
		cancel()
		if named != c.named {
			t.Errorf("%T named a host: %t, want %t", c.command, named, c.named)
		}
	}
}
