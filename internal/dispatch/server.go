package dispatch

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

// Arrival is the server-selected input spool and its verified directory handle.
type Arrival struct {
	Path  string
	Dir   *os.Root
	Owner bool

	admittedOneWay bool // Set only by the member spool after admission.
}

// Resolution is the dispatcher-facing caller and first-join handoff.
type Resolution struct {
	Caller     core.Caller
	Vendor     string
	SessionKey string
	Enroll     func(context.Context, store.Tx, int64) error
	Refusal    *core.Result
}

// Services connects the dispatcher to identity and domain handlers.
// Decode passes Vendor and Enroll on the typed domain request.
type Services struct {
	Resolve func(context.Context, store.Tx, Arrival, wire.Request) (Resolution, error)
	Decode  func(wire.Request, Resolution) (domain.Request, error)
	Handle  domain.Handler
	// Hello may supply a filesystem publication to run after its transaction commits.
	Hello func(context.Context, store.Tx, []byte) (core.Result, func() error, error)
}

// Config supplies the daemon's resolved paths and application services.
type Config struct {
	LaunchConfig
	Services
	Publish PublishTransport
}

type server struct {
	cfg             Config
	state           *os.Root
	db              *store.Store
	watch           *fsnotify.Watcher
	ready           readyMarker
	readyMu         sync.Mutex
	readyLease      *os.File
	cancel          context.CancelFunc
	nextReap        time.Time
	lastLogged      string
	adapterSequence uint64
}

// Serve runs in the dedicated dispatcher process until ctx is canceled.
// A competing instance exits without opening the store.
func Serve(ctx context.Context, cfg Config) error {
	if err := CheckStateRoot(cfg.Version, cfg.StateRoot, cfg.DefaultStateRoot); err != nil {
		return err
	}
	if cfg.Resolve == nil || cfg.Decode == nil || cfg.Handle == nil || cfg.Hello == nil {
		return errors.New("dispatcher services are incomplete")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if err := prepareDaemonProcess(); err != nil {
		return err
	}
	state, err := spool.OpenServerRoot(cfg.StateRoot)
	if err != nil {
		return err
	}
	defer state.Close()
	lock, err := spool.OpenServerFile(state, "dispatcher.lock", os.O_CREATE|os.O_RDWR)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil
		}
		return err
	}
	if err := prepareNamedSpool(state, "owner"); err != nil {
		return err
	}
	root, err := openRendezvous(cfg.Rendezvous)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.Remove("ready"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	db, err := store.Open(ctx, cfg.StateRoot)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := pruneOrphanedLeaves(ctx, db, root); err != nil {
		return fmt.Errorf("prune stale member directories: %w", err)
	}
	watcher, err := fsnotify.NewWatcher()
	var events <-chan fsnotify.Event
	var watchErrors <-chan error
	if err != nil {
		log.Printf("dispatcher: directory watches unavailable: %v", err)
	} else {
		defer watcher.Close()
		events, watchErrors = watcher.Events, watcher.Errors
	}
	var instance [16]byte
	rand.Read(instance[:])
	s := server{cfg: cfg, state: state, db: db, watch: watcher, cancel: cancel,
		ready: readyMarker{Instance: hex.EncodeToString(instance[:]), Version: wire.Version, PID: os.Getpid(), Lease: true, Probe: true}}
	publish := &publisher{db: db, transport: cfg.Publish, profiles: domain.TestedPublishProfiles}
	if err := publish.recover(ctx, time.Now()); err != nil {
		return err
	}
	if cfg.Publish.Send != nil {
		defer func() {
			cancel()
			publish.workers.Wait()
		}()
	}
	stopWatching := s.withdrawReadyOnCancel(ctx)
	defer stopWatching()
	if err := s.scan(ctx); err != nil {
		return err
	}
	if cfg.Publish.Send != nil {
		logPublishFailure(publish.tick(ctx, time.Now(), true))
	}
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		publishTick := false
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			publishTick = true
		case _, ok := <-events:
			if !ok {
				events = nil
			}
		case err, ok := <-watchErrors:
			if !ok {
				watchErrors = nil
			} else {
				log.Printf("dispatcher: watch error: %v", err)
			}
		}
		if err := s.scan(ctx); err != nil && !errors.Is(err, context.Canceled) {
			s.logFailure(err)
		}
		if publishTick && cfg.Publish.Send != nil {
			logPublishFailure(publish.tick(ctx, time.Now(), false))
		}
	}
}

