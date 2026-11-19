//go:build e2e

package e2e

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

type departureEvidence struct {
	Fact   string `json:"fact"`
	Source string `json:"source"`
	AtMS   int64  `json:"at_ms"`
	AgeS   int64  `json:"age_s"`
}

type departureBinding struct {
	ID             int64              `json:"binding_id"`
	Alias          string             `json:"alias"`
	State          string             `json:"binding_state"`
	Token          string             `json:"depart_token"`
	Evidence       *departureEvidence `json:"last_evidence"`
	EvidenceStale  bool               `json:"evidence_stale"`
	OpenRequestIDs []int64            `json:"open_request_ids"`
}

func findDepartureBinding(t *testing.T, owner *participant, matches func(departureBinding) bool) departureBinding {
	t.Helper()
	list := owner.must("owner", "who", "--all")
	var roster struct {
		Channels []struct {
			Name    string             `json:"name"`
			Members []departureBinding `json:"members"`
		} `json:"channels"`
	}
	if err := json.Unmarshal([]byte(list.stdout), &roster); err != nil {
		t.Fatal(err)
	}
	for _, channel := range roster.Channels {
		if channel.Name != "checkout" {
			continue
		}
		for _, row := range channel.Members {
			if matches(row) {
				return row
			}
		}
	}
	t.Fatalf("owner who omitted the selected binding: %s", list.stdout)
	return departureBinding{}
}

var (
	departureTokenPattern = regexp.MustCompile(`^[0-9a-f]{24}$`)
	returnAliasPattern    = regexp.MustCompile(`^route[1-9][0-9]*$`)
)

func liveDepartureBinding(t *testing.T, owner *participant, alias string) departureBinding {
	t.Helper()
	row := findDepartureBinding(t, owner, func(row departureBinding) bool {
		return row.Alias == alias && row.State == "live"
	})
	if row.ID <= 0 || !departureTokenPattern.MatchString(row.Token) {
		t.Fatalf("owner who returned no actionable selection for @%s: %+v", alias, row)
	}
	return row
}

func departureBindingByID(t *testing.T, owner *participant, id int64) departureBinding {
	t.Helper()
	return findDepartureBinding(t, owner, func(row departureBinding) bool { return row.ID == id })
}

func latestCommandStamp(t *testing.T, db *sql.DB, binding int64) (seq, atMS int64) {
	t.Helper()
	err := db.QueryRow(`SELECT o.seq, o.observed_at_ms FROM observation o JOIN binding b
		ON b.principal_id = o.principal_id WHERE b.id = ? AND o.kind = 'command'
		ORDER BY o.seq DESC LIMIT 1`, binding).Scan(&seq, &atMS)
	if err != nil {
		t.Fatal(err)
	}
	return seq, atMS
}

func TestOwnerDepartRejectsInterveningCommandWithSameClock(t *testing.T) {
	w, owner, members := setup(t)
	form, tax := members[0], members[1]
	tax.must("who")
	selected := liveDepartureBinding(t, owner, "tax")
	if row := selected.Evidence; row == nil || row.Fact != "command" || row.Source != "cli" || row.AtMS <= 0 {
		t.Fatalf("owner who did not expose the selected command evidence: %+v", row)
	}
	binding := strconv.FormatInt(selected.ID, 10)
	if out := owner.run("owner", "depart", binding); out.exit != 2 || !strings.Contains(out.stdout, "owner who --all") {
		t.Fatalf("depart without --seen did not explain how to select a binding: exit %d, %s", out.exit, out.stdout)
	}
	if out := form.run("owner", "depart", binding, "--seen", selected.Token); out.exit != 3 || out.obj["code"] != "owner_only" {
		t.Fatalf("a member attempted owner departure: exit %d, %s", out.exit, out.stdout)
	}

	// Create a real second observation, then use the disposable store to give
	// both observations the same timestamp without relying on scheduler timing.
	path := filepath.Join(w.state, "store.sqlite3")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(2500)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	first, at := latestCommandStamp(t, db, selected.ID)
	if at != selected.Evidence.AtMS {
		t.Fatalf("initial selected evidence %d differs from stored observation %d", selected.Evidence.AtMS, at)
	}
	tax.must("who")
	next, _ := latestCommandStamp(t, db, selected.ID)
	if next <= first {
		t.Fatalf("second command had no observation: %d then %d", first, next)
	}
	if _, err := db.Exec("UPDATE observation SET observed_at_ms = ? WHERE seq = ?", at, next); err != nil {
		t.Fatal(err)
	}
	updated := liveDepartureBinding(t, owner, "tax")
	if updated.Evidence == nil || updated.Evidence.AtMS != at || updated.Token == selected.Token {
		t.Fatalf("new observation did not invalidate the selection at the same clock time: before %+v, after %+v", selected, updated)
	}
	out := owner.run("owner", "depart", binding, "--seen", selected.Token)
	if out.exit != 3 || out.obj["code"] != "binding_changed" {
		t.Fatalf("depart accepted a stale selection: exit %d, %s", out.exit, out.stdout)
	}
	if row := departureBindingByID(t, owner, selected.ID); row.State != "live" || row.Token != updated.Token {
		t.Fatalf("refusal changed the binding: %+v", row)
	}
}

