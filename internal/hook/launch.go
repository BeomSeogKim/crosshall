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
	"github.com/BeomSeogKim/crosshall/internal/spool"
)

// A Claude PID is a label within this session. Only a SessionStart can pair
// it with a launch. Compact and terminal events cannot create that evidence.
func bindLaunch(leaf *os.Root, generation string, e event, spec eventSpec, entry time.Time) (string, bool, error) {
	if e.Identity.Vendor != "claude" || e.LaunchRef == "" {
		return "session", false, nil
	}
	pid, err := strconv.ParseUint(e.LaunchRef, 10, 32)
	if err != nil || pid == 0 {
		return "session", false, nil
	}
	gen, err := ident.ReadPrivateFile(leaf, "gen", 1024)
	if err != nil {
		return "session", false, err
	}
	if generation == "" || strings.TrimSpace(string(gen)) != generation {
		return "session", false, errors.New("member generation changed")
	}
	start := spec.Enroll && (e.Source == "startup" || e.Source == "resume" || e.Source == "clear")
	name := fmt.Sprintf("launch-%d-%x", pid, sha256.Sum256([]byte(generation)))
	flags := os.O_RDONLY
	if start {
		flags = os.O_CREATE | os.O_RDWR
	}
	guard, err := spool.OpenServerFile(leaf, name+".lock", flags)
	if errors.Is(err, os.ErrNotExist) {
		return "session", false, nil
	}
	if err != nil {
		return "session", false, err
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
			return "session", false, nil
		}
		if !time.Now().Before(until) {
			// Record the uncertainty before falling back. A future event cannot
			// attribute this start to the preceding PID binding. A strictly
			// newer start can establish certainty again.
			_, err = guard.WriteAt([]byte{'?'}, 0)
			return "session", false, err
		}
		time.Sleep(time.Millisecond)
	}
	if err != nil {
		return "session", false, err
	}
	at := entry.UnixMilli()
	snapshot, err := ident.ReadPrivateFile(leaf, name, 96)
	found := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "session", false, err
	}
	var latest int64
	var digest string
	if found {
		record, complete := strings.CutSuffix(string(snapshot), "\n")
		when, key, ok := strings.Cut(record, " ")
		latest, err = strconv.ParseInt(when, 10, 64)
		if !ok || !complete || err != nil || latest < 0 {
			return "session", false, nil
		}
		if key != "-" {
			hash, err := hex.DecodeString(key)
			if err != nil || len(hash) != sha256.Size {
				return "session", false, nil
			}
		}
		digest = key
	}
	if at < latest || (!start && !found) {
		return "session", false, nil
	}
	if start {
		incoming := fmt.Sprintf("%x", sha256.Sum256([]byte(e.reference(entry))))
		if found && at == latest && digest != incoming {
			incoming = "-" // Two distinct references at the same time have no ordering.
		}
		if !found || at != latest || digest != incoming {
			if err := writeRecord(leaf, name, fmt.Sprintf("%d %s\n", at, incoming)); err != nil {
				return "session", false, err
			}
			latest, digest = at, incoming
		}
	}
	// The one-byte guard records a start that could not acquire the lock.
	// Its mtime is when the missed callback gave up, after its entry time.
	stat, err := guard.Stat()
	if err != nil {
		return "session", false, err
	}
	if digest == "-" || (stat.Size() != 0 && latest <= stat.ModTime().UnixMilli()) {
		return "session", false, nil
	}
	return fmt.Sprintf("pid-%d-%s", pid, digest), true, nil
}
