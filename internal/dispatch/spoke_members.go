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

	// turn admits one frame exchange at a time; waiting for it is bounded
	// by the caller's deadline.
	turn chan struct{}
	mu   sync.Mutex
	conn spokeConn // nil while the link is down or recovering
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
	return &spokeMembers{db: db, members: members, turn: make(chan struct{}, 1)}, nil
}

// setConn installs or clears the link the member commands go over. A new
// link is published only after recovery, so no fresh command is sent
// before every record the previous link left behind is resolved.
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

func (m *spokeMembers) current() spokeConn {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.conn
}

// drop closes a link whose exchange failed, so a late answer is never read
// as the next command's, and the reconnect loop dials again.
func (m *spokeMembers) drop(c spokeConn) {
	c.Close()
	m.mu.Lock()
	if m.conn == c {
		m.conn = nil
	}
	m.mu.Unlock()
}

// errNotSent says no byte of the frame reached the link.
var errNotSent = errors.New("the link to the home is down")

// take waits for the exchange turn until the deadline.
func (m *spokeMembers) take(ctx context.Context, deadline time.Time) bool {
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case m.turn <- struct{}{}:
		return true
	case <-timer.C:
	case <-ctx.Done():
	}
	return false
}

func (m *spokeMembers) give() { <-m.turn }

// exchange sends one frame over the published link and reads its result,
// all by the deadline. Nothing is written once the deadline has passed,
// or while the link is down or recovering: that is errNotSent.
func (m *spokeMembers) exchange(ctx context.Context, frame string, call domain.LinkCall, deadline time.Time) (core.Result, error) {
	if !m.take(ctx, deadline) {
		return core.Result{}, errNotSent
	}
	defer m.give()
	conn := m.current()
	if conn == nil || !time.Now().Before(deadline) {
		return core.Result{}, errNotSent
	}
	return m.roundTrip(ctx, conn, frame, call, deadline)
}