func TestOwnerDepartKeepsOpenWorkForTheReturningKey(t *testing.T) {
	w, owner, members := setup(t)
	form, tax := members[0], members[1]
	grantRoots(t, w, "form", 1)
	grantRoots(t, w, "tax", 2)
	id := num(t, form.must("ask", "@tax", "review the totals before release", "--due", "10m"), "id")
	message := strconv.FormatInt(id, 10)
	tax.must("read", message)
	selected := liveDepartureBinding(t, owner, "tax")
	if selected.Evidence == nil || selected.Evidence.AgeS < 0 || selected.EvidenceStale ||
		!slices.Equal(selected.OpenRequestIDs, []int64{id}) {
		t.Fatalf("owner who did not show recent evidence and the open request: %+v", selected)
	}
	// The running dispatcher uses real time. Restart this fixture only with
	// a 30-minute domain offset to cross the command-evidence cap instantly.
	futureOwner := &participant{w: w, extra: []string{"CROSSHALL_TEST_CLOCK_OFFSET=30m"}}
	w.kill()
	futureOwner.must("owner", "start")
	aged := liveDepartureBinding(t, owner, "tax")
	if !aged.EvidenceStale || aged.Evidence == nil || aged.Evidence.AgeS < 1800 ||
		aged.Evidence.AtMS != selected.Evidence.AtMS || aged.Token != selected.Token {
		t.Fatalf("clock aging should flag the same evidence without changing the live selection: before %+v, after %+v", selected, aged)
	}
	if aliases := form.must("who").aliases(); !slices.Contains(aliases, "tax") {
		t.Fatalf("an aged evidence row automatically retired a live member: %v", aliases)
	}

	before := pendingDepartureRecipient(t, form, id)
	binding := strconv.FormatInt(selected.ID, 10)
	owner.must("--key", "depart-tax", "owner", "depart", binding, "--seen", selected.Token)
	departed := departureBindingByID(t, owner, selected.ID)
	if departed.State != "departed" || departed.Token != "" || !slices.Equal(departed.OpenRequestIDs, []int64{id}) {
		t.Fatalf("owner who did not show the departed binding and its open request: %+v", departed)
	}
	if aliases := form.must("who").aliases(); slices.Contains(aliases, "tax") {
		t.Fatalf("member who still lists the departed binding: %v", aliases)
	}
	if out := tax.run("who"); out.exit == 0 {
		t.Fatalf("the departed key returned without an explicit join: %s", out.stdout)
	}
	after := pendingDepartureRecipient(t, form, id)
	if after["binding_state"] != "departed" || after["picked_up_at_ms"] == nil ||
		after["picked_up_at_ms"] != before["picked_up_at_ms"] || !slices.Equal(stages(after), stages(before)) {
		t.Fatalf("waiting lost the recipient departure or pickup evidence: before %v, after %v", before, after)
	}

	fresh := w.member()
	fresh.must("join", "checkout", "--as", "tax")
	replacement := liveDepartureBinding(t, owner, "tax")
	if replacement.ID == selected.ID || len(replacement.OpenRequestIDs) != 0 {
		t.Fatalf("a new key inherited the original binding or requests: %+v", replacement)
	}
	mail := fresh.must("inbox")
	if len(ids(mail.obj["unread"])) != 0 || len(ids(mail.obj["owed"])) != 0 || departureCredits(t, fresh) != 0 {
		t.Fatalf("a fresh key inherited the old key's mail or credits: %s", mail.stdout)
	}

	// Bare same-key return keeps the original binding, and cannot take the
	// alias that is now occupied by the new key.
	returned := tax.must("join", "checkout")
	alias, _ := returned.obj["alias"].(string)
	if !returnAliasPattern.MatchString(alias) {
		t.Fatalf("bare join did not allocate a generated route alias: %s", returned.stdout)
	}
	returnedRow := liveDepartureBinding(t, owner, alias)
	if returnedRow.ID != selected.ID || !slices.Equal(returnedRow.OpenRequestIDs, []int64{id}) ||
		!slices.Contains(ids(tax.must("inbox").obj["owed"]), id) || departureCredits(t, tax) != 2 {
		t.Fatalf("bare join lost the original binding, open request, or credits: %+v", returnedRow)
	}

	// The retry of the recorded departure cannot act on the returning session.
	owner.must("--key", "depart-tax", "owner", "depart", binding, "--seen", selected.Token)
	returnedRow = liveDepartureBinding(t, owner, alias)
	owner.must("owner", "depart", binding, "--seen", returnedRow.Token)
	w.member().must("join", "checkout", "--as", alias)
	named := tax.must("join", "checkout", "--as", "tax-returned")
	if named.obj["alias"] != "tax-returned" || liveDepartureBinding(t, owner, "tax-returned").ID != selected.ID {
		t.Fatalf("explicit alternative did not return the same binding: %s", named.stdout)
	}
	tax.must("reply", message, "verified and ready")
}

func pendingDepartureRecipient(t *testing.T, asker *participant, id int64) map[string]any {
	t.Helper()
	req := soleLedgerRow(t, asker.must("waiting").obj, "requests")
	row := soleLedgerRow(t, req, "recipients")
	if req["id"] != float64(id) || row["state"] != "open" {
		t.Fatalf("waiting lost open request #%d: %v", id, req)
	}
	return row
}

func departureCredits(t *testing.T, p *participant) int64 {
	t.Helper()
	status := p.must("status")
	self, ok := status.obj["self"].(map[string]any)
	if !ok {
		t.Fatalf("status omitted the member credit summary: %s", status.stdout)
	}
	return num(t, outcome{obj: self}, "root_tokens")
}