func openRendezvous(path string) (*os.Root, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("rendezvous path must be absolute")
	}
	if err := os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	root, err := spool.OpenServerRoot(path)
	if err != nil {
		return nil, err
	}
	if err := prepareNamedSpool(root, "first"); err != nil {
		root.Close()
		return nil, err
	}
	members, err := spool.OpenServerChild(root, "m", true)
	if err != nil {
		root.Close()
		return nil, err
	}
	members.Close()
	return root, nil
}

type memberLeaf struct {
	digest   string
	identity string
}

// Only startup runs this, while holding the dispatcher lock and before
// processing first contact. The store retains every registered prefix.
func pruneOrphanedLeaves(ctx context.Context, db *store.Store, rendezvous *os.Root) error {
	registered := make(map[string]bool)
	if err := db.Update(ctx, func(tx store.Tx) error {
		rows, err := tx.QueryContext(ctx, "SELECT substr(digest, 1, 16) FROM leaf")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var prefix string
			if err := rows.Scan(&prefix); err != nil {
				return err
			}
			registered[prefix] = true
		}
		return rows.Err()
	}); err != nil {
		return err
	}
	members, err := spool.OpenServerChild(rendezvous, "m", false)
	if err != nil {
		return err
	}
	defer members.Close()
	entries, err := fs.ReadDir(members.FS(), ".")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || len(name) != 16 || registered[name] ||
			strings.Trim(name, "0123456789abcdef") != "" {
			continue
		}
		leaf, err := spool.OpenServerChild(members, name, false)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			log.Printf("dispatcher: skipping unverified orphan %s: %v", name, err)
			continue
		}
		err = removeVerifiedLeaf(members, leaf, name)
		leaf.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func removeVerifiedLeaf(members, leaf *os.Root, name string) error {
	opened, err := leaf.Stat(".")
	if err != nil {
		return err
	}
	current, err := members.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !os.SameFile(opened, current) {
		return fmt.Errorf("orphaned member directory %s changed while pruning", name)
	}
	return members.RemoveAll(name)
}

func (s *server) scan(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	root, err := openRendezvous(s.cfg.Rendezvous)
	if err != nil {
		return err
	}
	defer root.Close()
	first, err := spool.OpenServerChild(root, "first", false)
	if err != nil {
		return err
	}
	defer first.Close()
	owner, err := spool.OpenServerChild(s.state, "owner", false)
	if err != nil {
		return err
	}
	defer owner.Close()
	watched := s.beginWatches()
	watched.add(filepath.Join(s.cfg.Rendezvous, "first", "in"))
	watched.add(filepath.Join(s.cfg.StateRoot, "owner", "in"))
	now := time.Now()
	reap := !now.Before(s.nextReap)
	s.runSpool(first, reap, now, func(id string, raw []byte) (core.Result, error) {
		return s.hello(ctx, id, raw)
	})
	s.runSpool(owner, reap, now, func(id string, raw []byte) (core.Result, error) {
		return s.command(ctx, Arrival{Path: filepath.Join(s.cfg.StateRoot, "owner"), Dir: owner, Owner: true}, id, raw)
	})
	if err := ctx.Err(); err != nil {
		return err
	}
	leaves, err := s.leaves(ctx)
	if err != nil {
		return err
	}
	members, err := spool.OpenServerChild(root, "m", false)
	if err != nil {
		return err
	}
	defer members.Close()
	for _, leaf := range leaves {
		s.runMember(ctx, members, watched, leaf, reap, now)
	}
	watched.removeStale()
	if reap {
		s.nextReap = now.Add(time.Hour)
	}
	return s.publishReady(ctx, root)
}

