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
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

const startupRepoDir = "hook-start-repos"

type parsedStartup struct {
	done  chan struct{}
	event event
	err   error
}

func prepareStartupRepo(entry time.Time, args []string, input io.Reader, locate func() (Paths, error)) *parsedStartup {
	if args[1] != "SessionStart" {
		return nil
	}
	m, ok := loadManifest(args[0])
	start, known := m.event(args[1])
	prompt, _ := m.event("UserPromptSubmit")
	if !ok || !known || !start.Enroll || !prompt.RecoverEnroll {
		return nil
	}
	parsed := &parsedStartup{done: make(chan struct{})}
	go func() {
		defer close(parsed.done)
		ctx, cancel := context.WithDeadline(context.Background(), entry.Add(Budget-counterAllowance))
		defer cancel()
		if hookExpired(ctx) {
			parsed.err = context.DeadlineExceeded
			return
		}
		parsed.event, parsed.err = parseEvent(input, m, start, os.LookupEnv)
		if parsed.err == nil && parsed.event.Source == "startup" && !hookExpired(ctx) {
			if paths, err := locate(); err == nil {
				parsed.event.startupRepo = recordStartupRepo(ctx, paths, parsed.event)
			}
		}
	}()
	return parsed
}

func recordStartupRepo(ctx context.Context, paths Paths, e event) *string {
	state, err := ident.OpenPrivateDir(paths.State)
	if err != nil {
		return nil
	}
	defer state.Close()
	if !startupPolicyEnabled(paths, state, e.Identity.Vendor) || hookExpired(ctx) {
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
	if hookExpired(ctx) {
		return &repo
	}
	temp := wire.NewID() + ".part"
	file, err := records.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return &repo
	}
	defer records.Remove(temp)
	_, err = file.WriteString(repoDigestRecord(repo))
	if errors.Join(err, file.Close()) == nil && !hookExpired(ctx) {
		_ = records.Link(temp, name)
	}
	return &repo
}

func startupPolicyEnabled(paths Paths, state *os.Root, adapter string) bool {
	if rv, err := ident.OpenPrivateDir(paths.Rendezvous); err == nil {
		defer rv.Close()
		if policy, err := readPolicy(rv, adapter); err == nil {
			return policy.Enabled
		}
	}
	policy, err := readPolicyFile(state, StatePolicyName, adapter)
	return err == nil && policy.Enabled
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

func repoDigestRecord(repo string) string {
	if repo == "" {
		return "-\n"
	}
	return fmt.Sprintf("%x\n", sha256.Sum256([]byte(repo)))
}
