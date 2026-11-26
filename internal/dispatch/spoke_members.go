package dispatch

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/link"
	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

// spokeMembers forwards the member commands of a linked device's own
// sessions to the home (#400). Its store and member rendezvous belong to
// one hall and device: a link to another hall uses another directory,
// and unlinking leaves the local hall's rendezvous untouched.
type spokeMembers struct {
	db      *store.Store
	members string // the member rendezvous

	mu   sync.Mutex
	conn spokeConn // nil while the link is down
}

// spokeConn is the link the member commands go over.
type spokeConn interface {
	Write(frame string, v any) error
	Read() (link.Frame, error)
	Close()
}

type sshSpokeConn struct{ c *link.SSHConnection }

func (s sshSpokeConn) Write(frame string, v any) error { return s.c.Writer.Write(frame, v) }
func (s sshSpokeConn) Read() (link.Frame, error)       { return s.c.Reader.Read() }
func (s sshSpokeConn) Close()                          { s.c.Close() }

// spokeDir is the private directory of one hall and device's spoke store.
func spokeDir(state *os.Root, stateRoot string, linked linkConfig) (string, error) {
	name := linked.Hall + "-" + linked.Device
	if !validSpokeName(name) {
		return "", fmt.Errorf("invalid spoke store name %q", name)
	}
	for _, dir := range []string{"spoke", filepath.Join("spoke", name)} {
		if err := state.Mkdir(dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return "", err
		}
		fi, err := state.Lstat(dir)
		if err != nil {
			return "", err
		}
		if !fi.IsDir() || fi.Mode().Perm() != 0700 {
			return "", fmt.Errorf("%s must be a private directory", dir)
		}
	}
	return filepath.Join(stateRoot, "spoke", name), nil
}

