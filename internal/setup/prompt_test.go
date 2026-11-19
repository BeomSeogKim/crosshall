package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/setuptest"
)

// fakeOps stands in for the operations layer and records what it was asked.
type fakeOps struct {
	statuses  []AdapterStatus
	statusErr error
	plans     map[string]VendorPlan
	planErr   error
	failing   map[string]error
	installed []string
	rules     map[string]ClaudeRules
	removed   []string
	planned   [][]string
}

func newFakeOps() *fakeOps {
	plan := func(adapter, name, path string) VendorPlan {
		vp := VendorPlan{Action: "setup", Adapter: adapter, Name: name, Path: path, PathSource: "default"}
		vp.Files = []PlanFile{{Adapter: adapter, Path: path, Add: []Entry{{Event: "Stop", Value: json.RawMessage(`"crosshall hook ` + adapter + ` Stop"`)}}}}
		if adapter == "claude" {
			vp.Approvals = []ApprovalOffer{{Adapter: "claude", Kind: "read"}, {Adapter: "claude", Kind: "send"}}
		}
		return vp
	}
	return &fakeOps{
		statuses: []AdapterStatus{
			{Adapter: "claude", Name: "Claude Code", HookAdapter: true, CLI: "found", Footprint: "none"},
			{Adapter: "codex", Name: "Codex", HookAdapter: true, CLI: "found", Footprint: "none"},
			{Adapter: "agy", Name: "Antigravity", CLI: "found", Footprint: "unknown"},
		},
		plans: map[string]VendorPlan{
			"claude": plan("claude", "Claude Code", "/fixture/home/.claude/settings.json"),
			"codex":  plan("codex", "Codex", "/fixture/home/.codex/hooks.json"),
		},
		failing: map[string]error{},
		rules:   map[string]ClaudeRules{},
	}
}

func (f *fakeOps) Status(context.Context) ([]AdapterStatus, error) { return f.statuses, f.statusErr }

func (f *fakeOps) PlanInstall(_ context.Context, names []string) ([]VendorPlan, error) {
	f.planned = append(f.planned, names)
	if f.planErr != nil {
		return nil, f.planErr
	}
	var plans []VendorPlan
	for _, n := range names {
		plans = append(plans, f.plans[n])
	}
	return plans, nil
}

func (f *fakeOps) Install(_ context.Context, plan VendorPlan, rules ClaudeRules) error {
	if err := f.failing[plan.Adapter]; err != nil {
		return err
	}
	f.installed = append(f.installed, plan.Adapter)
	f.rules[plan.Adapter] = rules
	return nil
}

func (f *fakeOps) PlanUninstall(_ context.Context, names []string) ([]VendorPlan, error) {
	var plans []VendorPlan
	for _, n := range names {
		vp := f.plans[n]
		vp.Action = "uninstall"
		plans = append(plans, vp)
	}
	return plans, nil
}

func (f *fakeOps) Uninstall(_ context.Context, plan VendorPlan) error {
	f.removed = append(f.removed, plan.Adapter)
	return nil
}

// countingReader fails the test if anything reads it.
type countingReader struct{ reads int }

func (r *countingReader) Read([]byte) (int, error) { r.reads++; return 0, errors.New("input was read") }

type promptRun struct {
	out    string
	err    error
	ops    *fakeOps
	s      Service
	prompt string
}

func runPrompts(t *testing.T, ops *fakeOps, req Request, input string) promptRun {
	t.Helper()
	f := setuptest.New(t)
	f.CheckTargets(t)
	return runPromptsIn(t, Service{StateRoot: f.State}, ops, req, input)
}

func runPromptsIn(t *testing.T, s Service, ops *fakeOps, req Request, input string) promptRun {
	t.Helper()
	var out, prompt bytes.Buffer
	req.Output, req.Prompt = &out, &prompt
	if req.Input == nil {
		req.Input = strings.NewReader(input)
	}
	if req.Action == "" {
		req.Action = "setup"
	}
	err := s.prompts(context.Background(), req, ops)
	return promptRun{out: out.String(), err: err, ops: ops, s: s, prompt: prompt.String()}
}

