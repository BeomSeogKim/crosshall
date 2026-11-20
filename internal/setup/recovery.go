package setup

import (
	"context"
	"errors"
	"fmt"
	"slices"
)

func pendingCanResume(op operation, action string) bool {
	if op.Action != action {
		return false
	}
	for _, e := range op.Edits {
		current, err := readConfig(e.Path)
		if err != nil {
			return false
		}
		now := stamp(current)
		if e.matchesAfter(now) {
			continue
		}
		if !e.matchesBefore(now) {
			return false
		}
		if now != e.Before && !canResumeNewHome(e) {
			return false
		}
		next, err := e.materialize(current.Bytes)
		if err != nil || len(next) > maxConfigBytes || (e.After.Exists && sum(next) != e.After.Hash) {
			return false
		}
	}
	return true
}

func canResumeNewHome(e edit) bool {
	location, err := openConfigLocation(e.Path)
	if err != nil {
		return false
	}
	defer location.dir.Close()
	return location.matchesMissingDir(e.Before.Dir) &&
		(location.missing != "" || checkNewConfigDir(location.dir) == nil)
}

// reconcilePublished records only changes we can link to a completed
// publication. Bytes alone cannot identify a file written independently.
func reconcilePublished(j *journal) {
	for _, e := range j.Pending.Edits {
		current, err := readConfig(e.Path)
		if err != nil {
			if e.Before != e.After && e.After.File != "" {
				rememberUnprovenPublication(j, e)
			}
			continue
		}
		now := stamp(current)
		if e.matchesAfter(now) {
			recordApplied(j, e, now)
			continue
		}
		// An in-place owner edit preserves the staged file identity. Adopt
		// the installed entries but discard exact reversal of that file.
		if e.Before != e.After && e.After.File != "" && now.Exists && now.File == e.After.File &&
			e.publicationDirectoryMatches(now.Dir) {
			if e.Record != nil {
				record := *e.Record
				record.Undo = nil
				e.Record = &record
			}
			recordApplied(j, e, now)
		} else if e.Before != e.After && e.After.File != "" && !e.matchesBefore(now) {
			rememberUnprovenPublication(j, e)
		}
	}
	j.Pending = nil
}

func rememberUnprovenPublication(j *journal, e edit) {
	if slices.ContainsFunc(j.Retained, func(file retainedFile) bool {
		return file.Adapter == e.Adapter && file.Path == e.Path
	}) {
		return
	}
	j.Retained = append(j.Retained, retainedFile{
		Adapter: e.Adapter, Path: e.Path, Reason: "an interrupted publication could not be verified; inspect it manually",
	})
}

func recordApplied(j *journal, e edit, observed fingerprint) {
	if e.Record == nil {
		delete(j.Files, e.Adapter)
		return
	}
	record := *e.Record
	record.Latest = observed
	j.Files[e.Adapter] = record
}

func pendingRecoveryPathError(op operation) error {
	if err := verifyTargets(op); err != nil {
		return err
	}
	for _, e := range op.Edits {
		if e.StageDir == "" {
			continue
		}
		location, err := openConfigLocation(e.Path)
		if err != nil {
			return fmt.Errorf("%s configuration: %w", e.Adapter, err)
		}
		info, err := location.dir.Stat(".")
		location.dir.Close()
		if err != nil {
			return fmt.Errorf("%s configuration: %w", e.Adapter, err)
		}
		if location.missing != "" {
			if e.matchesCleanupAbsence(location.missingParents[len(location.missingParents)-1]) {
				continue
			}
		} else if configIdentity(info) == e.StageDir {
			continue
		}
		return fmt.Errorf("%s configuration directory changed while the plan was pending", e.Adapter)
	}
	return nil
}

func pendingShutdownPlan(op operation, reason error) operation {
	shutdown := operation{Action: "uninstall", Adapters: adapterNames()}
	for _, e := range op.Edits {
		shutdown.Retained = append(shutdown.Retained, retainedFile{
			Adapter: e.Adapter, Path: e.Path, Reason: reason.Error(),
		})
	}
	return shutdown
}

func (s Service) recoverJournal(ctx context.Context, digest string, uninstall bool, selected ...string) (journal, string, error) {
	state, lock, err := s.lockState()
	if err != nil {
		return journal{}, "", err
	}
	defer state.Close()
	defer lock.Close()
	j, observed, err := readJournalAt(state)
	if err != nil {
		return journal{}, "", err
	}
	if observed != digest || j.Pending == nil {
		return journal{}, "", errors.New("installation changed after planning; rerun to review the new plan")
	}
	blocked := pendingRecoveryPathError(*j.Pending)
	if blocked == nil {
		for i := range j.Pending.Edits {
			e := &j.Pending.Edits[i]
			if err := e.stageCleanup(); err != nil {
				blocked = fmt.Errorf("%s temporary configuration: %w", e.Adapter, err)
				break
			}
			if len(e.CleanupDirs) == 0 {
				continue
			}
			current, err := readConfig(e.Path)
			if err != nil {
				blocked = fmt.Errorf("%s configuration: %w", e.Adapter, err)
				break
			}
			if now := stamp(current); e.matchesAfter(now) {
				if _, err := finishEdit(state, j, e, now); err != nil {
					blocked = fmt.Errorf("%s configuration directory cleanup: %w", e.Adapter, err)
					break
				}
			}
		}
	}
	if blocked != nil {
		if !uninstall {
			return journal{}, "", blocked
		}
		// Shutdown is safe even when filesystem recovery is blocked. The
		// journal remains intact so the owner can restore the original path.
		shutdown := pendingShutdownPlan(*j.Pending, blocked)
		if len(selected) != 0 {
			shutdown.Adapters = selected
		}
		if err := s.setPolicies(ctx, shutdown.Adapters, false); err != nil {
			return journal{}, "", err
		}
		j.Pending.Retained = shutdown.Retained
		return j, observed, nil
	}
	reconcilePublished(&j)
	if err := writeJournal(state, j); err != nil {
		return journal{}, "", err
	}
	return readJournalAt(state)
}
