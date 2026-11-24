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
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/hook"
	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/publish"
	"github.com/BeomSeogKim/crosshall/internal/setup"
	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/telemetry"
	"github.com/BeomSeogKim/crosshall/internal/telemetryconfig"
	"github.com/BeomSeogKim/crosshall/internal/telemetrysend"
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
	// AfterCommit, when set, runs once the request's transaction committed.
	// It returns the member directory's new generation when it published one.
	AfterCommit func() (string, error)
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
	retention       retention
	lastLogged      string
	adapterSequence uint64
	revisions       revisionNotes
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
	if acquired, err := acquireDispatcherLock(ctx, lock); err != nil || !acquired {
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
		ready: newReadyMarker(hex.EncodeToString(instance[:]))}
	pub := &publisher{db: db, evidence: domain.BuiltinCapabilityEvidence, stamps: s.applyAdapterStamps,
		homes: func(adapter string) string { return setup.InstalledConfigDir(cfg.StateRoot, adapter) }}
	sends := len(publish.Adapters().Senders()) > 0
	if err := pub.recover(ctx, time.Now()); err != nil {
		return err
	}
	if sends {
		defer func() {
			cancel()
			pub.workers.Wait()
		}()
	}
	stopWatching := s.withdrawReadyOnCancel(ctx)
	defer stopWatching()
	if err := s.scan(ctx); err != nil {
		return err
	}
	if sends {
		logPublishFailure(pub.tick(ctx, time.Now(), true))
	}
	var launches launchSweep
	launches.start(ctx, db, time.Now())
	usage := telemetrysend.New(telemetrysend.Config{
		StateRoot: cfg.StateRoot, Rendezvous: cfg.Rendezvous, Version: cfg.Version,
		Endpoint: telemetry.Receiver(cfg.Version), PolicyPath: telemetryconfig.ManagedOffPath,
		Curl: telemetryconfig.CurlPath, Vetoed: telemetryconfig.EnvironmentOff(),
	}, db)
	defer func() {
		cancel()
		usage.Wait()
	}()
	version := cfg.Version
	notify, notifyArgs := notifyCommand(runtime.GOOS)
	if command, ok := testNotifyCommand(); ok {
		version, notify = "v0.0.0", command
	}
	alerts := newNotifier(db, version, notifyOff(), notify, notifyArgs)
	if err := alerts.recover(ctx); err != nil {
		return err
	}
	defer func() {
		cancel()
		alerts.wait()
	}()
	var nextUsage, nextAlert time.Time
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		publishTick := false
		select {
		case <-ctx.Done():
			return nil
		case result := <-launches.pending:
			if err := launches.apply(ctx, &s, result); err != nil && !errors.Is(err, context.Canceled) {
				s.logFailure(fmt.Errorf("reconcile Claude launches: %w", err))
			}
			continue
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
		if publishTick {
			now := time.Now()
			s.retentionTick(ctx, now)
			launches.start(ctx, db, now)
			if !now.Before(nextUsage) {
				usage.Tick(ctx, now)
				nextUsage = now.Add(time.Minute)
			}
			if !now.Before(nextAlert) {
				alerts.tick(ctx, now)
				nextAlert = now.Add(time.Second)
			}
			if sends {
				logPublishFailure(pub.tick(ctx, now, false))
			}
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
	digest     string
	identity   string
	generation string
	fenced     bool
	membership core.Membership
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
	if sameLease && readReadyAt(root).same(s.ready) {
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
	s.logFailure(s.adoptRepoDefaults(ctx))
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
		s.logFailure(spool.ReapServerSpool(dir, now.Add(-24*time.Hour), false))
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
	if leaf.fenced {
		s.logFailure(hook.RetireJoinHintFiles(dir))
	} else if err := ident.RepublishGeneration(dir, leaf.generation); !errors.Is(err, ident.ErrDirectoryBusy) {
		// A generation committed but not yet published (a renderer held the
		// directory, or the dispatcher restarted in between) is published
		// by the next pass.
		s.logFailure(err)
	}
	s.logFailure(ident.RepublishMembership(dir, leaf.membership))
	watched.add(filepath.Join(path, "in"))
	if reap {
		s.logFailure(spool.ReapServerSpool(dir, now.Add(-24*time.Hour), true))
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
		rows, err := tx.QueryContext(ctx, `SELECT l.digest, l.file_identity, l.generation, l.state = 'fenced',
			COALESCE(s.binding_id, 0), COALESCE(s.generation, 0)
			FROM leaf l LEFT JOIN member_selection s ON s.principal_id = l.principal_id ORDER BY l.digest`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var leaf memberLeaf
			if err := rows.Scan(&leaf.digest, &leaf.identity, &leaf.generation, &leaf.fenced,
				&leaf.membership.BindingID, &leaf.membership.Generation); err != nil {
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
		Version     int    `json:"v"`
		Attempt     string `json:"attempt"`
		ExpiresAtMS int64  `json:"expires_at_ms"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope.Version != wire.Version || envelope.Attempt != id {
		return commandError(core.ExitUsage, "", "invalid first-contact envelope"), nil
	}
	// Older clients omit the deadline.
	if envelope.ExpiresAtMS != 0 && envelope.ExpiresAtMS <= time.Now().UnixMilli() {
		return commandError(core.ExitUnavailable, core.CodeUnsent, "first contact expired"), nil
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
	outputAck := !arrival.Owner && oneWayHook && req.Verb == wire.VerbHookJoinHintAck
	if req.ExpiresAtMS <= time.Now().UnixMilli() && !outputAck && (!arrival.admittedOneWay || arrival.Owner || !oneWayHook) {
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
	refreshFiles := req.Verb == wire.VerbAdapterSet || req.Verb == domain.VerbChannelDefault || req.Verb == domain.VerbChannelAuto ||
		req.Verb == domain.VerbAdapterEvidence
	var result core.Result
	var enrolledSession ident.Identity
	var channelCreated bool
	var membershipToPublish *core.Membership
	var afterCommit func() (string, error)
	// lowered says this request's transaction wrote a "0" turn-end record:
	// if the transaction does not commit, the record goes, and the Stop waits.
	var lowered bool
	err := s.db.Update(ctx, func(tx store.Tx) error {
		resolved, err := s.cfg.Resolve(ctx, tx, arrival, req)
		if err != nil {
			return err
		}
		afterCommit = resolved.AfterCommit
		if resolved.Refusal != nil {
			result = *resolved.Refusal
			return errRefusalRollback
		}
		if !arrival.Owner && !matchesMembership(req.Membership, resolved.Caller.Membership) {
			if resolved.Caller.PrincipalID != nil && (req.Verb == domain.VerbJoin || req.Verb == domain.VerbLeave) {
				request, err := s.cfg.Decode(req, resolved)
				if err != nil {
					result = commandError(core.ExitUsage, "", "invalid command arguments")
					return errRefusalRollback
				}
				replay, found, err := domain.ReplayMembershipChange(ctx, tx, *resolved.Caller.PrincipalID, req.OpKey, request.Command)
				if err != nil {
					return err
				}
				if found {
					result = replay
					return errRefusalRollback
				}
			}
			result = membershipChanged(req)
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
		if req.Verb == wire.VerbHookJoinHintAck {
			if !outputAck || resolved.Caller.Role != core.RoleMember || resolved.Caller.PrincipalID == nil || req.OpKey != wire.VerbHookJoinHintAck {
				result = commandError(core.ExitRefused, "", "hook join acknowledgement requires member ingress")
				return errRefusalRollback
			}
			if err := acknowledgeHookJoinHint(ctx, tx, arrival, req, resolved.Caller); err != nil {
				return err
			}
			if err := acknowledgeHookReach(ctx, tx, arrival, req, resolved.Caller); err != nil {
				return err
			}
			var ack wire.HookOutputAckArgs
			if len(req.Args) > 0 && json.Unmarshal(req.Args, &ack) == nil && ack.Offer != nil {
				return domain.ConfirmHookOffer(ctx, tx, *resolved.Caller.PrincipalID, req.Generation, *ack.Offer, ack.WrittenAtMS)
			}
			return nil
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
		if err := acknowledgeHookJoinHint(ctx, tx, arrival, req, resolved.Caller); err != nil {
			return err
		}
		if err := acknowledgeHookReach(ctx, tx, arrival, req, resolved.Caller); err != nil {
			return err
		}
		membership := callerMembership(resolved.Caller)
		pendingAck, pendingRules := leafPendingReceipt(arrival, req.Generation, membership)
		clearedAck, clearedRules, err := s.clearedReceipts(ctx, tx, resolved.Caller, time.Now())
		if err != nil {
			return err
		}
		boundedAck, undoAck, err := domain.AcknowledgeRoster(ctx, tx, resolved.Caller.BindingID,
			max(req.Ack, pendingAck, clearedAck), max(leafRulesReceipt(arrival, membership), pendingRules, clearedRules))
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
		if set, ok := request.Command.(domain.AdapterSet); ok && set.Enabled == nil && set.Reminder == nil {
			// The hook policy file carries enabled, reminder and the epoch
			// only. The publish and turn-end switches are read from the
			// store, so a change to them leaves the file in place: hooks
			// read during the change would otherwise find no policy.
			refreshFiles = false
		}
		switch request.Command.Verb() {
		case domain.VerbCreateChannel, domain.VerbJoin, domain.VerbRead, domain.VerbCancel, domain.VerbBudgetGrant, domain.VerbRestore, "ask", "reply",
			domain.VerbNote, domain.VerbLeave, domain.VerbFocus, domain.VerbOwnerDepart, domain.VerbOwnerDepartEnded, domain.VerbPublish, domain.VerbOwnerPublish:
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
		if req.Verb == domain.VerbJoin || hookEnrolled(req, resolved.Caller, result) {
			enrolledSession = ident.Identity{Vendor: resolved.Vendor, Key: resolved.SessionKey}
			channelCreated = hookCreatedChannel(req, result)
			selected, err := leafSelectedMembership(ctx, tx, req.KeyDigest)
			if err != nil {
				return err
			}
			if req.Verb == domain.VerbJoin && req.Membership == nil && membership.BindingID != 0 &&
				selected.BindingID != 0 && selected.BindingID != membership.BindingID && resolved.Caller.PrincipalID != nil {
				result = domain.Fail(core.ExitRefused, core.CodeBindingChanged, "moving channels requires a current membership selection",
					"update crosshall or retry the command once the dispatcher is ready")
				return errRefusalRollback
			}
			if selected.BindingID != 0 {
				membershipToPublish = &selected
				if isHook {
					membership = selected
				}
			}
		}
		if req.Verb != domain.VerbJoin && membership.BindingID != 0 {
			result.Membership = &membership
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
		if isHook && !arrival.Owner && resolved.Caller.Role == core.RoleMember && resolved.Caller.PrincipalID != nil {
			if lowered, err = s.publishTurnEndAsk(ctx, tx, arrival.Dir, req.Generation, *resolved.Caller.PrincipalID, true); err != nil {
				return err
			}
		}
		if isHook && !oneWayHook {
			return s.addHookAck(ctx, tx, arrival, req, &result)
		}
		return nil
	})
	if err != nil && lowered {
		if removeErr := arrival.Dir.Remove(hook.TurnEndAskFile); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			s.logFailure(removeErr)
		}
	}
	if errors.Is(err, errRefusalRollback) {
		return result, nil
	}
	if err != nil {
		if ctx.Err() != nil || outputAck {
			return core.Result{}, err // Keep the claim on shutdown or retry an uncommitted output acknowledgement.
		}
		s.logFailure(err)
		var uncertain *store.CommitError
		if errors.As(err, &uncertain) {
			return unknownOutcome("commit outcome unknown", req.OpKey), nil
		}
		return commandError(core.ExitError, "", "command failed; see dispatcher.log"), nil
	}
	if afterCommit != nil {
		generation, err := afterCommit()
		s.logFailure(err)
		// The reply is acknowledged under the directory's new generation.
		if generation != "" && err == nil && result.Status == core.ExitOK {
			result.Data = withGeneration(result.Data, generation)
		}
	}
	if membershipToPublish != nil && arrival.Dir != nil {
		if err := ident.RepublishMembership(arrival.Dir, *membershipToPublish); err != nil {
			s.logFailure(err)
			return unknownOutcome("membership publication failed after commit", req.OpKey), nil
		}
	}
	if enrolledSession.Key != "" {
		s.logFailure(hook.RemoveStartupRepo(s.state, enrolledSession))
	}
	if channelCreated {
		// The repository set gained an entry; hooks tolerate the old one
		// while automatic joining stays on.
		s.logFailure(s.publishAdapterFiles(ctx))
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
		s.withdrawPolicy()
		s.withdrawReady()
	}
	return result, nil
}

// clearedReceiptWindow bounds how long after a /clear the replaced
// conversation's directory is still read. A command it was rendering then
// records its receipt within seconds, but a background crosshall wait it
// started before the clear (up to nine minutes) records its receipt only
// when it returns.
const clearedReceiptWindow = 10 * time.Minute

// clearedReceipts reads the receipts in the directories of the member's keys
// that a /clear retired recently. A command of the replaced conversation
// may still have been rendering when the clear took the member over, and
// records what it showed after the takeover read those directories; the
// member's next request applies it, so nothing it saw is shown again.
func (s *server) clearedReceipts(ctx context.Context, tx store.Tx, caller core.Caller, now time.Time) (cursor, rules uint64, err error) {
	if caller.PrincipalID == nil || caller.BindingID == nil {
		return 0, 0, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT k.vendor, k.key, l.file_identity, l.generation
		FROM principal_key k JOIN leaf l ON l.vendor = k.vendor AND l.key = k.key
		WHERE k.principal_id = ? AND k.state = 'retired' AND k.retired_by = 'clear' AND k.retired_at_ms >= ?`,
		*caller.PrincipalID, now.Add(-clearedReceiptWindow).UnixMilli())
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var id ident.Identity
		var fileID, generation string
		if err := rows.Scan(&id.Vendor, &id.Key, &fileID, &generation); err != nil {
			return 0, 0, err
		}
		c, r := ident.LeafReceipts(s.cfg.Rendezvous, id, fileID, generation, callerMembership(caller))
		cursor, rules = max(cursor, c), max(rules, r)
	}
	return cursor, rules, rows.Err()
}

// leafPendingReceipt reads the receipts a renderer left pending for the
// request's generation, which the resolver checked is the published one.
func leafPendingReceipt(arrival Arrival, generation string, membership core.Membership) (cursor, rules uint64) {
	if arrival.Owner || arrival.Dir == nil {
		return 0, 0
	}
	return ident.PendingReceipt(arrival.Dir, generation, membership)
}

// leafRulesReceipt reads the roster cursor of the reply with which the CLI
// printed the member rules, as it recorded it in the member's directory. A
// hook's own output acknowledges nothing, but its request finds this record
// like any other, so a prompt after one command knows the rules were given.
func leafRulesReceipt(arrival Arrival, membership core.Membership) uint64 {
	if arrival.Owner || arrival.Dir == nil {
		return 0
	}
	cursor, _ := ident.ReadRosterReceipt(arrival.Dir, spool.RulesReceipt, membership)
	return cursor
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

// withGeneration adds the member directory's new generation to a reply's
// data object, for the client to acknowledge the reply under it.
func withGeneration(data json.RawMessage, generation string) json.RawMessage {
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || fields == nil {
		return data
	}
	fields["generation"], _ = json.Marshal(generation)
	patched, err := json.Marshal(fields)
	if err != nil {
		return data
	}
	return patched
}
