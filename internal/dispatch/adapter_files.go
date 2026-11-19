package dispatch

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

const (
	adapterStampName   = "stamp"
	adapterPolicyName  = "policy"
	adapterRepoSetName = "repo-defaults"
	adapterTurnEndName = "turn_end"
)

type leafAdapterStamp struct {
	NoticeSeq   uint64
	ReminderSeq uint64
}

func (s *server) adapterStamp(dir *os.Root, generation string) (leafAdapterStamp, bool) {
	var stamp leafAdapterStamp
	if dir == nil || s.ready.Instance == "" || generation == "" {
		return stamp, false
	}
	file, err := spool.OpenServerFile(dir, adapterStampName, os.O_RDONLY)
	if err != nil {
		return stamp, false
	}
	defer file.Close()
	const maxStamp = 256
	raw, err := io.ReadAll(io.LimitReader(file, maxStamp+1))
	if err != nil || len(raw) > maxStamp {
		return stamp, false
	}
	fields := strings.Fields(string(raw))
	if len(fields) != 4 || fields[0] != s.ready.Instance || fields[1] != generation {
		return stamp, false
	}
	var reminderErr error
	stamp.NoticeSeq, err = strconv.ParseUint(fields[2], 10, 64)
	stamp.ReminderSeq, reminderErr = strconv.ParseUint(fields[3], 10, 64)
	if err != nil || reminderErr != nil {
		return leafAdapterStamp{}, false
	}
	return stamp, true
}

func (s *server) publishAdapterStamp(dir *os.Root, generation string, notice, reminder bool) (leafAdapterStamp, error) {
	stamp, current := s.adapterStamp(dir, generation)
	// The counter belongs to the daemon instance and advances even if the
	// SQLite transaction rolls back. A later commit cannot reuse an ack.
	s.adapterSequence++
	if !current || notice {
		stamp.NoticeSeq = s.adapterSequence
	}
	if !current || reminder {
		stamp.ReminderSeq = s.adapterSequence
	}
	raw := fmt.Sprintf("%s %s %d %d\n", s.ready.Instance, generation, stamp.NoticeSeq, stamp.ReminderSeq)
	return stamp, publishAdapterText(dir, adapterStampName, raw)
}

type adapterInvalidation struct {
	digest, identity, generation string
	notice, reminder             bool
}

