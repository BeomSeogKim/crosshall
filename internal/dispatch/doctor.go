package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

type doctorCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// Doctor diagnoses owner paths without launching the dispatcher or migrating
// the database. The CLI must enforce the owner gate before calling it.
func (cfg LaunchConfig) Doctor(ctx context.Context) core.Result {
	lock := doctorCheck{Name: "lock"}
	readiness := doctorCheck{Name: "readiness"}
	version := doctorCheck{Name: "store_version"}
	permissions := doctorCheck{Name: "rendezvous_permissions"}
	ownerSpool := doctorCheck{Name: "owner_spool_writable"}
	var state *os.Root
	// Starting the dispatcher is the remedy when none holds the lock, or when
	// the state root does not exist yet; an unsafe path needs the owner instead.
	notRunning := false
	stateErr := CheckStateRoot(cfg.Version, cfg.StateRoot, cfg.DefaultStateRoot)
	if stateErr == nil {
		state, stateErr = spool.OpenServerRoot(cfg.StateRoot)
		rendezvous, err := spool.OpenServerRoot(cfg.Rendezvous)
		if err == nil {
			rendezvous.Close()
		}
		permissions.record("rendezvous owned by this user with mode 0700", err)
	} else {
		permissions.record("", stateErr)
	}
	if stateErr == nil {
		defer state.Close()
		switch held, err := lockState(state); {
		case err != nil:
			lock.record("", fmt.Errorf("dispatcher lock cannot be inspected: %w", err))
		case held:
			lock.record("dispatcher lock held", nil)
		default:
			lock.record("", errors.New("dispatcher lock is absent or idle"))
			notRunning = true
		}
		if marker := readReady(cfg.Rendezvous); marker.live(state) {
			readiness.record(fmt.Sprintf("dispatcher PID %d is ready", marker.PID), nil)
		} else {
			readiness.record("", errors.New("no live dispatcher readiness marker"))
		}
		current, err := doctorStoreVersion(ctx, state)
		version.record(fmt.Sprintf("schema version %d", current), err)
		ownerSpool.record("owner spool accepts writes", probeOwnerSpool(state))
	} else {
		for _, check := range []*doctorCheck{&lock, &readiness, &version, &ownerSpool} {
			check.record("", stateErr)
		}
		notRunning = errors.Is(stateErr, fs.ErrNotExist)
	}
	hint := ""
	if notRunning {
		hint = "crosshall owner start"
	}
	checks := []doctorCheck{lock, readiness, version, permissions, ownerSpool}
	if !allowsTestClock(cfg.Version) {
		checks = append(checks, doctorCheck{Name: "test_clock", OK: true, Detail: "release builds ignore " + clockOffsetEnv})
	}
	return doctorResult(hint, checks...)
}

func (check *doctorCheck) record(detail string, err error) {
	check.OK = err == nil
	if err != nil {
		detail = err.Error()
	}
	check.Detail = detail
}

func doctorResult(hint string, checks ...doctorCheck) core.Result {
	status := core.ExitOK
	for _, check := range checks {
		if !check.OK {
			status = core.ExitError
			break
		}
	}
	data, _ := json.Marshal(struct {
		Checks []doctorCheck `json:"checks"`
		Hint   string        `json:"hint,omitempty"`
	}{checks, hint})
	return core.Result{Status: status, Data: data}
}

type doctorStoreFS struct{ state *os.Root }

func (files doctorStoreFS) Open(name string) (fs.File, error) {
	return spool.OpenServerFile(files.state, name, os.O_RDONLY)
}

func doctorStoreVersion(ctx context.Context, state *os.Root) (int, error) {
	files := doctorStoreFS{state}
	// SQLite treats any failed journal lookup as a missing journal. Report an
	// unsafe journal explicitly so a stale main file cannot look current.
	for _, name := range []string{"store.sqlite3", "store.sqlite3-wal", "store.sqlite3-journal"} {
		file, err := files.Open(name)
		if err != nil {
			if name != "store.sqlite3" && errors.Is(err, os.ErrNotExist) {
				continue
			}
			return 0, err
		}
		file.Close()
	}
	return store.CheckVersion(ctx, files)
}

func probeOwnerSpool(state *os.Root) error {
	owner, err := spool.OpenServerChild(state, "owner", false)
	if err != nil {
		return err
	}
	defer owner.Close()
	in, err := spool.OpenServerChild(owner, "in", false)
	if err != nil {
		return err
	}
	defer in.Close()
	name := ".doctor-" + wire.NewID() + ".part"
	file, err := in.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, err = file.WriteString("doctor\n")
	return errors.Join(err, file.Close(), in.Remove(name))
}
