package hook

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

type shortJoinHintWriter struct{}

func (shortJoinHintWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }

func TestHookJoinHintReceiptRequiresAFullWriteAndSuppressesOnlyGuidance(t *testing.T) {
	rv, _, leafPath := runFixture(t)
	leaf, err := ident.OpenPrivateDir(leafPath)
	if err != nil {
		t.Fatal(err)
	}
	defer leaf.Close()
	m, _ := loadManifest("codex")
	spec, _ := m.event("UserPromptSubmit")
	ack := wire.HookJoinHintAck{Generation: "fixture-gen", BindingID: 5, RouteEpoch: 1, RosterSeq: 2, Offer: "offer-1"}
	result := wire.HookResult{
		Epoch: 7, Hint: "fixture member rules", PendingLine: "[crosshall] pending: 12", JoinHintAck: &ack,
	}
	projected, err := json.Marshal(ack)
	if err != nil {
		t.Fatal(err)
	}
	if err := leaf.WriteFile(JoinHintOfferFile, projected, 0600); err != nil {
		t.Fatal(err)
	}
	write := func(w io.Writer) (string, bool) {
		return writeHookResultAndConfirmJoin(t.Context(), rv, leaf, "fixture-gen", "codex", 7, spec, result, w)
	}
	if outcome, seen := write(shortJoinHintWriter{}); outcome != "refused" || seen {
		t.Fatalf("short write was acknowledged: %q %t", outcome, seen)
	}
	if _, ok := ReadJoinHintAck(leaf, "fixture-gen"); ok {
		t.Fatal("short write persisted a receipt")
	}
	lock, err := spool.OpenServerFile(leaf, joinHintLock, os.O_CREATE|os.O_RDWR)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	if outcome, seen := write(&output); outcome != "" || seen || strings.Contains(output.String(), result.Hint) ||
		!strings.Contains(output.String(), result.PendingLine) {
		t.Fatalf("concurrent output lost notice or printed a second guide: %q %t %q", outcome, seen, output.String())
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if outcome, seen := write(&output); outcome != "" || !seen || !strings.Contains(output.String(), result.Hint) {
		t.Fatalf("full write did not acknowledge guidance: %q %t %q", outcome, seen, output.String())
	}
	first := ack
	ack.RosterSeq++
	output.Reset()
	if outcome, seen := write(&output); outcome != "" || !seen || strings.Contains(output.String(), result.Hint) ||
		!strings.Contains(output.String(), result.PendingLine) {
		t.Fatalf("repeat suppressed a notice or reprinted guidance: %q %t %q", outcome, seen, output.String())
	}
	if got, ok := ReadJoinHintAck(leaf, "fixture-gen"); !ok || got != first {
		t.Fatalf("retry acknowledged content it did not deliver: %+v %t", got, ok)
	}
	if _, ok := ReadJoinHintAck(leaf, "replacement-gen"); ok {
		t.Fatal("old generation carried the receipt into a replacement exchange")
	}
}

func TestRevokedJoinHintDoesNotReachStdoutOrCreateALock(t *testing.T) {
	rv, _, leafPath := runFixture(t)
	leaf, err := ident.OpenPrivateDir(leafPath)
	if err != nil {
		t.Fatal(err)
	}
	defer leaf.Close()
	m, _ := loadManifest("codex")
	spec, _ := m.event("UserPromptSubmit")
	stale := wire.HookJoinHintAck{Generation: "fixture-gen", BindingID: 5, RouteEpoch: 1, RosterSeq: 2, Offer: "revoked"}
	result := wire.HookResult{Epoch: 7, Hint: "fixture member rules", PendingLine: "[crosshall] pending: 12", JoinHintAck: &stale}
	var output strings.Builder
	failure, seen := writeHookResultAndConfirmJoin(t.Context(), rv, leaf, stale.Generation, "codex", 7, spec, result, &output)
	if failure != "" || seen || strings.Contains(output.String(), result.Hint) || !strings.Contains(output.String(), result.PendingLine) {
		t.Fatalf("revoked guide reached stdout or hid unrelated output: %q %t %q", failure, seen, output.String())
	}
	if _, err := leaf.Lstat(joinHintLock); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("revoked response left an output lock: %v", err)
	}
}
