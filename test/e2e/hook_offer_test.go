//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/domain"
)

func TestInstalledPromptRetriesOffersAfterOutputDeadline(t *testing.T) {
	f := newSetupWorld(t)
	f.apply("", "setup", "codex", "--apply")
	repo := f.enrollmentRepo()
	const event = "UserPromptSubmit"
	f.waitForWelcomeDelivery("codex", "SessionStart", time.Now().Add(fixtureEventTimeout), func(string) string {
		return f.invokeHook(repo, "codex", "SessionStart", `"source":"startup"`)
	})
	f.waitEnrollmentPackets("codex", "SessionStart", time.Now().Add(fixtureEventTimeout))
	writer := f.member()
	writer.must("join", "setup-check", "--as", "writer")
	var messageIDs []string
	for _, body := range []string{"first request", "second request"} {
		id := num(t, writer.must("ask", "@codex1", body), "id")
		messageIDs = append(messageIDs, strconv.FormatInt(id, 10))
	}
	waiting := func(shown, pickedUp bool) bool {
		t.Helper()
		var result domain.WaitingResult
		if err := json.Unmarshal([]byte(writer.must("waiting").stdout), &result); err != nil {
			t.Fatal(err)
		}
		if len(result.Requests) != len(messageIDs) {
			t.Fatalf("waiting requests: %+v", result.Requests)
		}
		for _, request := range result.Requests {
			if len(request.Recipients) != 1 || request.Recipients[0].Alias != "codex1" {
				t.Fatalf("unexpected recipients for request %d: %+v", request.ID, request.Recipients)
			}
			r := request.Recipients[0]
			if (r.ShownAtMS != nil) != shown || (r.PickedUpAtMS != nil) != pickedUp {
				return false
			}
		}
		return true
	}
	db := f.db()
	deadline := time.Now().Add(fixtureEventTimeout)
	for attempt := 1; ; attempt++ {
		f.blockHookOutput(repo, "codex", event, fmt.Sprintf(`"turn_id":"blocked-%d"`, attempt))
		f.waitEnrollmentPackets("codex", event, deadline)
		var committed int
		if err := db.QueryRow("SELECT COUNT(DISTINCT message_id) FROM offer_item WHERE message_id IN (?, ?)", messageIDs[0], messageIDs[1]).Scan(&committed); err != nil {
			t.Fatal(err)
		}
		if committed == len(messageIDs) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("blocked stdout returned no bytes, but only %d of %v had committed offers", committed, messageIDs)
		}
	}
	if !waiting(false, false) {
		t.Errorf("waiting reported shown or picked up after the blocked output: %s", writer.must("waiting").stdout)
	}
	prompt := func(turn string) (string, string) {
		t.Helper()
		before, err := f.hookCounterRecords("codex", event)
		if err != nil {
			t.Fatal(err)
		}
		out := f.invokeHook(repo, "codex", event, fmt.Sprintf(`"turn_id":%q`, turn))
		f.waitEnrollmentPackets("codex", event, time.Now().Add(fixtureEventTimeout))
		after, err := f.hookCounterRecords("codex", event)
		if err != nil {
			t.Fatal(err)
		}
		outcome := ""
		if len(after) > len(before) {
			outcome = strings.Fields(after[len(after)-1])[2]
		}
		return hostContext(t, out), outcome
	}
	want := pendingContext(messageIDs)
	deadline = time.Now().Add(fixtureEventTimeout)
	for attempt := 1; ; attempt++ {
		got, outcome := prompt(fmt.Sprintf("retry-%d", attempt))
		if got != want && (got != "" || outcome == "rpc" || outcome == "clean") {
			t.Fatalf("next prompt after committed offers %v and empty stdout got %q (%s), want %q", messageIDs, got, outcome, want)
		}
		if got == want && waiting(true, false) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no confirmed retry after committed offers %v and empty stdout: last output %q, want %q; last counter %q", messageIDs, got, want, f.hookCounter("codex", event))
		}
	}
	deadline = time.Now().Add(fixtureEventTimeout)
	for attempt := 1; ; attempt++ {
		turn := fmt.Sprintf("after-receipt-%d", attempt)
		if got, _ := prompt(turn); got != "" {
			t.Fatalf("prompt after successful receipt repeated %q", got)
		}
		if hookPromptObserved(t, db, "codex", turn) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no later prompt observed after successful receipt")
		}
	}
	recipient := &participant{w: f.world, extra: []string{"CODEX_THREAD_ID=" + setupSessionPrefix + "codex"}}
	recipient.must("read")
	if !waiting(false, true) {
		t.Fatalf("waiting after reading both requests: %s", writer.must("waiting").stdout)
	}
}
