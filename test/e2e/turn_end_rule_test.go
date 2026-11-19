//go:build e2e

package e2e

import (
	"errors"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// unreadOfKind returns the IDs of unread inbox items of one kind.
func unreadOfKind(t *testing.T, o outcome, kind string) []int64 {
	t.Helper()
	items, _ := o.obj["unread"].([]any)
	var ids []int64
	for _, item := range items {
		row, _ := item.(map[string]any)
		if row["kind"] == kind {
			ids = append(ids, int64(row["id"].(float64)))
		}
	}
	return ids
}

func TestMemberResultsGuideShellOnlyMember(t *testing.T) {
	w := newWorld(t)
	owner := w.owner()
	owner.must("owner", "start")
	owner.must("owner", "channel", "create", "checkout")
	form, tax := w.member(), w.member()
	form.must("join", "checkout", "--as", "form")
	tax.must("join", "checkout", "--as", "tax")
	grantRoots(t, w, "form", 1)

	require := func(output, text string) {
		t.Helper()
		if !strings.Contains(output, text) {
			t.Errorf("missing %q in %q", text, output)
		}
	}
	const wait = "crosshall wait --existing --work-only --timeout 30s"
	require(tax.mustPlain("inbox"), wait)
	cmd := exec.Command(binary, "wait", "--existing", "--work-only", "--timeout", "350ms")
	cmd.Dir, cmd.Env = w.home, tax.env()
	out, err := cmd.CombinedOutput()
	var failed *exec.ExitError
	if !errors.As(err, &failed) || failed.ExitCode() != 6 {
		t.Fatalf("expected wait timeout, got %v: %s", err, out)
	}
	require(string(out), "crosshall inbox")

	request := form.must("ask", "@tax", "Check the totals.")
	id := strconv.FormatInt(num(t, request, "id"), 10)
	form.must("note", "@tax", "Totals are in column A.")
	const unread = "[crosshall] 2 unread, 0 owed. Run crosshall inbox."
	require(tax.mustPlain("whoami"), unread)
	require(tax.mustPlain("wait", "--existing", "--work-only", "--timeout", "30s"), unread)
	require(tax.mustPlain("inbox"), "#"+id+" ask from @form")
	read := tax.mustPlain("read", id)
	require(read, "due ")
	require(read, "crosshall reply "+id)
	require(read, "--more-time")
	require(read, "--decline")
	const owed = "[crosshall] 1 unread, 1 owed. Run crosshall inbox."
	require(read, owed)
	require(tax.mustPlain("inbox"), "#"+id+" ask from @form, due ")
	report := tax.must("status")
	if report.obj["owed_count"] != float64(1) || len(ids(report.obj["pending"])) != 1 {
		t.Errorf("wrong member inbox summary: %s", report.stdout)
	}
	refused := tax.run("read", "99999")
	if refused.exit != 3 || refused.obj["owed_count"] != float64(1) || len(ids(refused.obj["pending"])) != 1 {
		t.Errorf("refusal lost its member inbox summary: %+v", refused)
	}
	cmd = exec.Command(binary, "read", "99999")
	cmd.Dir, cmd.Env = w.home, tax.env()
	out, err = cmd.CombinedOutput()
	if !errors.As(err, &failed) || failed.ExitCode() != 3 || strings.Count(string(out), "crosshall inbox") != 1 {
		t.Errorf("refusal should direct the member to the inbox once: %v, %s", err, out)
	}
	tax.must("reply", id, "--more-time")
	require(tax.mustPlain("status"), owed)
	require(tax.mustPlain("reply", id, "The totals match."), "[crosshall] 1 unread, 0 owed. Run crosshall inbox.")
	if out := tax.mustPlain("read", id); strings.Contains(out, "crosshall reply "+id) {
		t.Fatalf("settled request still asks for a reply: %s", out)
	}
}

// The turn-end rule at the protocol level: a member with a shell and no
// adapter learns the rule from join and follows it with crosshall inbox. A
// request that arrived during its turn is listed as an unread request before
// it stops; a note, a reply to its own request and a roster change that
// arrived at the same time are not requests; reading the request clears it.
func TestTurnEndRuleShellOnlyMember(t *testing.T) {
	w := newWorld(t)
	owner := w.owner()
	owner.must("owner", "start")
	owner.must("owner", "channel", "create", "checkout")
	form, tax := w.member(), w.member()
	form.must("join", "checkout", "--as", "form")
	joined := tax.mustPlain("join", "checkout", "--as", "tax")
	const rule = "Before you end a turn by your own choice, run `crosshall inbox` and read each request addressed to you that it lists. What arrives after that check belongs to your next turn."
	if !strings.Contains(joined, rule) {
		t.Fatalf("join does not state the turn-end rule:\n%s", joined)
	}
	grantRoots(t, w, "tax", 1)

	// tax's turn: it asks form something of its own.
	own := num(t, tax.must("ask", "@form", "Which rounding mode?"), "id")

	// During that turn: a reply to tax's own request, a note, a roster
	// change and one request addressed to tax.
	form.must("reply", strconv.FormatInt(own, 10), "Half-even.")
	form.must("note", "@tax", "FYI: the copy changed.")
	w.member().must("join", "checkout", "--as", "lint")
	request := num(t, form.must("ask", "@tax", "Please check the totals."), "id")

	// Before it stops, tax follows the rule.
	before := tax.must("inbox")
	if got := unreadOfKind(t, before, "ask"); !slices.Equal(got, []int64{request}) {
		t.Fatalf("unread requests before the turn ends: %v, want [%d]: %s", got, request, before.stdout)
	}
	if len(unreadOfKind(t, before, "note")) != 1 || len(unreadOfKind(t, before, "reply")) != 1 {
		t.Fatalf("the note and the reply should be unread but not requests: %s", before.stdout)
	}
	plain := tax.mustPlain("inbox")
	if !strings.Contains(plain, "#"+strconv.FormatInt(request, 10)+" ask from @form") {
		t.Fatalf("plain inbox does not show the request as an ask:\n%s", plain)
	}

	// Reading the request satisfies the rule: no unread request is left, and
	// the request stays owed until tax answers it.
	tax.must("read", strconv.FormatInt(request, 10))
	after := tax.must("inbox")
	if got := unreadOfKind(t, after, "ask"); len(got) != 0 {
		t.Fatalf("a read request is still unread: %v", got)
	}
	owed, _ := after.obj["owed"].([]any)
	if len(owed) != 1 || int64(owed[0].(map[string]any)["id"].(float64)) != request {
		t.Fatalf("the read request should stay owed: %s", after.stdout)
	}
}
