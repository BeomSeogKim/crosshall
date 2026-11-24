package domain

import (
	"context"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

func wantOffer(t *testing.T, got wire.HookResult, ids ...int64) wire.HookOfferReceipt {
	t.Helper()
	if got.PendingLine != pendingLine(ids...) || got.OfferReceipt == nil || got.OfferReceipt.Nonce == "" ||
		got.OfferID == 0 || got.OfferID != got.OfferReceipt.OfferID {
		t.Fatalf("got %+v, want an offer receipt and pending IDs %v", got, ids)
	}
	return *got.OfferReceipt
}

func (f *fixture) confirmOffer(principal int64, gen string, receipt wire.HookOfferReceipt, at int64) {
	f.t.Helper()
	if err := ConfirmHookOffer(f.t.Context(), f.db, principal, gen, receipt, at); err != nil {
		f.t.Fatal(err)
	}
}

func TestUnconfirmedHookOfferRetriesOldestBatch(t *testing.T) {
	f, writer := adapterChannel(t)
	f.offerReceipts = true
	s := f.newSession("claude")
	at := f.now.UnixMilli()
	f.hook(s, start("launch", "startup", at))
	pid := f.principalOf(s)
	ids := f.noteTo(writer, "claude1", OfferCap+1)
	first := f.hook(s, prompt("launch", "lost", at+1))
	receipt := wantOffer(t, first, ids[:OfferCap]...)
	retry := f.hook(s, prompt("launch", "retry", at+2))
	if got := wantOffer(t, retry, ids[:OfferCap]...); got != receipt || first.Ack != nil || retry.Ack != nil {
		t.Fatalf("retry %v with ACK %v, first %v with ACK %v", got, retry.Ack, receipt, first.Ack)
	}
	if n := f.count(`SELECT COUNT(*) FROM offer`); n != 1 {
		t.Fatalf("retry stored %d offers", n)
	}
	f.confirmOffer(pid, "g1", receipt, at+3)
	last := f.hook(s, prompt("launch", "last", at+4))
	lastReceipt := wantOffer(t, last, ids[OfferCap])
	if last.Ack == nil || last.Ack.Kind != "notice" || lastReceipt == receipt {
		t.Fatalf("last batch %+v", last)
	}
	again := f.hook(s, prompt("launch", "last-again", at+5))
	if got := wantOffer(t, again, ids[OfferCap]); got != lastReceipt || again.Ack == nil {
		t.Fatalf("retry of the last batch %+v", again)
	}
	f.confirmOffer(pid, "g1", lastReceipt, at+6)
	f.confirmOffer(pid, "g1", receipt, at+7)
	if n := f.count(`SELECT COUNT(*) FROM offer WHERE id = ? AND written_at_ms = ?`, receipt.OfferID, at+3); n != 1 {
		t.Fatal("reordered duplicate changed the first accepted write")
	}
	if got := f.hook(s, prompt("launch", "done", at+8)); got.PendingLine != "" || got.OfferReceipt != nil || got.Ack == nil {
		t.Fatalf("after both writes %+v", got)
	}
	if n := f.count(`SELECT COUNT(*) FROM recipient WHERE picked_up_at_ms IS NOT NULL`); n != 0 {
		t.Fatalf("confirmation recorded %d pickups", n)
	}
}

func TestHookOfferRechecksEligibilityAndWaiting(t *testing.T) {
	p := wakeFixture(t)
	p.offerReceipts = true
	at := p.now.UnixMilli()
	read := p.turnEndAsk(p.writer, "reader", "read")
	cancel := p.turnEndAsk(p.writer, "reader", "cancel")
	pending := p.turnEndAsk(p.writer, "reader", "pending")
	pid := p.principalOf(p.reader)
	prepared := wantOffer(t, p.hook(p.reader, prompt(publishTestLaunch, "before", at+1)), read, cancel, pending)
	if row := p.waitingRecipient(pending); row.ShownAtMS != nil {
		t.Fatalf("unconfirmed pending line shown at %d", *row.ShownAtMS)
	}
	wantOK(t, p.run(p.reader, Read{IDs: []int64{read}}, "read"))
	wantOK(t, p.owner(Cancel{Channel: "checkout", ID: cancel}, "cancel"))
	p.mutate(`UPDATE binding SET dnd = 1 WHERE principal_id = ?`, pid)
	if got := p.hook(p.reader, prompt(publishTestLaunch, "hold", at+2)); got.OfferReceipt != nil || got.PendingLine != "" || got.Ack != nil {
		t.Fatalf("DND presented the unconfirmed offer: %+v", got)
	}
	p.mutate(`UPDATE binding SET dnd = 0 WHERE principal_id = ?`, pid)
	retried := wantOffer(t, p.hook(p.reader, prompt(publishTestLaunch, "after", at+3)), pending)
	if retried.Nonce == prepared.Nonce || retried.OfferID == prepared.OfferID {
		t.Fatalf("different batches shared a descriptor: %v, %v", prepared, retried)
	}
	p.confirmOffer(pid, "g1", retried, at+5)
	p.confirmOffer(pid, "g1", retried, at+4)
	if row := p.waitingRecipient(pending); row.ShownAtMS == nil || *row.ShownAtMS != at+4 || row.PickedUpAtMS != nil {
		t.Fatalf("confirmed pending request in waiting: %+v", row)
	}
	p.confirmOffer(pid, "g1", prepared, at+2)
	if row := p.waitingRecipient(pending); row.ShownAtMS == nil || *row.ShownAtMS != at+2 {
		t.Fatalf("earlier write imported later: %+v", row)
	}
}

func TestHookOfferCompatibility(t *testing.T) {
	p := wakeFixture(t)
	at := p.now.UnixMilli()
	id := p.turnEndAsk(p.writer, "reader", "old")
	old := p.hook(p.reader, prompt(publishTestLaunch, "old-hook", at+1))
	if old.PendingLine != pendingLine(id) || old.OfferReceipt != nil || old.Ack == nil {
		t.Fatalf("legacy hook result %+v", old)
	}
	if row := p.waitingRecipient(id); row.ShownAtMS == nil || *row.ShownAtMS != at+1 {
		t.Fatalf("legacy hook write counted on commit: %+v", row)
	}
	p.offerReceipts = true
	if got := p.hook(p.reader, prompt(publishTestLaunch, "after-legacy", at+2)); got.PendingLine != "" {
		t.Fatalf("repeated a new legacy offer: %+v", got)
	}
	// The migration marks offers from before receipt negotiation as historical.
	p.mutate(`UPDATE offer SET receipt_mode = 'historical' WHERE id = ?`, old.OfferID)
	if row := p.waitingRecipient(id); row.ShownAtMS != nil {
		t.Fatalf("historical output with unknown write counted as shown: %+v", row)
	}
	p.offerReceipts = false
	if got := p.hook(p.reader, prompt(publishTestLaunch, "legacy-historical", at+3)); got.PendingLine != "" {
		t.Fatalf("legacy hook repeated a historical offer: %+v", got)
	}
	p.offerReceipts = true
	wantOffer(t, p.hook(p.reader, prompt(publishTestLaunch, "upgrade", at+4)), id)

	next := p.turnEndAsk(p.writer, "reader", "mixed")
	wantOffer(t, p.hook(p.reader, prompt(publishTestLaunch, "unconfirmed", at+5)), id, next)
	p.offerReceipts = false
	got := p.hook(p.reader, prompt(publishTestLaunch, "after-unconfirmed", at+6))
	if got.PendingLine != pendingLine(next) || got.OfferReceipt != nil {
		t.Fatalf("unconfirmed batch hid a new ID from a legacy hook: %+v", got)
	}
}

func TestHookOfferReceiptScope(t *testing.T) {
	p := wakeFixture(t)
	p.offerReceipts = true
	at := p.now.UnixMilli()
	pid := p.principalOf(p.reader)
	id := p.turnEndAsk(p.writer, "reader", "work")
	// A different, newer live leaf belongs to the same principal.
	peer := p.newSession("claude")
	p.mutate(`INSERT INTO principal_key (vendor, key, principal_id, created_at_ms) VALUES (?, ?, ?, ?)`,
		peer.vendor, peer.key, pid, at+1)
	p.mutate(`UPDATE leaf SET generation = 'peer', principal_id = ?, created_at_ms = ? WHERE digest = ?`, pid, at+1, peer.digest())
	startup := start(publishTestLaunch, "resume", at+2)
	r := wantOffer(t, p.hook(p.reader, startup), id)
	if n := p.count(`SELECT COUNT(*) FROM offer WHERE id = ? AND generation = 'g1'`, r.OfferID); n != 1 {
		t.Fatal("offer took its generation from the newest leaf")
	}
	if got := wantOffer(t, p.hook(p.reader, prompt(publishTestLaunch, "retry", at+3)), id); got != r {
		t.Fatalf("prompt reissued the start's batch: %v, %v", got, r)
	}
	p.confirmOffer(p.principalOf(p.writer), "g1", r, at+4)
	p.confirmOffer(pid, "peer", r, at+4)
	p.confirmOffer(pid, "g1", wire.HookOfferReceipt{OfferID: r.OfferID, Nonce: "other"}, at+4)
	p.mutate(`UPDATE leaf SET state = 'fenced' WHERE digest = ?`, p.reader.digest())
	p.confirmOffer(pid, "g1", r, at+4)
	p.mutate(`UPDATE leaf SET state = 'live' WHERE digest = ?`, p.reader.digest())
	p.mutate(`UPDATE binding SET route_epoch = route_epoch + 1 WHERE id = ?`, p.binding.ID)
	p.confirmOffer(pid, "g1", r, at+4)
	if row := p.waitingRecipient(id); row.ShownAtMS != nil {
		t.Fatalf("a fenced receipt confirmed the offer: %+v", row)
	}
	newRoute := wantOffer(t, p.hook(p.reader, prompt(publishTestLaunch, "new-route", at+5)), id)
	if newRoute.Nonce == r.Nonce || newRoute.OfferID == r.OfferID {
		t.Fatalf("descriptor reused across routes: %v and %v", r, newRoute)
	}
	p.mutate(`DELETE FROM offer_item WHERE offer_id = ?`, newRoute.OfferID)
	p.mutate(`DELETE FROM offer WHERE id = ?`, newRoute.OfferID)
	reusedID := wantOffer(t, p.hook(p.reader, prompt(publishTestLaunch, "reused-id", at+6)), id)
	if reusedID.OfferID != newRoute.OfferID || reusedID.Nonce == newRoute.Nonce {
		t.Fatalf("expected SQLite ID reuse and a new nonce: %v, %v", newRoute, reusedID)
	}
	p.confirmOffer(pid, "g1", newRoute, at+7)
	if row := p.waitingRecipient(id); row.ShownAtMS != nil {
		t.Fatalf("old nonce confirmed a reused ID: %+v", row)
	}
	otherLaunch := start("other-launch", "startup", at+8)
	newLaunch := wantOffer(t, p.hook(p.reader, otherLaunch), id)
	p.policy("claude", true, 2)
	newPolicy := prompt("other-launch", "new-policy", at+9)
	newPolicy.Epoch = 2
	newEpoch := wantOffer(t, p.hook(p.reader, newPolicy), id)
	if newLaunch.Nonce == reusedID.Nonce || newEpoch.Nonce == newLaunch.Nonce {
		t.Fatalf("shared descriptor across launches or policy epochs: %v, %v, %v", reusedID, newLaunch, newEpoch)
	}
}

func TestHookOfferReceiptLeavesTurnEndClaimAlone(t *testing.T) {
	f, reader, writer := turnEndFixture(t, "launch")
	f.offerReceipts = true
	wantOK(t, f.owner(turnEndEvidence("tested"), "evidence"))
	at := f.now.UnixMilli()
	id := f.turnEndAsk(writer, "reader", "turn-end")
	r := wantOffer(t, f.hook(reader, prompt("launch", "work", at+1)), id)
	got := f.hook(reader, stop("launch", "work", at+2))
	if got.TurnEndLine != pendingLine(id) || got.OfferReceipt != nil {
		t.Fatalf("Stop with an unconfirmed prompt: %+v", got)
	}
	f.confirmOffer(f.principalOf(reader), "g1", r, at+3)
	again := event("launch", FactTurnFinishing, "distinct-stop", "work", at+4, true)
	if got := f.hook(reader, again); got.TurnEndLine != "" || got.OfferReceipt != nil {
		t.Fatalf("receipt replayed Stop: %+v", got)
	}
}

func TestHookOfferReceiptLeavesPublishObservationAlone(t *testing.T) {
	p := newPublishFixture(t)
	p.offerReceipts = true
	p.proveRoute()
	p.finishPreviousTurn()
	id := p.turnEndAsk(p.writer, "reader", "work")
	attempt := p.reserve()
	if ids := p.begin(attempt); len(ids) != 1 || ids[0] != id {
		t.Fatalf("publish attempt carried %v", ids)
	}
	at := p.now.UnixMilli()
	r := wantOffer(t, p.hook(p.reader, prompt(publishTestLaunch, "user", at+1)), id)
	p.confirmOffer(p.principalOf(p.reader), "g1", r, at+2)
	p.observation(attempt, "")
	observed := p.promptReceipt("notice", publishTestLaunch, at+3, []int64{id})
	if got := p.hook(p.reader, observed); got.PendingLine != "" || got.OfferReceipt != nil {
		t.Fatalf("publish notice repeated the batch: %+v", got)
	}
	p.observation(attempt, "own_turn")
}

func TestRecoveryEnrollmentOffersPendingLineWithReceipt(t *testing.T) {
	f, _, writer := turnEndFixture(t, "existing")
	f.offerReceipts = true
	id := f.noteTo(writer, "reader", 1)[0]
	s := f.newSession("claude")
	cmd := recoveryPrompt("launch", "first", f.now.UnixMilli()+1)
	req := Request{Command: cmd, OpKey: hookKey(cmd), Vendor: s.vendor, Adapter: &AdapterContext{Generation: "g1", OfferReceipts: true}}
	// Arrange pending mail inside first enrollment, when the principal ID becomes known.
	req.Enroll = func(ctx context.Context, tx store.Tx, principal int64) error {
		if err := enroll(s)(ctx, tx, principal); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO recipient (principal_id, message_id, alias_at_commit, owes)
			VALUES (?, ?, 'claude1', 0)`, principal, id)
		return err
	}
	result := f.exec(f.caller(s), req)
	wantOK(t, result)
	got := decode[wire.HookResult](t, result)
	r := wantOffer(t, got, id)
	if got.Enrollment != "joined" {
		t.Fatalf("recovery did not enroll: %+v", got)
	}
	retry := f.hook(s, prompt("launch", "retry", cmd.HookAtMS+1))
	if got := wantOffer(t, retry, id); got != r {
		t.Fatalf("recovered prompt issued a fresh receipt: %v, %v", got, r)
	}
}

func TestHookOfferWithSameFirstIDButDifferentBatch(t *testing.T) {
	f, reader, writer := turnEndFixture(t, "launch")
	f.offerReceipts = true
	at := f.now.UnixMilli()
	ids := f.noteTo(writer, "reader", 2)
	first := wantOffer(t, f.hook(reader, prompt("launch", "before", at+1)), ids...)
	wantOK(t, f.run(reader, Read{IDs: ids[1:]}, "pickup"))
	added := f.noteTo(writer, "reader", 1)[0]
	next := wantOffer(t, f.hook(reader, prompt("launch", "after", at+2)), ids[0], added)
	if first.Nonce == next.Nonce || first.OfferID == next.OfferID {
		t.Fatalf("different two-item batches shared a descriptor: %v, %v", first, next)
	}
	f.confirmOffer(f.principalOf(reader), "g1", first, at+3)
	wantOffer(t, f.hook(reader, prompt("launch", "delayed", at+4)), added)
}
