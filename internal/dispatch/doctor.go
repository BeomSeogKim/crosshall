package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/publish"
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
	var adapters []doctorAdapter
	var warnings []string
	now := time.Now()
	if stateErr == nil {
		adapters = localDoctorAdapters(cfg.StateRoot)
		for _, adapter := range adapters {
			if warning := codexHookRoutingWarning(adapter.CodexHookStateRouting); warning != "" {
				warnings = append(warnings, warning)
			}
		}
		state, stateErr = spool.OpenServerRoot(cfg.StateRoot)
		rendezvous, err := spool.OpenServerRoot(cfg.Rendezvous)
		if err == nil {
			attachDoctorHooks(adapters, rendezvous, now)
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
		snapshot, err := doctorStoreInfo(ctx, state, adapters, now)
		version.record(fmt.Sprintf("schema version %d", snapshot.SchemaVersion), err)
		for _, v := range snapshot.IdentityVersions {
			id := ident.Identity{Vendor: "agy", Version: v}
			if id.UnknownAntigravityCLI() {
				msg := id.Warning()
				if !slices.Contains(warnings, msg) {
					warnings = append(warnings, msg)
				}
			}
		}
		ownerSpool.record("owner spool accepts writes", probeOwnerSpool(state))
	} else {
		for _, check := range []*doctorCheck{&lock, &readiness, &version, &ownerSpool} {
			check.record("", stateErr)
		}
		notRunning = errors.Is(stateErr, os.ErrNotExist)
	}
	hint := ""
	if notRunning {
		hint = "crosshall owner start"
	}
	checks := []doctorCheck{lock, readiness, version, permissions, ownerSpool}
	if !allowsTestClock(cfg.Version) {
		checks = append(checks, doctorCheck{Name: "test_clock", OK: true, Detail: "release builds ignore " + clockOffsetEnv})
	}
	return cfg.doctorResult(hint, checks, adapters, warnings)
}

func (check *doctorCheck) record(detail string, err error) {
	check.OK = err == nil
	if err != nil {
		detail = err.Error()
	}
	check.Detail = detail
}

func (cfg LaunchConfig) doctorResult(hint string, checks []doctorCheck, adapters []doctorAdapter, warnings []string) core.Result {
	status := core.ExitOK
	for _, check := range checks {
		if !check.OK {
			status = core.ExitError
			break
		}
	}
	data, _ := json.Marshal(struct {
		StateDir   string          `json:"state_dir,omitempty"`
		Rendezvous string          `json:"rendezvous,omitempty"`
		Checks     []doctorCheck   `json:"checks"`
		Hint       string          `json:"hint,omitempty"`
		Adapters   []doctorAdapter `json:"adapters,omitempty"`
		Warnings   []string        `json:"warnings,omitempty"`
	}{cfg.StateRoot, cfg.Rendezvous, checks, hint, adapters, warnings})
	return core.Result{Status: status, Data: data}
}

func doctorStoreInfo(ctx context.Context, state *os.Root, adapters []doctorAdapter, now time.Time) (store.DoctorSnapshot, error) {
	names := make([]string, 0, len(adapters))
	for _, adapter := range adapters {
		names = append(names, adapter.Adapter)
	}
	snapshot, err := store.InspectDoctorStoreAt(ctx, spool.ServerFiles{Root: state}, names, "agy", now)
	for i := range adapters {
		adapter := &adapters[i]
		if state, ok := snapshot.Adapters[adapter.Adapter]; ok {
			adapter.Policy = &state.Policy
			adapter.Evidence = state.Evidence
			adapter.SetReminderEvidence(state.Evidence)
			adapter.UnverifiedLaunches = &state.UnverifiedLaunches
			adapter.UnseenStarts = &state.UnseenStarts
			adapter.TurnEndNotice = &state.TurnEndNotice
			status := state.Publish
			if _, supported := publish.Lookup(adapter.Adapter); !supported {
				status.State, status.Reason = "off", "no publish adapter"
			}
			adapter.Publish = &status
		}
	}
	return snapshot, err
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
