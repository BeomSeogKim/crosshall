package hook

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

const startupRepoDir = "hook-start-repos"

// Local bookkeeping can finish after a delayed SessionStart loses its RPC budget.
const startupRepoGrace = 25 * time.Millisecond

type parsedLifecycle struct {
	done   chan struct{}
	cancel context.CancelFunc
	event  event
	err    error
}

func prepareRepoLifecycle(args []string, input io.Reader, locate func() (Paths, error)) *parsedLifecycle {
	if args[1] != "SessionStart" && args[1] != "SessionEnd" {
		return nil
	}
	m, ok := loadManifest(args[0])
	spec, known := m.event(args[1])
	// Every adapter that enrols keeps the record: a later start from
	// another place must not join the session there.
	if !ok || !known || (!spec.Enroll && spec.Fact != wire.HookSessionEnded) {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	parsed := &parsedLifecycle{done: make(chan struct{}), cancel: cancel}
	go func() {
		defer close(parsed.done)
		parsed.event, parsed.err = parseEvent(input, m, spec, os.LookupEnv)
		if parsed.err != nil || ctx.Err() != nil {
			return
		}
		paths, err := locate()
		if err != nil || ctx.Err() != nil {
			return
		}
		state, err := ident.OpenPrivateDir(paths.State)
		if err != nil {
			return
		}
		defer state.Close()
		if spec.Enroll {
			parsed.event.startupRepo = recordStartupRepo(ctx, state, parsed.event)
		} else if spec.Fact == wire.HookSessionEnded && ctx.Err() == nil {
			_ = RemoveStartupRepo(state, parsed.event.Identity)
		}
	}()
	return parsed
}

func (p *parsedLifecycle) waitUntil(deadline time.Time) bool {
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-p.done:
		return true
	case <-timer.C:
		return false
	}
}

func recordStartupRepo(ctx context.Context, state *os.Root, e event) *string {
	if ctx.Err() != nil {
		return nil
	}
	if err := state.Mkdir(startupRepoDir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil
	}
	records, err := ident.OpenPrivateChild(state, startupRepoDir)
	if err != nil {
		return nil
	}
	defer records.Close()
	name := e.Identity.Digest()
	if _, err := records.Lstat(name); !errors.Is(err, os.ErrNotExist) {
		return nil
	}
	repo := enrollmentRoot(e)
	if ctx.Err() != nil {
		return &repo
	}
	temp := wire.NewID() + ".part"
	file, err := records.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return &repo
	}
	defer records.Remove(temp)
	_, err = file.WriteString(repoDigestRecord(repo))
	if errors.Join(err, file.Close()) == nil && ctx.Err() == nil {
		_ = records.Link(temp, name)
	}
	return &repo
}

func startupRepoDigest(stateRoot string, identity ident.Identity) string {
	state, err := ident.OpenPrivateDir(stateRoot)
	if err != nil {
		return ""
	}
	defer state.Close()
	records, err := ident.OpenPrivateChild(state, startupRepoDir)
	if err != nil {
		return ""
	}
	defer records.Close()
	record, err := ident.ReadPrivateFile(records, identity.Digest(), sha256.Size*2+1)
	if err != nil {
		return ""
	}
	return string(record)
}

// RemoveStartupRepo retires the local enrollment record for the session.
func RemoveStartupRepo(state *os.Root, identity ident.Identity) error {
	records, err := ident.OpenPrivateChild(state, startupRepoDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer records.Close()
	name := identity.Digest()
	file, err := spool.OpenServerFile(records, name, os.O_RDONLY)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	if err := records.Remove(name); !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func repoDigestRecord(repo string) string {
	if repo == "" {
		return "-\n"
	}
	return fmt.Sprintf("%x\n", sha256.Sum256([]byte(repo)))
}
