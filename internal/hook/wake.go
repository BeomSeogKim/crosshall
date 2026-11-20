package hook

import (
	"context"
	"errors"
	"log"
	"os"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/spool"
)

// Paths locates the hall for one callback.
type Paths struct {
	State, Rendezvous string
	// Start starts a stopped dispatcher and waits for its readiness until
	// the context ends. It reports whether it launched a process.
	Start func(context.Context) (bool, error)
}

// StatePolicyName is the dispatcher's copy of the hook policy in the state
// root. A reboot clears the rendezvous; this copy tells a hook whether its
// adapter is on before any dispatcher has published there again.
const StatePolicyName = "hook-policy"

// startDeadline bounds an enrolling callback that found the dispatcher
// stopped, from its entry to its return: it waits for the start so the
// session joins in this callback, keeping a full Budget for the enrolment
// itself. It stays inside the 1 s timeout the manifests install.
const startDeadline = 800 * time.Millisecond

const startWait = startDeadline - Budget - logAllowance

// spawnWait is what any other callback gives a start: enough to launch the
// process, which goes on starting by itself.
const spawnWait = 20 * time.Millisecond

// logAllowance lets a finished start record its line before the callback
// goes on.
const logAllowance = 10 * time.Millisecond

// After a start that failed, hooks leave the dispatcher alone for
// startRetryGap, so a hall that cannot start does not hold every session
// start. A start not ready in time is not a failure.
const (
	startRetryGap   = 30 * time.Second
	startFailedName = "hook-start-failed"
)

// wake starts a stopped dispatcher when the adapter policy is on. It
// returns the entry time the callback continues from: a fresh one after an
// enrolling callback's start succeeded, else the original. A failed start
// is written to dispatcher.log and the callback goes on as before.
func wake(entry time.Time, args []string, locate func() (Paths, error)) time.Time {
	m, ok := loadManifest(args[0])
	spec, known := m.event(args[1])
	if !ok || !known {
		return entry
	}
	wait := spawnWait
	if spec.Enroll {
		wait = startWait
	}
	stopped := make(chan bool, 1)
	started := make(chan bool, 1)
	go func() {
		if !time.Now().Before(entry.Add(Budget)) {
			stopped <- false
			return
		}
		p, err := locate()
		if err != nil || p.Start == nil || !stoppedAndEnabled(p, m.Adapter) || failedRecently(p.State) ||
			!time.Now().Before(entry.Add(Budget)) {
			stopped <- false
			return
		}
		stopped <- true
		ctx, cancel := context.WithDeadline(context.Background(), entry.Add(wait))
		defer cancel()
		spawned, err := p.Start(ctx)
		logStart(p.State, m.Adapter+" "+args[1], entry, spawned, spec.Enroll, err)
		started <- err == nil
	}()
	// Finding a running dispatcher stays inside the ordinary Budget.
	check := time.NewTimer(time.Until(entry.Add(Budget)))
	defer check.Stop()
	select {
	case s := <-stopped:
		if !s {
			return entry
		}
	case <-check.C:
		return entry
	}
	done := time.NewTimer(time.Until(entry.Add(wait + logAllowance)))
	defer done.Stop()
	select {
	case ok := <-started:
		if ok && spec.Enroll {
			return time.Now()
		}
	case <-done.C:
	}
	return entry
}

// stoppedAndEnabled reads the policy from the rendezvous while it exists,
// else from the state root's copy. No readable policy means off.
func stoppedAndEnabled(p Paths, adapter string) bool {
	if rv, err := ident.OpenPrivateDir(p.Rendezvous); err == nil {
		defer rv.Close()
		if !dispatcherGone(rv) {
			return false
		}
		if policy, err := readPolicy(rv, adapter); err == nil {
			return policy.Enabled
		}
	}
	state, err := ident.OpenPrivateDir(p.State)
	if err != nil {
		return false
	}
	defer state.Close()
	policy, err := readPolicyFile(state, StatePolicyName, adapter)
	return err == nil && policy.Enabled
}

// logStart records what the hook did: a start, a start not ready in time,
// or a failure, which also opens the retry gap. A launch an ordinary
// callback did not wait for is not recorded.
func logStart(stateRoot, callback string, entry time.Time, spawned, waited bool, err error) {
	pending := errors.Is(err, context.DeadlineExceeded)
	var line string
	switch {
	case err == nil && spawned:
		line = "started the dispatcher"
	case err == nil, pending && !waited:
		return
	case pending:
		line = "start_pending: the dispatcher was not ready in time; the session joins after a restart"
	default:
		line = "could not start the dispatcher: " + err.Error()
	}
	state, openErr := spool.OpenServerRoot(stateRoot)
	if openErr != nil {
		return
	}
	defer state.Close()
	if err != nil && !pending {
		if f, err := spool.OpenServerFile(state, startFailedName, os.O_WRONLY|os.O_CREATE); err == nil {
			f.Close()
			now := time.Now()
			_ = state.Chtimes(startFailedName, now, now)
		}
	}
	f, openErr := spool.OpenServerFile(state, "dispatcher.log", os.O_WRONLY|os.O_APPEND|os.O_CREATE)
	if openErr != nil {
		return
	}
	defer f.Close()
	log.New(f, "", log.LstdFlags).Printf("hook %s: %s (%d ms)", callback, line, time.Since(entry).Milliseconds())
}

func failedRecently(stateRoot string) bool {
	state, err := ident.OpenPrivateDir(stateRoot)
	if err != nil {
		return false
	}
	defer state.Close()
	fi, err := state.Lstat(startFailedName)
	return err == nil && time.Since(fi.ModTime()) < startRetryGap
}
