package hook

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/proc"
	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

// launchBinding is the launch a callback reports: a process launch, verified,
// with the time its record was written, or "session".
type launchBinding struct {
	ref      string
	verified bool
	atMS     int64
	// late reports that this callback wrote the record a missed start left out.
	late bool
	// record names the launch's record in the member directory.
	record string
}

var sessionLaunch = launchBinding{ref: "session"}

// hostedBy reports whether the hook runs directly under the process.
var hostedBy = proc.HostedBy

// A launch PID, which an adapter whose manifest names a launch_env reports,
// is a label within this session. A SessionStart pairs it with a launch, and
// so does a prompt, turn end or idle prompt that finds no record, under a hand join's proof: the hook
// runs directly under that process, and no start holds or missed the lock.
// Compact and terminal events cannot create that evidence. Without a launch
// PID the launch stays session-scoped and unverified.
func bindLaunch(leaf *os.Root, generation string, e event, spec eventSpec, entry time.Time) (launchBinding, error) {
	if e.LaunchRef == "" {
		return sessionLaunch, nil
	}
	pid, err := strconv.ParseUint(e.LaunchRef, 10, 32)
	if err != nil || pid == 0 {
		return sessionLaunch, nil
	}
	gen, err := ident.ReadPrivateFile(leaf, "gen", 1024)
	if err != nil {
		return sessionLaunch, err
	}
	if generation == "" || strings.TrimSpace(string(gen)) != generation {
		return sessionLaunch, errors.New("member generation changed")
	}
	start := spec.Enroll && (e.Source == "startup" || e.Source == "resume" || e.Source == "clear")
	name := fmt.Sprintf("launch-%d-%x", pid, sha256.Sum256([]byte(generation)))
	flags := os.O_RDONLY
	if start {
		flags = os.O_CREATE | os.O_RDWR
	}
	late := !start && lateLaunchFact(e.Fact)
	guard, err := spool.OpenServerFile(leaf, name+".lock", flags)
	if errors.Is(err, os.ErrNotExist) {
		if late {
			return lateLaunch(leaf, name, pid, e, entry)
		}
		return sessionLaunch, nil
	}
	if err != nil {
		return sessionLaunch, err
	}
	defer guard.Close()
	lock := syscall.LOCK_SH
	if start {
		lock = syscall.LOCK_EX
	}
	// Only starts for the same PID compete for this lock. Allow a brief
	// concurrent update, preserving the hook's deadline for the actual RPC.
	until := time.Now().Add(12 * time.Millisecond)
	for {
		err = syscall.Flock(int(guard.Fd()), lock|syscall.LOCK_NB)
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			break
		}
		if !start {
			return sessionLaunch, nil
		}
		if !time.Now().Before(until) {
			// Record the uncertainty before falling back. A future event cannot
			// attribute this start to the preceding PID binding. A strictly
			// newer start can establish certainty again.
			_, err = guard.WriteAt([]byte{'?'}, 0)
			return sessionLaunch, err
		}
		time.Sleep(time.Millisecond)
	}
	if err != nil {
		return sessionLaunch, err
	}
	at := entry.UnixMilli()
	snapshot, err := ident.ReadPrivateFile(leaf, name, 96)
	found := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return sessionLaunch, err
	}
	var latest int64
	var digest string
	if found {
		record, complete := strings.CutSuffix(string(snapshot), "\n")
		when, key, ok := strings.Cut(record, " ")
		latest, err = strconv.ParseInt(when, 10, 64)
		if !ok || !complete || err != nil || latest < 0 {
			return sessionLaunch, nil
		}
		if key != "-" {
			hash, err := hex.DecodeString(key)
			if err != nil || len(hash) != sha256.Size {
				return sessionLaunch, nil
			}
		}
		digest = key
	}
	if late && !found {
		// Give up the shared lock; the record is written under an exclusive one.
		if err := syscall.Flock(int(guard.Fd()), syscall.LOCK_UN); err != nil {
			return sessionLaunch, err
		}
		return lateLaunch(leaf, name, pid, e, entry)
	}
	if at < latest || (!start && !found) {
		return sessionLaunch, nil
	}
	if start {
		incoming := fmt.Sprintf("%x", sha256.Sum256([]byte(e.reference(entry))))
		if found && at == latest && digest != incoming {
			incoming = "-" // Two distinct references at the same time have no ordering.
		}
		if !found || at != latest || digest != incoming {
			if err := writeRecord(leaf, name, fmt.Sprintf("%d %s\n", at, incoming)); err != nil {
				return sessionLaunch, err
			}
			latest, digest = at, incoming
		}
	}
	// The one-byte guard records a start that could not acquire the lock.
	// Its mtime is when the missed callback gave up, after its entry time.
	stat, err := guard.Stat()
	if err != nil {
		return sessionLaunch, err
	}
	if digest == "-" || (stat.Size() != 0 && latest <= stat.ModTime().UnixMilli()) {
		return sessionLaunch, nil
	}
	return launchBinding{ref: fmt.Sprintf("pid-%d-%s", pid, digest), verified: true, atMS: latest, record: name}, nil
}

// lateLaunchFact reports whether a callback may bind a launch whose start
// was missed: a prompt, or the turn end or idle prompt of a session that
// joined by hand during a turn, which has no later prompt to wait for (#437).
func lateLaunchFact(fact string) bool {
	return fact == wire.HookPromptSubmitted || fact == wire.HookTurnFinishing || fact == wire.HookIdlePrompt
}

// lateLaunch writes the record of a launch whose start was never recorded,
// for its first prompt, turn end or idle prompt: at that callback's time, under an exclusive lock taken
// without waiting, unless a start ever missed the lock.
func lateLaunch(leaf *os.Root, name string, pid uint64, e event, entry time.Time) (launchBinding, error) {
	if !hostedBy(int(pid)) {
		return sessionLaunch, nil
	}
	guard, err := spool.OpenServerFile(leaf, name+".lock", os.O_CREATE|os.O_RDWR)
	if err != nil {
		return sessionLaunch, err
	}
	defer guard.Close()
	if err := syscall.Flock(int(guard.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return sessionLaunch, nil
		}
		return sessionLaunch, err
	}
	stat, err := guard.Stat()
	if err != nil || stat.Size() != 0 {
		return sessionLaunch, err
	}
	// A start may have written the record since the shared read.
	if _, err := ident.ReadPrivateFile(leaf, name, 96); !errors.Is(err, os.ErrNotExist) {
		return sessionLaunch, err
	}
	at := entry.UnixMilli()
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte("late:"+e.reference(entry))))
	if err := writeRecord(leaf, name, fmt.Sprintf("%d %s\n", at, digest)); err != nil {
		return sessionLaunch, err
	}
	return launchBinding{ref: fmt.Sprintf("pid-%d-%s", pid, digest), verified: true, atMS: at, late: true, record: name}, nil
}

// launchAcked reports whether the dispatcher answered a callback of this
// launch: until it has, a callback that may bind the launch waits for its
// reply before its route report, which the dispatcher does not order after
// a one-way event (#437). A publication that never arrived leaves the
// launch unacknowledged, so the next such callback waits again.
func launchAcked(leaf *os.Root, l launchBinding) bool {
	if l.record == "" {
		return true
	}
	ack, err := ident.ReadPrivateFile(leaf, l.record+".ack", 256)
	return err == nil && string(ack) == l.ref
}

// ackLaunch records that the dispatcher answered a callback of the launch.
func ackLaunch(leaf *os.Root, l launchBinding) {
	if l.record != "" {
		_ = writeRecord(leaf, l.record+".ack", l.ref) // best effort: the next callback waits again
	}
}
