package dispatch

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"os"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/hook"
	"github.com/BeomSeogKim/crosshall/internal/link"
	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

func serveSpoke(ctx context.Context, cancel context.CancelFunc, cfg Config, state *os.Root, linked linkConfig) error {
	root, err := openRendezvousRoot(cfg.Rendezvous)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, file := range []string{"ready", adapterPolicyName} {
		if err := root.Remove(file); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := state.Remove(hook.StatePolicyName); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	var instance [16]byte
	rand.Read(instance[:])
	members, err := openSpokeMembers(ctx, state, cfg.StateRoot, linked)
	if err != nil {
		return err
	}
	defer members.db.Close()
	ready := newReadyMarker(hex.EncodeToString(instance[:]))
	ready.Hello = nil
	ready.Spoke = linked.Target
	ready.Members = members.members
	s := server{cfg: cfg, state: state, ready: ready, cancel: cancel, spokeMembers: members}
	stopWatching := s.withdrawReadyOnCancel(ctx)
	defer stopWatching()
	if err := s.publishReady(ctx, root); err != nil {
		return err
	}
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		s.reconnectSpoke(ctx, linked)
	}()
	defer func() {
		cancel()
		<-workerDone
	}()

	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for ctx.Err() == nil {
		if err := s.scanSpoke(ctx); err != nil && !errors.Is(err, context.Canceled) {
			s.logFailure(err)
		}
		select {
		case <-ctx.Done():
		case <-tick.C:
		}
	}
	return nil
}

func (s *server) scanSpoke(ctx context.Context) error {
	owner, err := spool.OpenServerChild(s.state, "owner", false)
	if err != nil {
		return err
	}
	defer owner.Close()
	now := time.Now()
	reap := !now.Before(s.nextReap)
	s.runSpool(owner, reap, now, func(id string, raw []byte) (core.Result, error) {
		return s.spokeOwnerCommand(ctx, id, raw)
	})
	if err := ctx.Err(); err != nil {
		return err
	}
	root, err := openRendezvousRoot(s.cfg.Rendezvous)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := s.publishReady(ctx, root); err != nil {
		return err
	}
	if reap {
		s.nextReap = now.Add(time.Hour)
		s.renewRendezvousFiles(root, now)
	}
	s.scanSpokeMembers(ctx, reap, now)
	return nil
}

func (s *server) spokeOwnerCommand(ctx context.Context, id string, raw []byte) (core.Result, error) {
	if err := ctx.Err(); err != nil {
		return core.Result{}, err
	}
	var req wire.Request
	if err := json.Unmarshal(raw, &req); err != nil || req.Version != wire.Version || req.Attempt != id {
		return commandError(core.ExitUsage, "", "invalid command envelope"), nil
	}
	if req.ExpiresAtMS <= time.Now().UnixMilli() {
		return unknownOutcome("attempt expired", req.OpKey), nil
	}
	if req.Reply != nil && !*req.Reply {
		return commandError(core.ExitUsage, "", "only hook observations may omit a reply"), nil
	}
	if req.KeyDigest != "" || req.Generation != "" {
		return commandError(core.ExitRefused, core.CodeOwnerOnly, "dispatcher control requires owner ingress"), nil
	}
	if req.Verb != wire.VerbOwnerReady && req.Verb != wire.VerbOwnerStop {
		return domain.Fail(core.ExitUnavailable, core.CodeDispatcherUnavailable, "local commands are unavailable while linked to "+s.ready.Spoke,
			"run crosshall link status to check the link or crosshall unlink to restore local mode"), nil
	}
	var requested wire.DispatcherInstance
	if err := json.Unmarshal(req.Args, &requested); err != nil || requested.Instance == "" || requested.Instance != s.ready.Instance {
		return commandError(core.ExitRefused, "", "dispatcher readiness instance changed"), nil
	}
	result, err := domain.OK(wire.DispatcherInstance{Instance: s.ready.Instance})
	if err == nil && req.Verb == wire.VerbOwnerStop {
		s.cancel()
		s.withdrawReady()
	}
	return result, err
}

func (s *server) reconnectSpoke(ctx context.Context, cfg linkConfig) {
	status := spokeStatusRecord{Instance: s.ready.Instance, ConfigID: cfg.ID, Status: link.Status{Target: cfg.Target}}
	publish := func(next string) {
		status.State = next
		if err := spool.PublishServerFile(s.state, spokeStatusFile, status); err != nil {
			log.Printf("dispatcher: publish link status: %v", err)
		}
	}
	defer func() {
		status.Error = ""
		publish("stopped")
	}()
	spoke := cfg.spoke(s.cfg.Version)
	backoff := time.Second
	for ctx.Err() == nil {
		publish("connecting")
		started := time.Now()
		connection, err := link.DialSSH(ctx, cfg.Endpoint, spoke)
		if err == nil {
			status.HomeRelease = connection.PeerRelease
			status.Version = connection.Link
			status.ClockOffsetMS = link.EstimateClockOffset(started, connection.PeerTimeMS)
			status.Error = ""
			publish("connected")
			connectedAt := time.Now()
			s.spokeMembers.setConn(connection)
			s.recoverOutbox(ctx)
			err = connection.Wait()
			s.spokeMembers.setConn(nil)
			if time.Since(connectedAt) >= 30*time.Second {
				backoff = time.Second
			}
		}
		if ctx.Err() != nil {
			return
		}
		status.Error = "SSH connection ended"
		if err != nil {
			status.Error = err.Error()
		}
		publish("disconnected")
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
			backoff = min(2*backoff, time.Minute)
		}
	}
}
