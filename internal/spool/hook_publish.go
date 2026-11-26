package spool

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"syscall"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/hostclock"
	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

func publishHookRequest(ctx context.Context, leaf, in *os.Root, req wire.Request) error {
	lock, err := OpenServerFile(leaf, "hook-publish.lock", os.O_CREATE|os.O_RDWR)
	if err != nil {
		return err
	}
	defer lock.Close()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if req.Verb == wire.VerbHookJoinHintAck || !errors.Is(err, syscall.EWOULDBLOCK) {
			return err
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	const counter = "hook-publish-seq"
	last, err := readSequence(leaf, counter)
	if err != nil {
		return err
	}
	req.PublishSeq = last + 1
	if req.PublishSeq == 0 {
		return errors.New("hook publication sequence exhausted")
	}
	if req.Capture != nil {
		// The causal token (design 6a): what the leaf held at this
		// sequence, under the lock its dispatcher stamps it under.
		capture, err := captureHook(leaf)
		if err != nil {
			return err
		}
		// The request was prepared for the membership it pins; a stamp
		// that changed before this lock is another membership's turn.
		if req.Membership != nil && (capture.Membership == nil || *capture.Membership != *req.Membership) {
			return errors.New("membership changed before hook publication")
		}
		req.Capture = &capture
	}
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	// Reserve the sequence on disk before publication so an interrupted writer
	// can leave a gap but cannot reuse a previously published sequence.
	if err := publishFile(ctx, leaf, counter+"-"+req.Attempt+".part", counter, []byte(strconv.FormatUint(req.PublishSeq, 10)+"\n")); err != nil {
		return err
	}
	return publishFile(ctx, in, req.Attempt+".part", req.Attempt+".json", data)
}

func captureHook(leaf *os.Root) (wire.HookCapture, error) {
	membership, err := ident.LookupMembership(leaf)
	if err != nil {
		return wire.HookCapture{}, err
	}
	boot, err := hostclock.Boot()
	if err != nil {
		return wire.HookCapture{}, err
	}
	mono, err := hostclock.Mono()
	if err != nil {
		return wire.HookCapture{}, err
	}
	return wire.HookCapture{Boot: boot, MonoNS: mono, WallMS: time.Now().UnixMilli(), Membership: membership}, nil
}
