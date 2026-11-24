package spool_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

func TestDelayedOutputAcknowledgementsSurviveMemberSpoolRecovery(t *testing.T) {
	f := setup(t)
	path := f.leaf(t)
	noReply := false
	old := time.Now().Add(-48 * time.Hour)
	for _, packet := range []struct{ section, digit, verb string }{
		{"in", "1", wire.VerbHookJoinHintAck},
		{"work", "2", wire.VerbHookJoinHintAck},
	} {
		req := wire.Request{Attempt: strings.Repeat(packet.digit, 32), Verb: packet.verb, OpKey: packet.verb,
			Reply: &noReply, ExpiresAtMS: old.UnixMilli()}
		writeMemberPacket(t, path, packet.section, req)
		if err := os.Chtimes(filepath.Join(path, packet.section, req.Attempt+".json"), old, old); err != nil {
			t.Fatal(err)
		}
	}
	leaf, err := os.OpenRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	defer leaf.Close()
	if err := spool.ReapServerSpool(leaf, time.Now().Add(-24*time.Hour), true); err != nil {
		t.Fatal(err)
	}
	writeMemberPacket(t, path, "in", wire.Request{Attempt: strings.Repeat("3", 32), Verb: wire.VerbHookRoute,
		Reply: &noReply, ExpiresAtMS: old.UnixMilli()})
	var got []string
	if err := spool.DrainMemberSpool(leaf, func(id string, _ []byte, oneWay bool) (core.Result, error) {
		if !oneWay {
			t.Fatal("acknowledgement was not one-way")
		}
		got = append(got, id)
		return core.Result{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	if want := []string{strings.Repeat("1", 32), strings.Repeat("2", 32)}; !slices.Equal(got, want) {
		t.Fatalf("late packets delivered = %q, want %q", got, want)
	}
}