func validSpokeName(name string) bool {
	if name == "" || len(name) > 160 {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

func openSpokeMembers(ctx context.Context, state *os.Root, stateRoot string, linked linkConfig) (*spokeMembers, error) {
	dir, err := spokeDir(state, stateRoot, linked)
	if err != nil {
		return nil, err
	}
	db, err := store.OpenSpoke(ctx, dir)
	if err != nil {
		return nil, err
	}
	members := ident.Rendezvous(dir, os.Geteuid())
	root, err := openRendezvous(members)
	if err != nil {
		db.Close()
		return nil, err
	}
	root.Close()
	return &spokeMembers{db: db, members: members}, nil
}

// setConn installs or clears the link the member commands go over.
func (m *spokeMembers) setConn(c *link.SSHConnection) {
	if c == nil {
		m.attach(nil)
		return
	}
	m.attach(sshSpokeConn{c})
}

func (m *spokeMembers) attach(c spokeConn) {
	m.mu.Lock()
	m.conn = c
	m.mu.Unlock()
}

// errNotSent says no byte of the frame reached the link.
var errNotSent = errors.New("the link to the home is down")

// exchange writes one frame and reads its result, under the link's lock,
// by the deadline. A failure after the write closes the link, so a late
// answer can never be read as the next command's.
func (m *spokeMembers) exchange(ctx context.Context, frame string, call domain.LinkCall, deadline time.Time) (core.Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	conn := m.conn
	if conn == nil {
		return core.Result{}, errNotSent
	}
	fail := func(err error) (core.Result, error) {
		conn.Close()
		m.conn = nil
		return core.Result{}, err
	}
	if err := conn.Write(frame, call); err != nil {
		return fail(err)
	}
	type read struct {
		f   link.Frame
		err error
	}
	got := make(chan read, 1)
	go func() {
		f, err := conn.Read()
		got <- read{f, err}
	}()
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return fail(ctx.Err())
	case <-timer.C:
		return fail(errors.New("the home did not answer in time"))
	case r := <-got:
		var result core.Result
		if r.err != nil || r.f.Type != "result" || json.Unmarshal(r.f.Body, &result) != nil {
			return fail(fmt.Errorf("link: no result frame: %v", r.err))
		}
		if result.Code == domain.CodeLinkStale {
			return fail(errors.New("the home closed this link")) // the reconnect loop dials again
		}
		return result, nil
	}
}

func (s *server) scanSpokeMembers(ctx context.Context, reap bool, now time.Time) {
	m := s.spokeMembers
	root, err := openRendezvous(m.members)
	if err != nil {
		s.logFailure(err)
		return
	}
	defer root.Close()
	first, err := spool.OpenServerChild(root, "first", false)
	if err != nil {
		s.logFailure(err)
		return
	}
	s.runSpool(first, reap, now, func(id string, raw []byte) (core.Result, error) {
		return s.spokeHello(ctx, id, raw)
	})
	first.Close()
	members, err := spool.OpenServerChild(root, "m", false)
	if err != nil {
		s.logFailure(err)
		return
	}
	defer members.Close()
	type leaf struct {
		digest, identity, generation string
		membership                   core.Membership
	}
	var leaves []leaf
	err = m.db.Update(ctx, func(tx store.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT digest, file_identity, generation, binding_id, membership_gen FROM spoke_leaf`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var l leaf
			if err := rows.Scan(&l.digest, &l.identity, &l.generation, &l.membership.BindingID, &l.membership.Generation); err != nil {
				return err
			}
			leaves = append(leaves, l)
		}
		return rows.Err()
	})
	if err != nil {
		s.logFailure(err)
		return
	}
	for _, l := range leaves {
		if ctx.Err() != nil {
			return
		}
		path := filepath.Join(m.members, "m", l.digest[:16])
		dir, err := spool.OpenServerChild(members, l.digest[:16], false)
		if errors.Is(err, os.ErrNotExist) {
			continue // first contact registers the session again
		}
		if err != nil {
			s.logFailure(err)
			continue
		}
		identity, err := spool.DirectoryIdentity(dir)
		if err != nil || identity != l.identity {
			dir.Close()
			continue
		}
		if _, err := spool.RestoreServerSpool(dir); err != nil {
			s.logFailure(err)
		}
		if err := ident.RepublishGeneration(dir, l.generation); !errors.Is(err, ident.ErrDirectoryBusy) {
			s.logFailure(err)
		}
		s.logFailure(ident.RepublishMembership(dir, l.membership))
		if reap {
			s.logFailure(spool.ReapServerSpool(dir, now.Add(-24*time.Hour), true))
		}
		s.logFailure(spool.DrainMemberSpool(dir, func(id string, raw []byte, _ bool) (core.Result, error) {
			return s.spokeCommand(ctx, path, dir, id, raw)
		}))
		dir.Close()
	}
}

func (s *server) spokeHello(ctx context.Context, id string, raw []byte) (core.Result, error) {
	var hello wire.Hello
	if err := json.Unmarshal(raw, &hello); err != nil || hello.Version != wire.Version || hello.Attempt != id {
		return commandError(core.ExitUsage, "", "invalid first-contact envelope"), nil
	}
	if hello.ExpiresAtMS != 0 && hello.ExpiresAtMS <= time.Now().UnixMilli() {
		return commandError(core.ExitUnavailable, core.CodeUnsent, "first contact expired"), nil
	}
	var reg ident.Registration
	err := s.spokeMembers.db.Update(ctx, func(tx store.Tx) error {
		var err error
		reg, err = ident.RegisterSpokeHello(ctx, tx, s.spokeMembers.members, hello, time.Now())
		return err
	})
	var env *ident.EnvironmentError
	if errors.As(err, &env) {
		return domain.Fail(core.ExitNotEnrolled, env.Code, env.Message, "crosshall join <channel>"), nil
	}
	if err != nil {
		s.logFailure(err)
		return commandError(core.ExitError, "", "first contact failed; see dispatcher.log"), nil
	}
	if err := reg.Publish(); err != nil {
		return core.Result{}, err // the next pass finishes a committed registration
	}
	return core.Result{Status: core.ExitOK, Data: json.RawMessage(`{}`)}, nil
}

// spokeCommand forwards one member command to the home. A keyed command
// is written to the outbox, committed, before the first byte can reach
// the link, so its outcome can always be looked up receipt-only.
func (s *server) spokeCommand(ctx context.Context, path string, dir *os.Root, id string, raw []byte) (core.Result, error) {
	var req wire.Request
	if err := json.Unmarshal(raw, &req); err != nil || req.Version != wire.Version || req.Attempt != id {
		return commandError(core.ExitUsage, "", "invalid command envelope"), nil
	}
	if req.ExpiresAtMS <= time.Now().UnixMilli() {
		return unknownOutcome("attempt expired", req.OpKey), nil
	}
	if req.IsHook() {
		return domain.Fail(core.ExitUnavailable, core.CodeDispatcherUnavailable,
			"hook facts of a linked device reach the hall in a later release", ""), nil
	}
	m := s.spokeMembers
	if req.Verb == domain.VerbRetry {
		resend, done, err := s.spokeRetry(ctx, path, dir, req)
		if err != nil || done != nil {
			if done != nil {
				return *done, nil
			}
			return core.Result{}, err
		}
		req = resend
	}
	var leaf ident.SpokeLeaf
	var outbox int64
	err := m.db.Update(ctx, func(tx store.Tx) error {
		var err error
		leaf, err = ident.ResolveSpokeMember(ctx, tx, m.members, ident.Ingress{Path: path, Dir: dir},
			ident.Proof{KeyDigest: req.KeyDigest, Generation: req.Generation})
		if err != nil || req.OpKey == "" {
			return err
		}
		var membership any
		if req.Membership != nil {
			raw, err := json.Marshal(req.Membership)
			if err != nil {
				return err
			}
			membership = string(raw)
		}
		// A key used again resends the same record; its bytes decide.
		err = tx.QueryRowContext(ctx, `INSERT INTO link_outbox (vendor, key, op_key, command, args, membership, state, at_ms)
			VALUES (?, ?, ?, ?, ?, ?, 'sent', ?)
			ON CONFLICT (vendor, key, op_key) DO UPDATE SET state = 'sent' RETURNING id`,
			leaf.Vendor, leaf.Key, req.OpKey, req.Verb, string(req.Args), membership, time.Now().UnixMilli()).Scan(&outbox)
		return err
	})
	var env *ident.EnvironmentError
	if errors.As(err, &env) {
		return domain.Fail(core.ExitNotEnrolled, env.Code, env.Message, "crosshall join <channel>"), nil
	}
	if err != nil {
		s.logFailure(err)
		return commandError(core.ExitError, "", "the device could not record the command; see dispatcher.log"), nil
	}
	call := domain.LinkCall{Vendor: leaf.Vendor, Key: leaf.Key, LeafGeneration: leaf.Generation, LeafEpoch: leaf.Epoch,
		Command: req.Verb, Args: req.Args, OpKey: req.OpKey, Membership: req.Membership, Ack: req.Ack}
	result, err := m.exchange(ctx, "call", call, time.UnixMilli(req.ExpiresAtMS))
	if errors.Is(err, errNotSent) {
		s.settleOutbox(ctx, outbox, nil)
		return domain.Fail(core.ExitUnavailable, core.CodeUnsent, "the link to the home is down; nothing was sent",
			"run crosshall link status, then send the command again"), nil
	}
	if err != nil {
		s.markUnknown(ctx, outbox)
		return lostAnswer(req.OpKey), nil
	}
	s.settleOutbox(ctx, outbox, &result)
	if result.Membership != nil {
		s.logFailure(m.db.Update(ctx, func(tx store.Tx) error {
			_, err := tx.ExecContext(ctx, `UPDATE spoke_leaf SET binding_id = ?, membership_gen = ? WHERE vendor = ? AND key = ?`,
				result.Membership.BindingID, result.Membership.Generation, leaf.Vendor, leaf.Key)
			return err
		}))
		s.logFailure(ident.RepublishMembership(dir, *result.Membership))
	}
	return result, nil
}

// settleOutbox closes a record: removed when nothing was sent, done with
// the home's answer otherwise.
func (s *server) settleOutbox(ctx context.Context, id int64, result *core.Result) {
	if id == 0 {
		return
	}
	s.logFailure(s.spokeMembers.db.Update(ctx, func(tx store.Tx) error {
		if result == nil {
			_, err := tx.ExecContext(ctx, `DELETE FROM link_outbox WHERE id = ?`, id)
			return err
		}
		raw, err := json.Marshal(result)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE link_outbox SET state = 'done', result = ? WHERE id = ?`, string(raw), id)
		return err
	}))
}

