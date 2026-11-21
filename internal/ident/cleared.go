package ident

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

// retiredKey is the refusal for a retired key. A key a /clear retired names
// the member its conversation was cleared into; a join makes it a new one.
func retiredKey(ctx context.Context, tx store.Tx, id Identity, retired string) error {
	var alias, channel string
	err := tx.QueryRowContext(ctx, `SELECT b.alias, c.name FROM principal_key k
		JOIN binding b ON b.principal_id = k.principal_id JOIN channel c ON c.id = b.channel_id
		WHERE k.vendor = ? AND k.key = ? AND k.retired_by = 'clear' ORDER BY b.id LIMIT 1`,
		id.Vendor, id.Key).Scan(&alias, &channel)
	if errors.Is(err, sql.ErrNoRows) {
		return &EnvironmentError{core.CodeKeyRetired, retired}
	}
	if err != nil {
		return err
	}
	return &EnvironmentError{core.CodeKeyCleared, fmt.Sprintf("this conversation was cleared into @%s in #%s", alias, channel)}
}

func isCleared(ctx context.Context, tx store.Tx, id Identity) (bool, error) {
	var cleared bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM principal_key
		WHERE vendor = ? AND key = ? AND state = 'retired' AND retired_by = 'clear')`, id.Vendor, id.Key).Scan(&cleared)
	return cleared, err
}

// clearedJoin lets a conversation a /clear retired join as a new member. The
// join's own transaction releases the key and its member directory, under a
// new generation, and enrolls the new member: a join that fails releases
// nothing. After commit the directory gets the new generation and loses the
// old roster cursor, so nothing of the member it was cleared into carries
// over.
func (r SQLResolver) clearedJoin(ctx context.Context, tx store.Tx, rendezvous string, id Identity, proof Proof, fileID string, old int64) (Resolution, bool, error) {
	if cleared, err := isCleared(ctx, tx, id); err != nil || !cleared {
		return Resolution{}, false, err
	}
	clock := r.Clock
	if clock == nil {
		clock = time.Now
	}
	var generation string
	enroll := func(ctx context.Context, target store.Tx, principal int64) error {
		if target != tx {
			return errors.New("enrollment belongs to another transaction")
		}
		next := wire.NewID()
		// The directory lets go of the key before the key goes.
		result, err := tx.ExecContext(ctx, `UPDATE leaf SET state = 'live', principal_id = NULL, generation = ?
			WHERE digest = ? AND generation = ? AND file_identity = ? AND state = 'fenced' AND principal_id = ?`,
			next, proof.KeyDigest, proof.Generation, fileID, old)
		if err := exactlyOne(result, err); err != nil {
			return err
		}
		result, err = tx.ExecContext(ctx, `DELETE FROM principal_key WHERE vendor = ? AND key = ? AND retired_by = 'clear'`,
			id.Vendor, id.Key)
		if err := exactlyOne(result, err); err != nil {
			return err
		}
		result, err = tx.ExecContext(ctx, `INSERT INTO principal_key (vendor, key, principal_id, state, created_at_ms)
			SELECT ?, ?, id, 'live', ? FROM principal WHERE id = ? AND kind = 'session' AND vendor = ?`,
			id.Vendor, id.Key, clock().UnixMilli(), principal, id.Vendor)
		if err := exactlyOne(result, err); err != nil {
			return err
		}
		result, err = tx.ExecContext(ctx, `UPDATE leaf SET principal_id = ? WHERE digest = ?`, principal, proof.KeyDigest)
		if err := exactlyOne(result, err); err != nil {
			return err
		}
		generation = next
		return nil
	}
	after := func() (string, error) {
		if generation == "" {
			return "", nil
		}
		members, err := openMembers(rendezvous)
		if err != nil {
			return "", err
		}
		defer members.Close()
		leaf, err := OpenPrivateChild(members, proof.KeyDigest[:16])
		if err != nil {
			return "", err
		}
		defer leaf.Close()
		if err := RepublishGeneration(leaf, generation); err != nil && !errors.Is(err, ErrDirectoryBusy) {
			return generation, err
		}
		return generation, nil
	}
	return Resolution{Caller: core.Caller{Role: core.RoleUnenrolled}, Vendor: id.Vendor, Key: id.Key,
		Enroll: enroll, AfterCommit: after}, true, nil
}

// ErrDirectoryBusy says a renderer holds the member directory's lock; the
// dispatcher's next pass publishes the generation.
var ErrDirectoryBusy = errors.New("member directory busy")

// RepublishGeneration makes a member directory carry the generation the
// store committed, without waiting for a renderer: the dispatcher's next
// pass retries. Calling it again changes nothing.
func RepublishGeneration(leaf *os.Root, generation string) error {
	return publishGeneration(leaf, generation, false)
}

// publishGeneration is the one way a generation reaches a member directory.
// A new generation starts without the old roster cursors, which go first,
// so an interruption never leaves an old cursor under the new generation.
// It holds the directory lock the CLI holds to write those cursors, so a
// renderer of the old membership cannot write into the new one; wait says
// whether to wait briefly for that renderer.
func publishGeneration(leaf *os.Root, generation string, wait bool) error {
	current := func() (string, error) {
		data, err := ReadPrivateFile(leaf, "gen", 4096)
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return strings.TrimSpace(string(data)), err
	}
	if gen, err := current(); err != nil || gen == generation {
		return err
	}
	lock, err := leaf.Open(".")
	if err != nil {
		return err
	}
	defer lock.Close()
	for tries := 0; ; tries++ {
		err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			break
		}
		if !wait || tries >= 100 {
			return ErrDirectoryBusy
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err != nil {
		return err
	}
	if gen, err := current(); err != nil || gen == generation {
		return err
	}
	// Both receipts hold roster cursors of the old membership: the
	// acknowledged one and the one with the rules (spool.RulesReceipt).
	for _, receipt := range []string{"ack", "rules"} {
		if err := leaf.Remove(receipt); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	if err := dropPendingReceipts(leaf, generation); err != nil {
		return err
	}
	return publishMetadata(leaf, "gen", generation)
}

// LeafReceipts reads the roster receipts in the member directory of a session
// key under rendezvous: the acknowledged cursor and the one with the rules.
// It reads them only from the registered directory (fileID) carrying the
// committed generation; otherwise, and for a missing or unreadable receipt,
// it reads 0.
func LeafReceipts(rendezvous string, id Identity, fileID, generation string) (cursor, rules uint64) {
	members, err := openMembers(rendezvous)
	if err != nil {
		return 0, 0
	}
	defer members.Close()
	leaf, err := OpenPrivateChild(members, id.Digest()[:16])
	if err != nil {
		return 0, 0
	}
	defer leaf.Close()
	if identity, err := FileIdentity(leaf); err != nil || identity != fileID {
		return 0, 0
	}
	if gen, err := ReadPrivateFile(leaf, "gen", 4096); err != nil || strings.TrimSpace(string(gen)) != generation {
		return 0, 0
	}
	read := func(name string) uint64 {
		data, err := ReadPrivateFile(leaf, name, 32)
		if err != nil {
			return 0
		}
		n, _ := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
		return n
	}
	pendingCursor, pendingRules := PendingReceipt(leaf, generation)
	return max(read("ack"), pendingCursor), max(read("rules"), pendingRules)
}

// pendingReceipt names the file holding receipts a renderer could not record
// under the directory lock in time, for the generation it names; the lock
// does not guard it, and the dispatcher applies it with the next request.
func pendingReceipt(generation string) string { return "pending-" + generation }

// PendingReceipt reads the receipts left pending for generation: the roster
// cursor and the cursor with the rules. A missing or unreadable record reads
// as 0.
func PendingReceipt(leaf *os.Root, generation string) (cursor, rules uint64) {
	if !validRoute(generation) {
		return 0, 0
	}
	data, err := ReadPrivateFile(leaf, pendingReceipt(generation), 64)
	if err != nil {
		return 0, 0
	}
	c, r, _ := strings.Cut(strings.TrimSpace(string(data)), " ")
	cursor, _ = strconv.ParseUint(c, 10, 64)
	rules, _ = strconv.ParseUint(r, 10, 64)
	return cursor, rules
}

// LeavePendingReceipt records receipts for generation without the directory
// lock, keeping the larger of what is there and what is given.
func LeavePendingReceipt(leaf *os.Root, generation string, cursor, rules uint64) error {
	if !validRoute(generation) {
		return fmt.Errorf("invalid generation")
	}
	c, r := PendingReceipt(leaf, generation)
	cursor, rules = max(cursor, c), max(rules, r)
	return publishMetadata(leaf, pendingReceipt(generation), strconv.FormatUint(cursor, 10)+" "+strconv.FormatUint(rules, 10)+"\n")
}

// dropPendingReceipts removes the pending receipts of every generation other
// than generation: they belong to an earlier membership.
func dropPendingReceipts(leaf *os.Root, generation string) error {
	dir, err := leaf.Open(".")
	if err != nil {
		return err
	}
	names, err := dir.Readdirnames(-1)
	dir.Close()
	if err != nil {
		return err
	}
	for _, name := range names {
		if strings.HasPrefix(name, "pending-") && name != pendingReceipt(generation) {
			if err := leaf.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}
