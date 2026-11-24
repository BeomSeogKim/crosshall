package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

// A retired key's explicit command names the takeover; its hooks never
// look, since the lookup scans the event log.
func TestOnlyExplicitCommandsNameATakeover(t *testing.T) {
	retired, _ := identityRefusal(&ident.EnvironmentError{Code: core.CodeKeyRetired, Message: "session identity is retired"})
	given := func(string) (string, error) { return "@vega was given to another session (takeover 7) at 16:50", nil }
	got, err := nameTakeover(wire.Request{Verb: domain.VerbStatus, KeyDigest: "d"}, retired, given)
	if err != nil || !strings.Contains(string(got.Data), "takeover 7") || got.Code != core.CodeKeyRetired {
		t.Fatalf("explicit command: %+v %v", got, err)
	}
	refuse := func(string) (string, error) { return "", errors.New("a hook read the event log") }
	for _, verb := range []string{wire.VerbHookEvent, wire.VerbHookEnroll} {
		if got, err := nameTakeover(wire.Request{Verb: verb, KeyDigest: "d"}, retired, refuse); err != nil || got.Code != core.CodeKeyRetired {
			t.Fatalf("hook %s: %+v %v", verb, got, err)
		}
	}
}
