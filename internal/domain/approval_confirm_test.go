package domain

import (
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/publish"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

// A Codex approval request becomes an open approval only when its server
// confirms the thread waits on its owner: the sender then reads
// awaiting_approval. It is asked once it has stood a moment, again only
// near its cap, and not after the turn ends.
func TestCodexApprovalIsOpenOnceItsServerConfirmsIt(t *testing.T) {
	f, writer := adapterChannel(t)
	f.policy("codex", true, 1)
	s := f.newSession("codex")
	at := f.now.UnixMilli()
	f.hook(s, codexStart("startup", publish.ModeDaemon, at))
	alias := decode[JoinResult](t, f.run(s, Join{Channel: "checkout"}, "")).Alias
	approval := event(SessionLaunch, FactApprovalRequested, "t1:approval", "t1", at+1, false)
	approval.Profile = codexProfile(publish.ModeDaemon)
	f.hook(s, approval)
	principal := f.principalOf(s)
	checks := func(now time.Time) []ApprovalCheck {
		t.Helper()
		var got []ApprovalCheck
		f.inTx(func(tx store.Tx) (err error) {
			got, err = ApprovalChecks(t.Context(), tx, now)
			return err
		})
		return got
	}
	if got := checks(f.now.Add(time.Second)); len(got) != 0 {
		t.Fatalf("asked about a request that has not stood: %+v", got)
	}
	got := checks(f.now.Add(3 * time.Second))
	if len(got) != 1 || got[0].Principal != principal || got[0].Target.Adapter != "codex" || got[0].Target.Address != s.key ||
		got[0].RouteHome != codexHome || got[0].Target.Queue != (PublishQueue{PID: codexServer.PID, StartMS: codexServer.Start}) {
		t.Fatalf("checks %+v", got)
	}
	confirmedAt := f.now.Add(3 * time.Second)
	var stored bool
	f.inTx(func(tx store.Tx) (err error) {
		stored, err = ConfirmApproval(t.Context(), tx, principal, got[0].FactSeq, confirmedAt)
		return err
	})
	if !stored {
		t.Fatal("confirmation not stored")
	}
	fact, err := latestFact(t.Context(), f.db, principal)
	if err != nil || fact == nil || !fact.ApprovalOpen {
		t.Fatalf("confirmed fact %+v, %v", fact, err)
	}
	send := decode[AskResult](t, f.ask(writer, Ask{To: []string{alias}, New: true, Body: "work"}, "codex")).Recipients[0]
	if send.Wake.Why != WhyApproval {
		t.Fatalf("send to a confirmed approval: %+v", send.Wake)
	}
	// A stale seq stores nothing.
	f.inTx(func(tx store.Tx) (err error) {
		stored, err = ConfirmApproval(t.Context(), tx, principal, got[0].FactSeq, confirmedAt)
		return err
	})
	if stored {
		t.Fatal("confirmed a fact that is no longer the latest")
	}
	if got := checks(confirmedAt.Add(time.Minute)); len(got) != 0 {
		t.Fatalf("asked again far from the cap: %+v", got)
	}
	if got := checks(confirmedAt.Add(26 * time.Minute)); len(got) != 1 {
		t.Fatalf("not asked again near the cap: %+v", got)
	}
	f.hook(s, func() HookEvent {
		e := stop(SessionLaunch, "t1", at+2)
		e.Profile = codexProfile(publish.ModeDaemon)
		return e
	}())
	if got := checks(confirmedAt.Add(26 * time.Minute)); len(got) != 0 {
		t.Fatalf("asked after the turn ended: %+v", got)
	}
	if fact, _ := latestFact(t.Context(), f.db, principal); fact == nil || fact.ApprovalOpen {
		t.Fatalf("after Stop: %+v", fact)
	}
}