// roundTrip writes and reads with the caller holding the turn. The write
// and the read both run against the deadline; on any failure the link is
// closed.
func (m *spokeMembers) roundTrip(ctx context.Context, conn spokeConn, frame string, call domain.LinkCall, deadline time.Time) (core.Result, error) {
	type answer struct {
		f   link.Frame
		err error
	}
	got := make(chan answer, 1)
	go func() {
		if err := conn.Write(frame, call); err != nil {
			got <- answer{err: err}
			return
		}
		f, err := conn.Read()
		got <- answer{f, err}
	}()
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		m.drop(conn)
		return core.Result{}, ctx.Err()
	case <-timer.C:
		m.drop(conn)
		return core.Result{}, errors.New("the home did not answer in time")
	case a := <-got:
		var result core.Result
		if a.err != nil || a.f.Type != "result" || json.Unmarshal(a.f.Body, &result) != nil {
			m.drop(conn)
			return core.Result{}, fmt.Errorf("link: no result frame: %v", a.err)
		}
		if result.Code == domain.CodeLinkStale {
			m.drop(conn)
			return core.Result{}, errors.New("the home closed this link")
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

// spokeCommand forwards one member command to the home. It takes the
// link's turn first, so a recovery never sees its record; then a keyed
// command is written to the outbox, committed, before its first byte can
// reach the link. A key already in the outbox is never reset: other bytes
// are a conflict, an answered one shows its answer, and an unresolved one
// is looked up receipt-only.
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
	if req.Verb == domain.VerbJoin {
		req.Args = withoutLocalLaunch(req.Args)
	}
	deadline := time.UnixMilli(req.ExpiresAtMS)
	unsent := domain.Fail(core.ExitUnavailable, core.CodeUnsent, "the link to the home is down; nothing was sent",
		"run crosshall link status, then send the command again")
	if !m.take(ctx, deadline) {
		return unsent, nil
	}
	defer m.give()
	conn := m.current()
	if conn == nil || !time.Now().Before(deadline) {
		return unsent, nil
	}
	var leaf ident.SpokeLeaf
	var rec outboxRecord
	err := m.db.Update(ctx, func(tx store.Tx) error {
		var err error
		leaf, err = ident.ResolveSpokeMember(ctx, tx, m.members, ident.Ingress{Path: path, Dir: dir},
			ident.Proof{KeyDigest: req.KeyDigest, Generation: req.Generation})
		if err != nil || req.OpKey == "" {
			return err
		}
		rec, err = recordOutbox(ctx, tx, leaf, req)
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
	switch {
	case rec.conflict:
		return domain.Refuse(core.CodeKeyConflict, "this operation key was already used for a different command",
			"crosshall retry "+req.OpKey+" shows what the hall did with the original"), nil
	case rec.answer != nil:
		return *rec.answer, nil
	case !rec.fresh && req.OpKey != "":
		// The same request, still unresolved: look it up, never resend.
		call.Command, call.Args, call.Membership = rec.command, rec.args, rec.membership
		answer, err := m.roundTrip(ctx, conn, "receipt", call, deadline)
		if err != nil {
			return lostAnswer(req.OpKey), nil
		}
		s.settleOutbox(ctx, rec.id, &answer)
		return answer, nil
	}
	result, err := m.roundTrip(ctx, conn, "call", call, deadline)
	if err != nil {
		s.markUnknown(ctx, rec.id)
		return lostAnswer(req.OpKey), nil
	}
	s.settleOutbox(ctx, rec.id, &result)
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

// withoutLocalLaunch drops the launch a member client adds to a join on
// its own: it names a process on this device, which the home never binds
// (#400). Anything else in the join stays as the member wrote it.
func withoutLocalLaunch(args json.RawMessage) json.RawMessage {
	var fields map[string]json.RawMessage
	if json.Unmarshal(args, &fields) != nil {
		return args
	}
	if _, ok := fields["launch"]; !ok {
		return args
	}
	delete(fields, "launch")
	out, err := json.Marshal(fields)
	if err != nil {
		return args
	}
	return out
}

// outboxRecord is a keyed command's record as recordOutbox found or wrote it.
type outboxRecord struct {
	id         int64
	fresh      bool
	conflict   bool
	answer     *core.Result
	command    string
	args       json.RawMessage
	membership *core.Membership
}

// recordOutbox writes a new keyed command's record, or reads the existing
// one under that key without changing it.
func recordOutbox(ctx context.Context, tx store.Tx, leaf ident.SpokeLeaf, req wire.Request) (outboxRecord, error) {
	var membership sql.NullString
	if req.Membership != nil {
		raw, err := json.Marshal(req.Membership)
		if err != nil {
			return outboxRecord{}, err
		}
		membership = sql.NullString{String: string(raw), Valid: true}
	}
	var rec outboxRecord
	var args, state string
	var storedMembership, stored sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT id, command, args, membership, state, result FROM link_outbox
		WHERE vendor = ? AND key = ? AND op_key = ?`, leaf.Vendor, leaf.Key, req.OpKey).
		Scan(&rec.id, &rec.command, &args, &storedMembership, &state, &stored)
	if errors.Is(err, sql.ErrNoRows) {
		rec.fresh = true
		err = tx.QueryRowContext(ctx, `INSERT INTO link_outbox (vendor, key, op_key, command, args, membership, state, at_ms)
			VALUES (?, ?, ?, ?, ?, ?, 'sent', ?) RETURNING id`,
			leaf.Vendor, leaf.Key, req.OpKey, req.Verb, string(req.Args), membership, time.Now().UnixMilli()).Scan(&rec.id)
		return rec, err
	}
	if err != nil {
		return rec, err
	}
	if rec.command != req.Verb || args != string(req.Args) || storedMembership != membership {
		rec.conflict = true
		return rec, nil
	}
	rec.args = json.RawMessage(args)
	if storedMembership.Valid {
		rec.membership = new(core.Membership)
		if err := json.Unmarshal([]byte(storedMembership.String), rec.membership); err != nil {
			return rec, err
		}
	}
	if state == "done" && stored.Valid {
		rec.answer = new(core.Result)
		return rec, json.Unmarshal([]byte(stored.String), rec.answer)
	}
	return rec, nil
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
// the device without an answer (those a link ended on, and those a crash
// left as sent), on a new link before it is published: the set is fixed
// while recovery holds the turn, so no fresh command's record is in it
// and none is sent until recovery is done. A lookup never runs a command;
// the home fences what it has no receipt for. It reports whether the
// link may be published.
func (s *server) recoverOutbox(ctx context.Context, conn spokeConn) bool {
	m := s.spokeMembers
	if !m.take(ctx, time.Now().Add(time.Minute)) {
		conn.Close()
		return false
	}
	defer m.give()
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
		conn.Close()
		return false
	}
	for _, r := range pending {
		result, err := m.roundTrip(ctx, conn, "receipt", r.call, time.Now().Add(10*time.Second))
		if err != nil {
			conn.Close()
			return false // the next connection tries again
		}
		s.settleOutbox(ctx, r.id, &result)
	}
	return true
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