func declined(t *testing.T, s Service) map[string]bool {
	t.Helper()
	d, err := s.declinedOffers()
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestEachVendorIsAskedAfterItsPlanWithItsPathAndSource(t *testing.T) {
	ops := newFakeOps()
	r := runPrompts(t, ops, Request{Terminal: true}, "yes\nyes\nno\nno\n")
	if r.err != nil {
		t.Fatal(r.err)
	}
	claudeQ := strings.Index(r.out, "Install Claude Code hooks in /fixture/home/.claude/settings.json (default under HOME)? [y/N]")
	claudePlan := strings.Index(r.out, "claude /fixture/home/.claude/settings.json")
	codexQ := strings.Index(r.out, "Install Codex hooks in /fixture/home/.codex/hooks.json (default under HOME)? [y/N]")
	codexPlan := strings.Index(r.out, "codex /fixture/home/.codex/hooks.json")
	if claudePlan < 0 || claudeQ < claudePlan || codexPlan < claudeQ || codexQ < codexPlan {
		t.Fatalf("plans and questions out of order:\n%s", r.out)
	}
	// Claude yes with read rules and no send rules; Codex no.
	if !slices.Equal(ops.installed, []string{"claude"}) || ops.rules["claude"] != (ClaudeRules{Read: true}) {
		t.Fatalf("installed %v rules %+v", ops.installed, ops.rules)
	}
	if d := declined(t, r.s); !d["codex"] || d["claude"] {
		t.Fatalf("declined %v", d)
	}
	if !strings.Contains(r.out, "claude: installed") || !strings.Contains(r.out, "codex: declined") {
		t.Fatalf("outcomes:\n%s", r.out)
	}
}

func TestBothVendorsInSequence(t *testing.T) {
	ops := newFakeOps()
	r := runPrompts(t, ops, Request{Terminal: true}, "y\nn\nn\ny\n")
	if r.err != nil || !slices.Equal(ops.installed, []string{"claude", "codex"}) || ops.rules["claude"] != (ClaudeRules{}) {
		t.Fatalf("installed %v rules %+v: %v", ops.installed, ops.rules, r.err)
	}
	if strings.Contains(r.out, "Add Codex") || !strings.Contains(r.out, "Review and trust these hooks with Codex /hooks.") {
		t.Fatalf("codex questions or trust notice:\n%s", r.out)
	}
}

func TestOnlyTheNamedVendorIsPlannedAndAsked(t *testing.T) {
	ops := newFakeOps()
	r := runPrompts(t, ops, Request{Terminal: true, Adapters: []string{"codex"}}, "y\n")
	if r.err != nil || !slices.Equal(ops.installed, []string{"codex"}) || len(ops.planned) != 1 || !slices.Equal(ops.planned[0], []string{"codex"}) ||
		strings.Contains(r.out, "Claude") {
		t.Fatalf("planned %v installed %v: %v\n%s", ops.planned, ops.installed, r.err, r.out)
	}
}

// Enter, other text and end of input install nothing and remember nothing.
func TestASkippedQuestionChangesNothing(t *testing.T) {
	for name, input := range map[string]string{"enter": "\n\n", "other text": "maybe\nlater\n", "end of input": ""} {
		ops := newFakeOps()
		r := runPrompts(t, ops, Request{Terminal: true}, input)
		if r.err != nil || len(ops.installed) != 0 || len(declined(t, r.s)) != 0 {
			t.Errorf("%s: installed %v declined %v: %v", name, ops.installed, declined(t, r.s), r.err)
		}
	}
}

func TestOverriddenAndChangedPathsAreShownWithTheirSource(t *testing.T) {
	ops := newFakeOps()
	c := ops.plans["claude"]
	c.Path, c.PathSource = "/fixture/claude-dir/settings.json", "override"
	ops.plans["claude"] = c
	x := ops.plans["codex"]
	x.RecordedPath = "/fixture/old-codex/hooks.json"
	ops.plans["codex"] = x
	r := runPrompts(t, ops, Request{Terminal: true}, "\n\n")
	for _, want := range []string{
		"Install Claude Code hooks in /fixture/claude-dir/settings.json (from CLAUDE_CONFIG_DIR)?",
		"Install Codex hooks in /fixture/home/.codex/hooks.json (default under HOME; recorded earlier at /fixture/old-codex/hooks.json)?",
	} {
		if !strings.Contains(r.out, want) {
			t.Errorf("missing %q in:\n%s", want, r.out)
		}
	}
}

// --apply installs the named vendors, reads no input and adds no allow rules.
func TestApplyInstallsTheNamedVendorsWithoutReadingInput(t *testing.T) {
	ops := newFakeOps()
	input := &countingReader{}
	r := runPrompts(t, ops, Request{Apply: true, Adapters: []string{"claude", "codex"}, Input: input}, "")
	if r.err != nil || input.reads != 0 || !slices.Equal(ops.installed, []string{"claude", "codex"}) || ops.rules["claude"] != (ClaudeRules{}) {
		t.Fatalf("reads %d installed %v rules %+v: %v", input.reads, ops.installed, ops.rules, r.err)
	}
	if len(declined(t, r.s)) != 0 {
		t.Fatal("--apply recorded a decision")
	}
}

func TestPreviewsChangeNothing(t *testing.T) {
	for name, req := range map[string]Request{
		"dry run":                {DryRun: true, Adapters: []string{"claude", "codex"}},
		"unnamed dry run":        {DryRun: true, Terminal: true},
		"named without terminal": {Adapters: []string{"claude"}},
	} {
		ops := newFakeOps()
		input := &countingReader{}
		req.Input = input
		r := runPrompts(t, ops, req, "")
		if r.err != nil || len(ops.installed) != 0 || input.reads != 0 || !strings.Contains(r.out, "claude: previewed") {
			t.Errorf("%s: installed %v reads %d: %v\n%s", name, ops.installed, input.reads, r.err, r.out)
		}
	}
	r := runPrompts(t, newFakeOps(), Request{Adapters: []string{"claude"}}, "")
	if !strings.Contains(r.out, "crosshall setup claude --apply") {
		t.Fatalf("named preview without the scripted form:\n%s", r.out)
	}
}

// A failure for one vendor is reported apart and does not stop the others.
func TestOneVendorsFailureLeavesTheOthers(t *testing.T) {
	ops := newFakeOps()
	ops.failing["claude"] = errors.New("config changed since the plan")
	x := ops.plans["codex"]
	r := runPrompts(t, ops, Request{Terminal: true}, "y\nn\nn\ny\n")
	if r.err == nil || !strings.Contains(r.err.Error(), "crosshall setup claude") || !slices.Equal(ops.installed, []string{"codex"}) {
		t.Fatalf("installed %v: %v", ops.installed, r.err)
	}
	if !strings.Contains(r.out, "claude: failed (config changed since the plan); next: crosshall setup claude") || !strings.Contains(r.out, "codex: installed") {
		t.Fatalf("outcomes:\n%s", r.out)
	}
	ops = newFakeOps()
	x.Err = errors.New("CODEX_HOME is not an absolute path")
	ops.plans["codex"] = x
	r = runPrompts(t, ops, Request{Terminal: true}, "y\nn\nn\n")
	if r.err == nil || !slices.Equal(ops.installed, []string{"claude"}) || strings.Contains(r.out, "Install Codex hooks") {
		t.Fatalf("an invalid override for codex: installed %v: %v\n%s", ops.installed, r.err, r.out)
	}
}

func TestAPendingOperationOfAnotherScopeIsShownNotResumed(t *testing.T) {
	ops := newFakeOps()
	ops.planErr = &PendingScopeError{Action: "setup", Adapters: []string{"claude", "codex"}}
	r := runPrompts(t, ops, Request{Terminal: true, Adapters: []string{"codex"}}, "y\n")
	if r.err == nil || !strings.Contains(r.err.Error(), "interrupted setup of claude, codex") ||
		!strings.Contains(r.err.Error(), "crosshall setup claude codex") || len(ops.installed) != 0 {
		t.Fatalf("installed %v: %v", ops.installed, r.err)
	}
}

func TestAnExactResumeAsksOnceAndKeepsTheRecordedRules(t *testing.T) {
	ops := newFakeOps()
	ops.plans["claude"] = VendorPlan{Action: "setup", Adapter: "claude", Name: "Claude Code", Adapters: []string{"claude", "codex"}, Resume: true}
	r := runPrompts(t, ops, Request{Terminal: true, Adapters: []string{"claude"}}, "y\n")
	if r.err != nil || !strings.Contains(r.out, "Resume the interrupted setup of claude, codex? [y/N]") ||
		strings.Contains(r.out, "allow rules?") || !slices.Equal(ops.installed, []string{"claude"}) || len(declined(t, r.s)) != 0 {
		t.Fatalf("installed %v: %v\n%s", ops.installed, r.err, r.out)
	}
}

func TestARecoveryIsReportedAsRecoveredNotInstalled(t *testing.T) {
	ops := newFakeOps()
	ops.plans["claude"] = VendorPlan{Action: "setup", Adapter: "claude", Name: "Claude Code", Adapters: []string{"claude"}, Recovery: true}
	r := runPrompts(t, ops, Request{Terminal: true, Adapters: []string{"claude"}}, "y\n")
	if r.err != nil || strings.Contains(r.out, "installed") || !strings.Contains(r.out, "claude: recovered; next: crosshall setup claude") {
		t.Fatalf("%v\n%s", r.err, r.out)
	}
}

func TestNamedUninstallRemovesOnlyTheConfirmedVendor(t *testing.T) {
	ops := newFakeOps()
	r := runPrompts(t, ops, Request{Action: "uninstall", Adapters: []string{"codex"}}, "yes\n")
	if r.err != nil || !slices.Equal(ops.removed, []string{"codex"}) || !strings.Contains(r.out, "Remove the crosshall hooks for Codex from /fixture/home/.codex/hooks.json? [y/N]") {
		t.Fatalf("removed %v: %v\n%s", ops.removed, r.err, r.out)
	}
	ops = newFakeOps()
	if r := runPrompts(t, ops, Request{Action: "uninstall", Adapters: []string{"codex"}, DryRun: true}, "yes\n"); r.err != nil || len(ops.removed) != 0 {
		t.Fatalf("dry-run removed %v: %v", ops.removed, r.err)
	}
}

func TestJSONKeepsQuestionsOffStandardOutput(t *testing.T) {
	ops := newFakeOps()
	r := runPrompts(t, ops, Request{Terminal: true, JSON: true, Adapters: []string{"codex"}}, "y\n")
	var result promptResult
	if r.err != nil || json.Unmarshal([]byte(r.out), &result) != nil || len(result.Vendors) != 1 || result.Vendors[0].Outcome != "installed" ||
		!strings.Contains(r.prompt, "Install Codex hooks") {
		t.Fatalf("stdout %q prompts %q: %v", r.out, r.prompt, r.err)
	}
}

func TestTheInstallerOfferAsksOnlyWhenEligible(t *testing.T) {
	f := setuptest.New(t)
	f.CheckTargets(t)
	s := Service{StateRoot: f.State}
	offer := Request{Offer: true, Terminal: true}

	for name, change := range map[string]func(*fakeOps){
		"no terminal":       nil,
		"claude present":    func(o *fakeOps) { o.statuses[0].Footprint = "present" },
		"codex unknown":     func(o *fakeOps) { o.statuses[1].Footprint = "unknown" },
		"status unreadable": func(o *fakeOps) { o.statusErr = errors.New("store busy") },
		"no CLI found":      func(o *fakeOps) { o.statuses[0].CLI, o.statuses[1].CLI = "missing", "missing" },
	} {
		ops := newFakeOps()
		req := offer
		if change == nil {
			req.Terminal = false
		} else {
			change(ops)
		}
		r := runPromptsIn(t, s, ops, req, "y\ny\ny\ny\n")
		if r.err != nil || r.out != "" || len(ops.planned) != 0 {
			t.Errorf("%s: asked %v, printed %q: %v", name, ops.planned, r.out, r.err)
		}
	}

	// Eligible: the Antigravity row is information only. Claude is declined.
	ops := newFakeOps()
	r := runPromptsIn(t, s, ops, offer, "\nn\n")
	if r.err != nil || !slices.Equal(ops.planned[0], []string{"claude", "codex"}) || !declined(t, s)["codex"] || declined(t, s)["claude"] {
		t.Fatalf("planned %v declined %v: %v", ops.planned, declined(t, s), r.err)
	}
	// The next offer skips the declined vendor; Enter left Claude available.
	ops = newFakeOps()
	r = runPromptsIn(t, s, ops, offer, "n\n")
	if r.err != nil || len(ops.planned) != 1 || !slices.Equal(ops.planned[0], []string{"claude"}) || !declined(t, s)["claude"] {
		t.Fatalf("second offer planned %v: %v", ops.planned, r.err)
	}
	// Both declined: nothing is asked again.
	ops = newFakeOps()
	if r = runPromptsIn(t, s, ops, offer, "y\n"); r.out != "" || len(ops.planned) != 0 {
		t.Fatalf("a declined vendor was offered again: %v", ops.planned)
	}
	// A remembered no is not consent either way: an explicit setup still
	// asks, and an explicit yes there clears it.
	ops = newFakeOps()
	r = runPromptsIn(t, s, ops, Request{Terminal: true, Adapters: []string{"codex"}}, "y\n")
	if r.err != nil || !slices.Equal(ops.installed, []string{"codex"}) || declined(t, s)["codex"] {
		t.Fatalf("explicit setup after a decline: installed %v declined %v: %v", ops.installed, declined(t, s), r.err)
	}
}

func TestAFailedOfferNamesTheRetry(t *testing.T) {
	ops := newFakeOps()
	ops.failing["codex"] = errors.New("cannot lock adapter installation")
	r := runPrompts(t, ops, Request{Offer: true, Terminal: true}, "\ny\n")
	if r.err == nil || !strings.Contains(r.err.Error(), "crosshall setup codex") {
		t.Fatalf("%v", r.err)
	}
}

func TestAJSONPreviewCarriesEachPlan(t *testing.T) {
	ops := newFakeOps()
	var out bytes.Buffer
	err := Service{}.prompts(context.Background(), Request{Action: "setup", JSON: true, DryRun: true,
		Adapters: []string{"claude", "codex"}, Output: &out, Input: strings.NewReader("")}, ops)
	var result promptResult
	if err != nil || json.Unmarshal(out.Bytes(), &result) != nil || len(result.Vendors) != 2 {
		t.Fatalf("%v: %s", err, out.String())
	}
	for _, v := range result.Vendors {
		if v.Plan == nil || len(v.Plan.Files) != 1 || v.Plan.Path != v.Path {
			t.Errorf("%s preview without its plan: %+v", v.Adapter, v)
		}
	}
	if result.Vendors[0].Plan == nil || len(result.Vendors[0].Plan.Approvals) != 2 {
		t.Error("the Claude preview lacks its rule offers")
	}
}

// --apply over a recovery is a failure that names the
// interactive command, not a success.
func TestApplyOverARecoveryFailsWithTheManualCommand(t *testing.T) {
	ops := newFakeOps()
	ops.plans["claude"] = VendorPlan{Action: "setup", Adapter: "claude", Name: "Claude Code", Adapters: []string{"claude"}, Recovery: true}
	r := runPrompts(t, ops, Request{Apply: true, JSON: true, Adapters: []string{"claude"}}, "")
	var result promptResult
	if r.err == nil || json.Unmarshal([]byte(r.out), &result) != nil || result.OK || len(ops.installed) != 0 ||
		result.Vendors[0].Outcome != "failed" || result.Vendors[0].Retry != "crosshall setup claude" {
		t.Fatalf("%v: %s", r.err, r.out)
	}
}

// A failed uninstall names uninstall, and a legacy
// multi-vendor plan is named by its recorded scope.
func TestFailuresNameTheirActionAndRecordedScope(t *testing.T) {
	ops := &failingUninstall{fakeOps: newFakeOps()}
	var out bytes.Buffer
	err := Service{}.prompts(context.Background(), Request{Action: "uninstall", Adapters: []string{"claude"},
		Output: &out, Input: strings.NewReader("y\n")}, ops)
	if err == nil || strings.Contains(err.Error(), "crosshall setup") || !strings.Contains(err.Error(), "crosshall uninstall claude") ||
		!strings.Contains(out.String(), "next: crosshall uninstall claude") {
		t.Fatalf("%v\n%s", err, out.String())
	}

	legacy := newFakeOps()
	legacy.plans["claude"] = VendorPlan{Action: "uninstall", Adapters: []string{"claude", "codex"}, Resume: true}
	out.Reset()
	err = Service{}.prompts(context.Background(), Request{Action: "uninstall", Adapters: []string{"claude"},
		Output: &out, Input: strings.NewReader("y\n")}, legacy)
	if err != nil || !strings.Contains(out.String(), "Resume the interrupted uninstall of claude, codex? [y/N]") ||
		!strings.Contains(out.String(), "claude,codex: removed") || strings.Contains(out.String(), "from ?") {
		t.Fatalf("%v\n%s", err, out.String())
	}
	failed := newFakeOps()
	failed.plans["claude"] = VendorPlan{Action: "setup", Adapters: []string{"claude", "codex"}, Resume: true}
	failed.failing[""] = errors.New("journal changed")
	out.Reset()
	err = Service{}.prompts(context.Background(), Request{Action: "setup", Terminal: true, Adapters: []string{"claude"},
		Output: &out, Input: strings.NewReader("y\n")}, failed)
	if err == nil || !strings.Contains(err.Error(), "crosshall setup claude codex") || !strings.Contains(out.String(), "claude,codex: failed") {
		t.Fatalf("%v\n%s", err, out.String())
	}
}

type failingUninstall struct{ *fakeOps }

func (f *failingUninstall) Uninstall(context.Context, VendorPlan) error {
	return errors.New("configuration changed since the plan")
}

// Work that became pending after the status read is not
// offered; the installer form stays silent.
func TestTheOfferStaysSilentWhenThePlanTurnsOutPending(t *testing.T) {
	for name, change := range map[string]func(*fakeOps){
		"resume": func(o *fakeOps) {
			o.plans["claude"] = VendorPlan{Action: "setup", Adapter: "claude", Adapters: []string{"claude"}, Resume: true}
		},
		"recovery": func(o *fakeOps) {
			o.plans["codex"] = VendorPlan{Action: "setup", Adapter: "codex", Adapters: []string{"codex"}, Recovery: true}
		},
		"pending scope": func(o *fakeOps) {
			o.planErr = &PendingScopeError{Action: "setup", Adapters: []string{"claude", "codex"}}
		},
		"plan error": func(o *fakeOps) { o.planErr = errors.New("store busy") },
	} {
		ops := newFakeOps()
		change(ops)
		r := runPrompts(t, ops, Request{Offer: true, Terminal: true}, "y\ny\ny\ny\n")
		if r.err != nil || r.out != "" || len(ops.installed) != 0 {
			t.Errorf("%s: %v %q installed %v", name, r.err, r.out, ops.installed)
		}
	}
}

// A recovery confirmation names the interrupted action from the journal,
// while the outcome and retry keep the requested one.
func TestARecoveryIsConfirmedByItsInterruptedAction(t *testing.T) {
	for _, tc := range []struct{ interrupted, requested string }{{"setup", "uninstall"}, {"uninstall", "setup"}} {
		ops := newFakeOps()
		ops.plans["claude"] = VendorPlan{Action: tc.requested, Adapter: "claude", Name: "Claude Code", Adapters: []string{"claude"},
			Recovery: true, journal: journal{Pending: &operation{Action: tc.interrupted, Adapters: []string{"claude"}}}}
		r := runPrompts(t, ops, Request{Action: tc.requested, Terminal: true, Adapters: []string{"claude"}}, "y\n")
		if r.err != nil || !strings.Contains(r.out, "Recover the interrupted "+tc.interrupted+" of claude without overwriting configuration?") ||
			!strings.Contains(r.out, "claude: recovered; next: crosshall "+tc.requested+" claude") {
			t.Errorf("%+v: %v\n%s", tc, r.err, r.out)
		}
	}
}

// statusAfter changes what Status reports once plans have been prepared.
type statusAfter struct {
	*fakeOps
	after func(*fakeOps)
}

func (o *statusAfter) PlanInstall(ctx context.Context, names []string) ([]VendorPlan, error) {
	plans, err := o.fakeOps.PlanInstall(ctx, names)
	o.after(o.fakeOps)
	return plans, err
}

// Eligibility is checked again after the plans are prepared: a vendor
// installed meanwhile silences the offer, and a decline saved meanwhile
// drops that vendor and is not overwritten.
func TestTheOfferRechecksEligibilityAfterPlanning(t *testing.T) {
	ops := &statusAfter{fakeOps: newFakeOps(), after: func(o *fakeOps) { o.statuses[1].Footprint = "present" }}
	f := setuptest.New(t)
	f.CheckTargets(t)
	s := Service{StateRoot: f.State}
	var out bytes.Buffer
	err := s.prompts(context.Background(), Request{Action: "setup", Offer: true, Terminal: true, Output: &out,
		Input: strings.NewReader("y\ny\ny\ny\n")}, ops)
	if err != nil || out.Len() != 0 || len(ops.installed) != 0 {
		t.Fatalf("install meanwhile: %v %q %v", err, out.String(), ops.installed)
	}

	declineMeanwhile := &statusAfter{fakeOps: newFakeOps(), after: func(*fakeOps) {
		seen, _ := s.offerSnapshot()
		if err := s.rememberAnswers(map[string]answer{"claude": answerNo}, seen, time.UnixMilli(5)); err != nil {
			t.Fatal(err)
		}
	}}
	out.Reset()
	err = s.prompts(context.Background(), Request{Action: "setup", Offer: true, Terminal: true, Output: &out,
		Input: strings.NewReader("y\n")}, declineMeanwhile)
	if err != nil || strings.Contains(out.String(), "Install Claude Code hooks") || !slices.Equal(declineMeanwhile.installed, []string{"codex"}) {
		t.Fatalf("decline meanwhile: %v %v\n%s", err, declineMeanwhile.installed, out.String())
	}
	if d, _ := s.declinedOffers(); !d["claude"] {
		t.Fatal("the decline saved meanwhile was lost")
	}
}