func (s *server) publishReady(ctx context.Context, root *os.Root) error {
	s.readyMu.Lock()
	defer s.readyMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	sameLease := false
	if s.readyLease != nil {
		held, statErr := s.readyLease.Stat()
		atPath, pathErr := root.Lstat(spool.ReadyLeaseFile)
		sameLease = statErr == nil && pathErr == nil && atPath.Mode() == 0600 && os.SameFile(held, atPath)
	}
	if sameLease && readReadyAt(root) == s.ready {
		return nil
	}
	if err := root.Remove("ready"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if !sameLease {
		lease, err := spool.HoldReadyLease(root, s.ready.Instance)
		if err != nil {
			return err
		}
		if s.readyLease != nil {
			s.readyLease.Close()
		}
		s.readyLease = lease
	}
	// After startup or a purge, the snapshots and held lease must be visible
	// in the same rendezvous before hooks can trust this ready marker.
	if err := s.publishAdapterFilesAt(ctx, root); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return spool.PublishServerFile(root, "ready", s.ready)
}

// Withdraw readiness as soon as shutdown starts, even if a scan is still
// unwinding. Wait for the withdrawal before releasing the state lock.
func (s *server) withdrawReadyOnCancel(ctx context.Context) func() {
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		s.withdrawReady()
		close(done)
	})
	return func() {
		if stop() {
			s.withdrawReady()
		} else {
			<-done
		}
	}
}

func (s *server) withdrawReady() {
	s.readyMu.Lock()
	defer s.readyMu.Unlock()
	if s.readyLease != nil {
		s.readyLease.Close()
		s.readyLease = nil
	}
	if root, err := spool.OpenServerRoot(s.cfg.Rendezvous); err == nil {
		root.Remove("ready")
		root.Close()
	}
}

func (s *server) runSpool(dir *os.Root, reap bool, now time.Time, handle func(string, []byte) (core.Result, error)) {
	if reap {
		s.logFailure(spool.ReapServerSpool(dir, now.Add(-24*time.Hour)))
	}
	s.logFailure(spool.DrainServerSpool(dir, handle))
}

func (s *server) runMember(ctx context.Context, members *os.Root, watched watchSet, leaf memberLeaf, reap bool, now time.Time) {
	path := filepath.Join(s.cfg.Rendezvous, "m", leaf.digest[:16])
	dir, err := spool.OpenServerChild(members, leaf.digest[:16], false)
	if errors.Is(err, os.ErrNotExist) {
		return // The client can restore an absent leaf through first contact.
	}
	if err != nil {
		s.logFailure(err)
		return
	}
	defer dir.Close()
	identity, err := spool.DirectoryIdentity(dir)
	if err != nil || identity != leaf.identity {
		s.logFailure(fmt.Errorf("registered member directory changed: %s", path))
		return
	}
	watched.add(filepath.Join(path, "in"))
	if reap {
		s.logFailure(spool.ReapServerSpool(dir, now.Add(-24*time.Hour)))
	}
	s.logFailure(spool.DrainMemberSpool(dir, func(id string, raw []byte, admittedOneWay bool) (core.Result, error) {
		return s.command(ctx, Arrival{Path: path, Dir: dir, admittedOneWay: admittedOneWay}, id, raw)
	}))
}

func (s *server) leaves(ctx context.Context) ([]memberLeaf, error) {
	var leaves []memberLeaf
	err := s.db.Update(ctx, func(tx store.Tx) error {
		// A client may still have its old leaf after a key rotation. Drain
		// that directory so the resolver can return the retirement refusal.
		rows, err := tx.QueryContext(ctx, "SELECT digest, file_identity FROM leaf ORDER BY digest")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var leaf memberLeaf
			if err := rows.Scan(&leaf.digest, &leaf.identity); err != nil {
				return err
			}
			leaves = append(leaves, leaf)
		}
		return rows.Err()
	})
	return leaves, err
}

func (s *server) logFailure(err error) {
	if err != nil && err.Error() != s.lastLogged {
		log.Printf("dispatcher: %v", err)
		s.lastLogged = err.Error()
	}
}

type watchSet struct {
	server    *server
	unvisited map[string]bool
}

func (s *server) beginWatches() watchSet {
	set := watchSet{server: s, unvisited: make(map[string]bool)}
	if s.watch != nil {
		for _, path := range s.watch.WatchList() {
			set.unvisited[path] = true
		}
	}
	return set
}

func (w watchSet) add(path string) {
	if w.unvisited[path] {
		delete(w.unvisited, path)
	} else if w.server.watch != nil {
		w.server.logFailure(w.server.watch.Add(path))
	}
}

func (w watchSet) removeStale() {
	for path := range w.unvisited {
		w.server.watch.Remove(path)
	}
}

var errRefusalRollback = errors.New("refusal rolled back")

