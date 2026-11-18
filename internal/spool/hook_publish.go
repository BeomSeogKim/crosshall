package spool

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"syscall"
	"time"

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
		if !errors.Is(err, syscall.EWOULDBLOCK) {
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