func adapterInvalidations(ctx context.Context, tx store.Tx) ([]adapterInvalidation, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT COALESCE(l.digest, ''), COALESCE(l.file_identity, ''),
		       COALESCE(l.generation, ''), d.notice, d.reminder
		FROM adapter_dirty d LEFT JOIN leaf l ON l.principal_id = d.principal_id
		  AND l.state = 'live' AND EXISTS (
		    SELECT 1 FROM principal_key k WHERE k.vendor = l.vendor AND k.key = l.key
		      AND k.principal_id = d.principal_id AND k.state = 'live')
		ORDER BY d.principal_id, l.digest`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var changes []adapterInvalidation
	for rows.Next() {
		var change adapterInvalidation
		if err := rows.Scan(&change.digest, &change.identity, &change.generation, &change.notice, &change.reminder); err != nil {
			return nil, err
		}
		changes = append(changes, change)
	}
	return changes, rows.Err()
}

// applyAdapterStamps publishes invalidations before the caller commits.
func (s *server) applyAdapterStamps(ctx context.Context, tx store.Tx) error {
	changes, err := adapterInvalidations(ctx, tx)
	if err != nil || len(changes) == 0 {
		return err
	}
	var members *os.Root
	for _, change := range changes {
		if change.digest == "" {
			continue
		}
		if members == nil {
			root, err := spool.OpenServerRoot(s.cfg.Rendezvous)
			if errors.Is(err, os.ErrNotExist) {
				break
			}
			if err != nil {
				return err
			}
			defer root.Close()
			members, err = spool.OpenServerChild(root, "m", false)
			if errors.Is(err, os.ErrNotExist) {
				break
			}
			if err != nil {
				return err
			}
			defer members.Close()
		}
		if err := s.invalidateAdapterLeaf(members, change); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, "DELETE FROM adapter_dirty")
	return err
}

func (s *server) invalidateAdapterLeaf(members *os.Root, change adapterInvalidation) error {
	dir, err := spool.OpenServerChild(members, change.digest[:16], false)
	if errors.Is(err, os.ErrNotExist) {
		return nil // Restoring the leaf requires a new generation.
	}
	if err != nil {
		return err
	}
	defer dir.Close()
	id, err := spool.DirectoryIdentity(dir)
	if err != nil {
		return err
	}
	if id != change.identity {
		return nil // The identity resolver cannot accept requests from this leaf.
	}
	if _, err := s.publishAdapterStamp(dir, change.generation, change.notice, change.reminder); err != nil {
		// Keep the ordinary command usable where the old stamp can still be
		// invalidated. A missing stamp forces a fresh output evaluation.
		if removeErr := dir.Remove(adapterStampName); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return errors.Join(err, removeErr)
		}
	}
	return nil
}

// addHookAck attaches the stamp the completed transaction evaluated.
func (s *server) addHookAck(_ context.Context, _ store.Tx, arrival Arrival, req wire.Request, result *core.Result) error {
	if result.Status != core.ExitOK || len(result.Data) == 0 || arrival.Owner || !req.IsHook() {
		return nil
	}
	var output wire.HookResult
	if err := json.Unmarshal(result.Data, &output); err != nil {
		return err
	}
	if output.Ack == nil {
		return nil
	}
	ack := output.Ack
	if req.Reply != nil && !*req.Reply {
		output.Ack = nil
	} else if arrival.Dir == nil || req.Generation == "" {
		return errors.New("hook acknowledgement has no authenticated generation")
	} else {
		stamp, ok := s.adapterStamp(arrival.Dir, req.Generation)
		if !ok {
			var err error
			stamp, err = s.publishAdapterStamp(arrival.Dir, req.Generation, true, true)
			if err != nil {
				return err
			}
		}
		ack.Instance, ack.Generation = s.ready.Instance, req.Generation
		switch ack.Kind {
		case "notice":
			ack.Seq = stamp.NoticeSeq
		case "reminder":
			if ack.TurnRef == "" {
				return errors.New("reminder acknowledgement has no turn")
			}
			ack.Seq = stamp.ReminderSeq
		default:
			return fmt.Errorf("unknown hook acknowledgement %q", ack.Kind)
		}
	}
	data, err := json.Marshal(output)
	if err == nil {
		result.Data = data
	}
	return err
}

// Invalidate an affected mirror before commit. A missing file is safe if
// either the commit or the subsequent publication fails.
func (s *server) invalidateAdapterFile(verb string) error {
	var name string
	switch verb {
	case wire.VerbAdapterSet:
		name = adapterPolicyName
	case domain.VerbChannelDefault:
		name = adapterRepoSetName
	case domain.VerbAdapterEvidence:
		name = adapterTurnEndName
	default:
		return nil
	}
	root, err := spool.OpenServerRoot(s.cfg.Rendezvous)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// publishAdapterFiles writes the committed snapshots after a policy or
// repository change and before a new dispatcher advertises readiness.
func (s *server) publishAdapterFiles(ctx context.Context) error {
	root, err := spool.OpenServerRoot(s.cfg.Rendezvous)
	if err != nil {
		return err
	}
	defer root.Close()
	return s.publishAdapterFilesAt(ctx, root)
}

func (s *server) publishAdapterFilesAt(ctx context.Context, root *os.Root) error {
	return s.db.Update(ctx, func(tx store.Tx) error {
		policy, err := adapterPolicyText(ctx, tx)
		if err != nil {
			return err
		}
		repos, err := adapterRepoSetText(ctx, tx)
		if err != nil {
			return err
		}
		turnEnd, err := adapterTurnEndText(ctx, tx)
		if err != nil {
			return err
		}
		return errors.Join(publishAdapterText(root, adapterPolicyName, policy),
			publishAdapterText(root, adapterRepoSetName, repos),
			publishAdapterText(root, adapterTurnEndName, turnEnd))
	})
}

func adapterPolicyText(ctx context.Context, tx store.Tx) (string, error) {
	var out strings.Builder
	for _, adapter := range []string{"claude", "codex"} {
		var enabled, reminder int
		var epoch uint64
		err := tx.QueryRowContext(ctx, `SELECT enabled, reminder, epoch
			FROM adapter_policy WHERE adapter = ?`, adapter).Scan(&enabled, &reminder, &epoch)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
		fmt.Fprintf(&out, "%s %d %d %d\n", adapter, enabled, reminder, epoch)
	}
	return out.String(), nil
}

// adapterTurnEndText lists the profiles with tested turn_end evidence, one
// "adapter version platform mode manifest_rev" line each. A Stop hook whose
// profile is not listed does not wait for the dispatcher; the domain still
// decides every continuation.
func adapterTurnEndText(ctx context.Context, tx store.Tx) (string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT vendor, vendor_version, platform, mode, manifest_rev
		FROM capability_evidence WHERE cap = 'turn_end' AND level = 'tested'
		ORDER BY vendor, vendor_version, platform, mode, manifest_rev`)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var out strings.Builder
	for rows.Next() {
		var f [5]string
		if err := rows.Scan(&f[0], &f[1], &f[2], &f[3], &f[4]); err != nil {
			return "", err
		}
		out.WriteString(strings.Join(f[:], " ") + "\n")
	}
	return out.String(), rows.Err()
}

func adapterRepoSetText(ctx context.Context, tx store.Tx) (string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT r.repo_root FROM repo_default r
		JOIN channel c ON c.id = r.channel_id WHERE c.state = 'active'`)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var hashes []string
	for rows.Next() {
		var root string
		if err := rows.Scan(&root); err != nil {
			return "", err
		}
		hashes = append(hashes, fmt.Sprintf("%x\n", sha256.Sum256([]byte(root))))
	}
	slices.Sort(hashes)
	return strings.Join(hashes, ""), rows.Err()
}

func publishAdapterText(root *os.Root, name, text string) error {
	staged := name + ".part"
	if err := root.Remove(staged); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := root.OpenFile(staged, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(staged)
	_, err = io.WriteString(file, text)
	if err := errors.Join(err, file.Close()); err != nil {
		return err
	}
	return root.Rename(staged, name)
}