func (s *server) hello(ctx context.Context, id string, raw []byte) (core.Result, error) {
	var envelope struct {
		Version int    `json:"v"`
		Attempt string `json:"attempt"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope.Version != wire.Version || envelope.Attempt != id {
		return commandError(core.ExitUsage, "", "invalid first-contact envelope"), nil
	}
	var result core.Result
	var publish func() error
	err := s.db.Update(ctx, func(tx store.Tx) error {
		var err error
		result, publish, err = s.cfg.Hello(ctx, tx, raw)
		if err == nil && result.Status != core.ExitOK {
			return errRefusalRollback
		}
		return err
	})
	if errors.Is(err, errRefusalRollback) {
		return result, nil
	}
	if err != nil {
		s.logFailure(err)
		return commandError(core.ExitError, "", "first contact failed; see dispatcher.log"), nil
	}
	if publish != nil {
		if err := publish(); err != nil {
			return core.Result{}, err // The next sweep can finish a committed registration.
		}
	}
	return result, nil
}

func (s *server) command(ctx context.Context, arrival Arrival, id string, raw []byte) (core.Result, error) {
	if err := ctx.Err(); err != nil {
		return core.Result{}, err
	}
	var req wire.Request
	if err := json.Unmarshal(raw, &req); err != nil || req.Version != wire.Version || req.Attempt != id {
		return commandError(core.ExitUsage, "", "invalid command envelope"), nil
	}
	oneWayHook := req.OneWayHook()
	if req.ExpiresAtMS <= time.Now().UnixMilli() && (!arrival.admittedOneWay || arrival.Owner || !oneWayHook) {
		return unknownOutcome("attempt expired", req.OpKey), nil
	}
	if req.Reply != nil && !*req.Reply && (arrival.Owner || !oneWayHook) {
		return commandError(core.ExitUsage, "", "only hook observations may omit a reply"), nil
	}
	isHook := req.IsHook()
	isWait := req.Verb == domain.VerbWait
	if isHook || isWait {
		req.Ack = 0 // Neither output acknowledges an ordinary roster.
	}
	refreshFiles := req.Verb == wire.VerbAdapterSet || req.Verb == domain.VerbChannelDefault || req.Verb == domain.VerbAdapterEvidence
	var result core.Result
	err := s.db.Update(ctx, func(tx store.Tx) error {
		resolved, err := s.cfg.Resolve(ctx, tx, arrival, req)
		if err != nil {
			return err
		}
		if resolved.Refusal != nil {
			result = *resolved.Refusal
			return errRefusalRollback
		}
		if req.Verb == wire.VerbOwnerReady || req.Verb == wire.VerbOwnerStop {
			if !arrival.Owner || resolved.Caller.Role != core.RoleOwner {
				result = commandError(core.ExitRefused, core.CodeOwnerOnly, "dispatcher control requires owner ingress")
				return errRefusalRollback
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			var probe wire.DispatcherInstance
			if err := json.Unmarshal(req.Args, &probe); err != nil || probe.Instance != s.ready.Instance || probe.Instance == "" {
				result = commandError(core.ExitRefused, "", "dispatcher readiness instance changed")
				return errRefusalRollback
			}
			result, err = domain.OK(wire.DispatcherInstance{Instance: s.ready.Instance})
			return err
		}
		var hook *domain.AdapterContext
		if isHook {
			var refusal core.Result
			hook, refusal, err = s.hookContext(ctx, tx, arrival, req, resolved)
			if err != nil {
				return err
			}
			if hook == nil {
				result = refusal
				return errRefusalRollback
			}
		}
		boundedAck, undoAck, err := acknowledgeRoster(ctx, tx, resolved.Caller.BindingID, req.Ack)
		if err != nil {
			return err
		}
		req.Ack = boundedAck
		request, err := s.cfg.Decode(req, resolved)
		if err != nil {
			result = commandError(core.ExitUsage, "", "invalid command arguments")
			return errRefusalRollback
		}
		request.Adapter = hook
		switch request.Command.Verb() {
		case domain.VerbCreateChannel, domain.VerbJoin, domain.VerbRead, domain.VerbCancel, domain.VerbBudgetGrant, domain.VerbRestore, "ask", "reply",
			domain.VerbNote, domain.VerbLeave, domain.VerbFocus, domain.VerbOwnerDepart, domain.VerbPublish, domain.VerbOwnerPublish:
			if req.OpKey == "" {
				result = commandError(core.ExitUsage, "", "an operation key is required for this command")
				return errRefusalRollback
			}
		}
		if policy, ok := request.Command.(domain.BudgetPolicy); ok && (policy.Refill != nil || policy.Ceiling != nil) && req.OpKey == "" {
			result = commandError(core.ExitUsage, "", "an operation key is required for this command")
			return errRefusalRollback
		}
		result, err = s.cfg.Handle.Handle(ctx, tx, resolved.Caller, request)
		if err != nil {
			return err
		}
		if result.Status != core.ExitOK {
			if resolved.Caller.Role == core.RoleUnenrolled {
				return errRefusalRollback
			}
			if undoAck != nil {
				return undoAck()
			}
			return nil
		}
		if refreshFiles {
			if err := s.invalidateAdapterFile(req.Verb); err != nil {
				return err
			}
		}
		if !isWait {
			if err := s.applyAdapterStamps(ctx, tx); err != nil {
				return err
			}
		}
		if isHook && !oneWayHook {
			return s.addHookAck(ctx, tx, arrival, req, &result)
		}
		return nil
	})
	if errors.Is(err, errRefusalRollback) {
		return result, nil
	}
	if err != nil {
		if ctx.Err() != nil {
			return core.Result{}, err // Keep the claim for recovery on shutdown.
		}
		s.logFailure(err)
		var uncertain *store.CommitError
		if errors.As(err, &uncertain) {
			return unknownOutcome("commit outcome unknown", req.OpKey), nil
		}
		return commandError(core.ExitError, "", "command failed; see dispatcher.log"), nil
	}
	if refreshFiles && result.Status == core.ExitOK {
		if err := s.publishAdapterFiles(ctx); err != nil {
			s.logFailure(err)
			return unknownOutcome("adapter state publication failed after commit", req.OpKey), nil
		}
	}
	if req.Verb == wire.VerbOwnerStop && result.Status == core.ExitOK {
		// Acknowledge the request only after retiring readiness. The
		// owner waits separately for the state lock to be released.
		s.cancel()
		s.withdrawReady()
	}
	return result, nil
}

// Apply the acknowledgement before the handler reads the roster. The undo
// restores just these fields on an enrolled refusal, leaving its audit intact.
func acknowledgeRoster(ctx context.Context, tx store.Tx, bindingID *int64, cursor uint64) (uint64, func() error, error) {
	if bindingID == nil || cursor == 0 {
		return 0, nil, nil
	}
	var previous, latest int64
	var hinted bool
	err := tx.QueryRowContext(ctx, `SELECT roster_ack_seq, hint_acked,
		(SELECT COALESCE(max(seq), 0) FROM roster_change WHERE channel_id = binding.channel_id)
		FROM binding WHERE id = ? AND state = 'live'`, *bindingID).Scan(&previous, &hinted, &latest)
	if err != nil {
		return 0, nil, err
	}
	bounded := min(cursor, uint64(max(latest, 0)))
	if bounded == 0 || (hinted && int64(bounded) <= previous) {
		return bounded, nil, nil
	}
	if _, err := tx.ExecContext(ctx, "UPDATE binding SET roster_ack_seq = ?, hint_acked = 1 WHERE id = ?",
		max(previous, int64(bounded)), *bindingID); err != nil {
		return 0, nil, err
	}
	return bounded, func() error {
		_, err := tx.ExecContext(ctx, "UPDATE binding SET roster_ack_seq = ?, hint_acked = ? WHERE id = ?",
			previous, hinted, *bindingID)
		return err
	}, nil
}

func unknownOutcome(message, key string) core.Result {
	if key == "" {
		return domain.Fail(core.ExitUnavailable, core.CodeOutcomeUnknown, message, "crosshall log shows whether it was stored; check before sending again")
	}
	if strings.ContainsAny(key, " \t\r\n'\"\\;$`&|*?()<>[]~!#{}") {
		key = "'" + strings.ReplaceAll(key, "'", "'\\''") + "'"
	}
	return domain.Fail(core.ExitUnavailable, core.CodeOutcomeUnknown, message, "retry with --key "+key)
}

func commandError(status core.ExitCode, code core.Code, message string) core.Result {
	data, _ := json.Marshal(struct {
		Message string `json:"message"`
	}{message})
	return core.Result{Status: status, Code: code, Data: data}
}
