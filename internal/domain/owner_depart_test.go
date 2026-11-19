package domain

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
)

func TestOwnerDepartFencesHooksAndKeepsReplayFromDepartingTheReturn(t *testing.T) {
	f, _ := adapterChannel(t)
	member := f.newSession("claude")
	started := f.now.UnixMilli()
	f.hook(member, start("pid-1", "startup", started))
	principal := *f.caller(member).PrincipalID

	listDeparture := func() Command {
		t.Helper()
		var listing struct {
			Channels []struct {
				Members []struct {
					Alias string `json:"alias"`
					ID    int64  `json:"binding_id"`
					Token string `json:"depart_token"`
				} `json:"members"`
			} `json:"channels"`
		}
		r := f.owner(OwnerWho{}, "")
		wantOK(t, r)
		if err := json.Unmarshal(r.Data, &listing); err != nil {
			t.Fatal(err)
		}
		for _, ch := range listing.Channels {
			for _, row := range ch.Members {
				if row.Alias != "claude1" {
					continue
				}
				if row.ID == 0 || row.Token == "" {
					t.Fatalf("owner listing needs a departure token: %+v", row)
				}
				args, err := json.Marshal(map[string]any{"binding_id": row.ID, "seen": row.Token})
				if err != nil {
					t.Fatal(err)
				}
				cmd, err := Decode("owner depart", args)
				if err != nil {
					t.Fatal(err)
				}
				return cmd
			}
		}
		t.Fatal("missing Claude binding")
		return nil
	}
	stale := listDeparture()
	f.hook(member, prompt("pid-1", "turn-1", started+1))
	if r := f.owner(stale, "stale"); r.Status != core.ExitRefused || r.Code != "binding_changed" {
		t.Fatalf("activity after listing must refuse: %s", r.Data)
	}

	depart := listDeparture()
	wantOK(t, f.owner(depart, "depart"))
	rows := func() [5]int {
		return [5]int{
			f.count("SELECT COUNT(*) FROM observation WHERE principal_id = ?", principal),
			f.count("SELECT COUNT(*) FROM adapter_launch WHERE principal_id = ?", principal),
			f.count("SELECT COUNT(*) FROM op WHERE principal_id = ?", principal),
			f.count("SELECT COUNT(*) FROM turn WHERE principal_id = ?", principal),
			f.count("SELECT COUNT(*) FROM roster_change WHERE principal_id = ?", principal),
		}
	}
	before := rows()
	if event, enrollment := f.hook(member, stop("pid-1", "turn-1", started+2)),
		f.hook(member, start("pid-2", "startup", started+3)); !quiet(event) || !quiet(enrollment) {
		t.Fatalf("late hooks emitted output: %+v, %+v", event, enrollment)
	}
	if after := rows(); after != before {
		t.Fatalf("late hooks changed rows: before=%v after=%v", before, after)
	}

	// These callbacks were captured after departure, but stayed in flight
	// until the same key explicitly returned. Neither belongs to the return.
	delayedStart := start("pid-1", "compact", started+4)
	delayedEnd := ended("pid-1", started+5)
	f.now = time.UnixMilli(started + 6)
	wantOK(t, f.join(member, Join{Channel: "checkout"}, "return"))
	wantOK(t, f.owner(depart, "depart"))
	if live := f.count("SELECT COUNT(*) FROM binding WHERE principal_id = ? AND state = 'live'", principal); live != 1 {
		t.Fatal("a replay departed the returning member")
	}
	before = rows()
	f.hook(member, delayedStart)
	f.hook(member, delayedEnd)
	if live := f.count("SELECT COUNT(*) FROM binding WHERE principal_id = ? AND state = 'live'", principal); live != 1 {
		t.Fatal("a delayed pre-return clean end departed the returning member")
	}
	f.hook(member, prompt("pid-1", "late-old-turn", started+7))
	if after := rows(); after != before {
		t.Fatalf("old launch after return changed rows: before=%v after=%v", before, after)
	}

	// A compact captured after the return is not a fresh launch.
	f.hook(member, start("pid-1", "compact", started+8))
	f.hook(member, ended("pid-1", started+9))
	if live := f.count("SELECT COUNT(*) FROM binding WHERE principal_id = ? AND state = 'live'", principal); live != 1 {
		t.Fatal("compaction re-established an old launch that departed the returning member")
	}
	if after := rows(); after != before {
		t.Fatalf("old compact or clean end changed rows: before=%v after=%v", before, after)
	}

	f.hook(member, start("pid-3", "startup", started+10))
	if refreshed := f.hook(member, start("pid-3", "compact", started+11)); refreshed.Enrollment != "restored" {
		t.Fatalf("fresh launch could not compact: %+v", refreshed)
	}
	f.hook(member, prompt("pid-3", "new-turn", started+12))
	if after := rows(); after[0] != before[0]+1 || after[1] != before[1]+1 {
		t.Fatalf("fresh launch after return was not observed: before=%v after=%v", before, after)
	}
}
