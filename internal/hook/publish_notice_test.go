package hook

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/publish"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

const fixtureNoticeSuffix = ". This notice is from crosshall, not from a Claude session: do not reply to the sender. Run crosshall read."

func TestPublishNoticeReceipt(t *testing.T) {
	notice := func(ids string) string { return "[crosshall] pending: " + ids + fixtureNoticeSuffix }
	quoted := func(text string) string {
		raw, err := json.Marshal(text)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	exact := quoted(notice("12 15"))
	for _, tc := range []struct {
		name, vendor, promptFields, want string
	}{
		{"exact typed notice", "claude", `,"prompt":` + exact, "publish:12 15"},
		{"one ID", "claude", `,"prompt":` + quoted(notice("9223372036854775807")), "publish:9223372036854775807"},
		{"at cap", "claude", `,"prompt":` + quoted(notice("1 2 3 4 5 6 7 8 9 10")), "publish:1 2 3 4 5 6 7 8 9 10"},
		{"another adapter", "codex", `,"prompt":` + exact, ""},
		{"missing prompt", "claude", "", ""},
		{"non-string prompt", "claude", `,"prompt":{"text":` + exact + `}`, ""},
		{"duplicate prompt key", "claude", `,"prompt":` + exact + `,"prompt":` + exact, ""},
		{"plain turn-end line", "claude", `,"prompt":` + quoted("[crosshall] pending: 12 15"), ""},
		{"wrong prefix", "claude", `,"prompt":` + quoted(" "+notice("12 15")), ""},
		{"extra prose", "claude", `,"prompt":` + quoted(notice("12 15")+" private-fixture-marker"), ""},
		{"altered suffix", "claude", `,"prompt":` + quoted(strings.Replace(notice("12 15"), "Claude session", "Codex session", 1)), ""},
		{"newline", "claude", `,"prompt":` + quoted(notice("12 15")+"\n"), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertPublishHookRequest(t, tc.vendor, tc.promptFields, tc.want)
		})
	}
	for _, ids := range []string{"", "0", "-1", "+1", "01", "12 12", "15 12", "12  15", "12\t15", "12\n15",
		" 12", "12 ", "１２", "9223372036854775808", "1 2 3 4 5 6 7 8 9 10 11"} {
		t.Run("bad IDs "+ids, func(t *testing.T) {
			assertPublishHookRequest(t, "claude", `,"prompt":`+quoted(notice(ids)), "")
		})
	}
}

func assertPublishHookRequest(t *testing.T, vendor, promptFields, want string) {
	t.Helper()
	const metadata = `{"hook_event_name":"UserPromptSubmit","session_id":"fixture","prompt_id":"turn","turn_id":"turn"`
	m, _ := loadManifest(vendor)
	spec, _ := m.event("UserPromptSubmit")
	observed := func(fields string) wire.Request {
		e, err := parseFixture(vendor, spec.Name, metadata+fields+"}", nil)
		if err != nil {
			t.Fatal(err)
		}
		return makeRequest(time.UnixMilli(1024), m, spec, e, "", true, 7)
	}
	baseline := observed("")
	request := observed(promptFields)
	packet, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{`"prompt":`, "[crosshall] pending:", "This notice is from", "private-fixture-marker"} {
		if strings.Contains(string(packet), private) {
			t.Fatalf("prompt text reached the stored request: %s", packet)
		}
	}
	var args wire.HookEventArgs
	if err := json.Unmarshal(request.Args, &args); err != nil || args.Receipt != want {
		t.Fatalf("receipt = %q, want %q (%v)", args.Receipt, want, err)
	}
	args.Receipt = ""
	withoutReceipt, err := json.Marshal(args)
	if err != nil || request.OpKey != baseline.OpKey || request.Verb != baseline.Verb ||
		string(withoutReceipt) != string(baseline.Args) {
		t.Fatalf("receipt changed ordinary observation: %s (%v)", packet, err)
	}
}

// echoFixture recognizes "fixture <ids>"; it stands in for any adapter.
type echoFixture struct{ publish.Adapter }

func (echoFixture) Echo(prompt string) (string, bool) { return strings.CutPrefix(prompt, "fixture ") }

// The hook asks the route's adapter to recognize its own notice and keeps
// the ID validation shared: no adapter is named here.
func TestPublishNoticeReceiptIsTheAdaptersToRecognize(t *testing.T) {
	codex, _ := publish.Lookup("codex")
	t.Cleanup(publish.Swap(publish.Shipped().With("codex", echoFixture{codex})))
	for prompt, want := range map[string]string{
		"fixture 12 15": "publish:12 15",
		"fixture 15 12": "",
		"fixture 12 x":  "",
		"[crosshall] pending: 12 15" + fixtureNoticeSuffix: "",
	} {
		raw, _ := json.Marshal(prompt)
		assertPublishHookRequest(t, "codex", `,"prompt":`+string(raw), want)
	}
}