func (s *server) markUnknown(ctx context.Context, id int64) {
	if id == 0 {
		return
	}
	s.logFailure(s.spokeMembers.db.Update(ctx, func(tx store.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE link_outbox SET state = 'unknown' WHERE id = ?`, id)
		return err
	}))
}

// recoverOutbox looks up, receipt-only, every record that may have left
// the device without an answer: those the link ended on, and those a
// crash left as sent. A lookup never runs a command; the home fences what
// it has no receipt for.
func (s *server) recoverOutbox(ctx context.Context) {
	m := s.spokeMembers
	type record struct {
		id   int64
		call domain.LinkCall
	}
	var pending []record
	err := m.db.Update(ctx, func(tx store.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT id, vendor, key, op_key, command, args, membership FROM link_outbox
			WHERE state IN ('sent', 'unknown') ORDER BY id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r record
			var args string
			var membership sql.NullString
			if err := rows.Scan(&r.id, &r.call.Vendor, &r.call.Key, &r.call.OpKey, &r.call.Command, &args, &membership); err != nil {
				return err
			}
			r.call.Args = json.RawMessage(args)
			if membership.Valid {
				r.call.Membership = new(core.Membership)
				if err := json.Unmarshal([]byte(membership.String), r.call.Membership); err != nil {
					return err
				}
			}
			pending = append(pending, r)
		}
		return rows.Err()
	})
	if err != nil {
		s.logFailure(err)
		return
	}
	for _, r := range pending {
		result, err := m.exchange(ctx, "receipt", r.call, time.Now().Add(10*time.Second))
		if err != nil {
			return // the next connection tries again
		}
		s.settleOutbox(ctx, r.id, &result)
	}
}

// lostAnswer reports a forwarded command whose answer never came: the
// device looks it up on the next connection, and the member may send it
// again as a new request.
func lostAnswer(key string) core.Result {
	if key == "" {
		return unknownOutcome("the link to the home ended before its answer", "")
	}
	return domain.Fail(core.ExitUnavailable, core.CodeOutcomeUnknown, "the link to the home ended before its answer",
		"crosshall retry "+key+" shows what the hall did with it, or sends it again as a new request")
}

// spokeRetry answers crosshall retry from the outbox (#400): an outcome
// the home answered is shown as it is; a record without one is looked up
// receipt-only first; only a request the home fenced, which never ran
// and never will under its key, is sent again under the retry's own new
// key with its original bytes and the member's current selection. The
// returned request is that resend; done ends the retry with its answer.
func (s *server) spokeRetry(ctx context.Context, path string, dir *os.Root, req wire.Request) (wire.Request, *core.Result, error) {
	var retry domain.Retry
	if err := json.Unmarshal(req.Args, &retry); err != nil || retry.Key == "" || retry.Key == req.OpKey {
		r := commandError(core.ExitUsage, "", "usage: crosshall retry <key>")
		return req, &r, nil
	}
	m := s.spokeMembers
	var id int64
	var call domain.LinkCall
	var state string
	var stored sql.NullString
	err := m.db.Update(ctx, func(tx store.Tx) error {
		leaf, err := ident.ResolveSpokeMember(ctx, tx, m.members, ident.Ingress{Path: path, Dir: dir},
			ident.Proof{KeyDigest: req.KeyDigest, Generation: req.Generation})
		if err != nil {
			return err
		}
		var args string
		var membership sql.NullString
		err = tx.QueryRowContext(ctx, `SELECT id, command, args, membership, state, result FROM link_outbox
			WHERE vendor = ? AND key = ? AND op_key = ?`, leaf.Vendor, leaf.Key, retry.Key).
			Scan(&id, &call.Command, &args, &membership, &state, &stored)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		call.Vendor, call.Key, call.OpKey, call.Args = leaf.Vendor, leaf.Key, retry.Key, json.RawMessage(args)
		if err == nil && membership.Valid {
			call.Membership = new(core.Membership)
			err = json.Unmarshal([]byte(membership.String), call.Membership)
		}
		return err
	})
	var env *ident.EnvironmentError
	if errors.As(err, &env) {
		r := domain.Fail(core.ExitNotEnrolled, env.Code, env.Message, "crosshall join <channel>")
		return req, &r, nil
	}
	if err != nil {
		return req, nil, err
	}
	if id == 0 {
		r := domain.Refuse(core.CodeNotFound, "this device has no command of this session under that key",
			"crosshall log shows what the hall stored")
		return req, &r, nil
	}
	var answer core.Result
	if state == "done" && stored.Valid {
		if err := json.Unmarshal([]byte(stored.String), &answer); err != nil {
			return req, nil, err
		}
	} else {
		answer, err = m.exchange(ctx, "receipt", call, time.UnixMilli(req.ExpiresAtMS))
		if errors.Is(err, errNotSent) {
			r := domain.Fail(core.ExitUnavailable, core.CodeUnsent, "the link to the home is down; nothing was sent",
				"run crosshall link status, then crosshall retry "+retry.Key+" again")
			return req, &r, nil
		}
		if err != nil {
			r := lostAnswer(retry.Key)
			return req, &r, nil
		}
		s.settleOutbox(ctx, id, &answer)
	}
	if answer.Code != domain.CodeLinkFenced {
		return req, &answer, nil
	}
	req.Verb, req.Args = call.Command, call.Args
	return req, nil, nil
}
