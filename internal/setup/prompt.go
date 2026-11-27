package setup

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/BeomSeogKim/crosshall/internal/hook"
	"io"
	"slices"
	"strings"
	"time"
)

// operations is what the interaction layer asks of the operations layer.
// Every path, ownership, journal and recovery rule lives behind it.
type operations interface {
	Status(context.Context) ([]AdapterStatus, error)
	PlanInstall(context.Context, []string) ([]VendorPlan, error)
	Install(context.Context, VendorPlan) error
	PlanUninstall(context.Context, []string) ([]VendorPlan, error)
	Uninstall(context.Context, VendorPlan) error
}

// vendorOutcome is one vendor's line in the result.
type vendorOutcome struct {
	Adapter    string `json:"adapter"`
	Path       string `json:"path,omitempty"`
	PathSource string `json:"path_source,omitempty"`
	Outcome    string `json:"outcome"` // previewed|installed|removed|recovered|declined|skipped|failed
	Error      string `json:"error,omitempty"`
	Detail     string `json:"detail,omitempty"`
	Retry      string `json:"retry,omitempty"`
	// Plan is the previewed plan, so a JSON preview carries what it shows.
	Plan *VendorPlan `json:"plan,omitempty"`
}

type promptResult struct {
	Version int             `json:"v"`
	OK      bool            `json:"ok"`
	Kind    string          `json:"kind"`
	DryRun  bool            `json:"dry_run"`
	Vendors []vendorOutcome `json:"vendors"`
}

// choice is the owner's decision for one vendor plan.
type choice struct {
	plan   VendorPlan
	answer answer
}

type prompter struct {
	s     Service
	ops   operations
	req   Request
	out   io.Writer // questions and plans; stderr under --json
	input *bufio.Reader
	now   func() time.Time
}

func (s Service) prompts(ctx context.Context, req Request, ops operations) error {
	p := &prompter{s: s, ops: ops, req: req, out: req.Output, input: bufio.NewReader(req.Input), now: time.Now}
	if req.JSON {
		p.out = req.Prompt
		if p.out == nil {
			p.out = io.Discard
		}
	}
	var promptErr error
	switch {
	case req.Action == "status":
		statuses, err := ops.Status(ctx)
		if err != nil {
			return err
		}
		return RenderStatus(req.Output, statuses, req.JSON)
	case req.Offer:
		promptErr = p.offer(ctx)
	case req.Action == "uninstall":
		return p.uninstall(ctx)
	default:
		promptErr = p.setup(ctx)
	}
	if promptErr == nil && req.OfferTelemetry != nil {
		return req.OfferTelemetry(p.input)
	}
	return promptErr
}

// offer is the installer's form. It asks only in an owner terminal when no
// shipped hook adapter has any footprint, and only about vendors whose CLI
// was found and which the owner has not declined. Anything else is silent.
func (p *prompter) offer(ctx context.Context) error {
	if !p.req.Terminal {
		return nil
	}
	statuses, err := p.ops.Status(ctx)
	if err != nil {
		return nil
	}
	declined, err := p.s.declinedOffers()
	if err != nil {
		return nil
	}
	var names []string
	for _, st := range statuses {
		if !st.HookAdapter {
			continue
		}
		if st.Footprint != "none" {
			return nil
		}
		if st.CLI == "found" && !declined[st.Adapter] {
			names = append(names, st.Adapter)
		}
	}
	if len(names) == 0 {
		return nil
	}
	// Work that became pending after the status read is not offered.
	plans, err := p.ops.PlanInstall(ctx, names)
	if err != nil {
		return nil
	}
	for _, plan := range plans {
		if plan.Resume || plan.Recovery {
			return nil
		}
	}
	// Check eligibility again now that the plans are ready: an install or a
	// decline made meanwhile wins. The decisions read here are the baseline
	// for the answers given below.
	statuses, err = p.ops.Status(ctx)
	if err != nil {
		return nil
	}
	eligible := make(map[string]bool)
	for _, st := range statuses {
		if st.HookAdapter && st.Footprint != "none" {
			return nil
		}
		eligible[st.Adapter] = st.HookAdapter && st.CLI == "found"
	}
	prefs, err := p.s.readOfferPrefs()
	if err != nil {
		return nil
	}
	plans = slices.DeleteFunc(plans, func(plan VendorPlan) bool {
		return !eligible[plan.Adapter] || prefs.Vendors[plan.Adapter].Answer == "no"
	})
	if len(plans) == 0 {
		return nil
	}
	return p.askFrom(ctx, plans, prefs.snapshot(), nil)
}

func (p *prompter) setup(ctx context.Context) error {
	names := p.req.Adapters
	if len(names) == 0 {
		statuses, err := p.ops.Status(ctx)
		if err != nil {
			return err
		}
		for _, st := range statuses {
			if st.HookAdapter && st.CLI == "found" {
				names = append(names, st.Adapter)
			}
		}
		if len(names) == 0 {
			return p.finish(promptResult{Kind: "setup", DryRun: p.req.DryRun}, "No supported agent CLI was found on PATH.")
		}
	}
	if p.req.DryRun || p.req.Apply || !p.req.Terminal {
		return p.unattended(ctx, names)
	}
	plans, err := p.ops.PlanInstall(ctx, names)
	if err != nil {
		return p.pending(err)
	}
	return p.ask(ctx, plans)
}

// unattended previews (--dry-run, or named without --apply and without a
// terminal) or installs the named vendors without reading input (--apply).
func (p *prompter) unattended(ctx context.Context, names []string) error {
	plans, err := p.ops.PlanInstall(ctx, names)
	if err != nil {
		return p.pending(err)
	}
	result := promptResult{Kind: "setup", DryRun: !p.req.Apply}
	var previewed []string
	var fresh []VendorPlan
	for _, plan := range plans {
		o := outcomeOf(plan)
		switch {
		case plan.Err != nil:
			o.Outcome, o.Error, o.Retry = "failed", plan.Err.Error(), retryAfter(plan, plan.Err)
		case !p.req.Apply:
			o.Outcome, o.Plan = "previewed", &plan
			previewed = append(previewed, scopeOf(plan)...)
			if !plan.Resume && !plan.Recovery {
				fresh = append(fresh, plan)
			}
			if err := p.printPlan(plan); err != nil {
				return err
			}
		case plan.Recovery:
			// --apply authorizes the displayed hook changes, not a recovery.
			o.Outcome, o.Error, o.Retry = "failed", "an interrupted operation needs review in a terminal", retry(plan)
		default:
			o = p.apply(ctx, choice{plan: plan, answer: answerYes})
		}
		result.Vendors = append(result.Vendors, o)
	}
	if len(fresh) > 0 {
		if err := groupInstallDisclosure(fresh).write(p.out); err != nil {
			return err
		}
	}
	note := ""
	if !p.req.DryRun && len(previewed) > 0 {
		note = "Nothing was changed. To install without questions: crosshall setup " + strings.Join(previewed, " ") + " --apply"
	}
	return p.finish(result, note)
}

func (p *prompter) ask(ctx context.Context, plans []VendorPlan) error {
	// The decisions as they stood before asking: an answer given here does
	// not replace a decision another run makes meanwhile.
	seen, seenErr := p.s.offerSnapshot()
	return p.askFrom(ctx, plans, seen, seenErr)
}

func (p *prompter) askFrom(ctx context.Context, plans []VendorPlan, seen map[string]int64, seenErr error) error {
	result := promptResult{Kind: "setup"}
	var eligible []VendorPlan
	for _, plan := range plans {
		if plan.Err != nil {
			o := outcomeOf(plan)
			o.Outcome, o.Error, o.Retry = "failed", plan.Err.Error(), retryAfter(plan, plan.Err)
			result.Vendors = append(result.Vendors, o)
			fmt.Fprintf(p.out, "%s: %v\n", plan.Name, plan.Err)
			continue
		}
		if plan.Resume || plan.Recovery {
			if err := p.printPlan(plan); err != nil {
				return err
			}
			a, err := p.interrupted(plan)
			if err != nil {
				return err
			}
			result.Vendors = append(result.Vendors, p.apply(ctx, choice{plan: plan, answer: a}))
			continue
		}
		if _, err := fmt.Fprintf(p.out, "%s hooks in %s (%s)\n", plan.Name, plan.Path, pathSource(plan)); err != nil {
			return err
		}
		if err := p.printPlan(plan); err != nil {
			return err
		}
		eligible = append(eligible, plan)
	}
	if len(eligible) == 0 {
		return p.finish(result, "")
	}
	if err := groupInstallDisclosure(eligible).write(p.out); err != nil {
		return err
	}
	if p.req.Offer {
		if _, err := fmt.Fprintln(p.out, "A no skips future installer offers for the listed agents. Enter skips this offer."); err != nil {
			return err
		}
	}
	label := "Install the listed hooks and command rules?"
	pathConsent := p.req.Offer && p.req.PathConsent != nil
	if pathConsent {
		label = "Apply the shown PATH change and install the listed hooks and command rules?"
	}
	a, err := p.question(label)
	if err != nil {
		return err
	}
	if pathConsent {
		if err := p.req.PathConsent(a == answerYes); err != nil {
			return fmt.Errorf("record installer consent: %w", err)
		}
	}
	if p.req.Offer || a == answerYes {
		answers := make(map[string]answer, len(eligible))
		for _, plan := range eligible {
			answers[plan.Adapter] = a
		}
		if seenErr == nil {
			seenErr = p.s.rememberAnswers(answers, seen, p.now())
		}
		if seenErr != nil {
			fmt.Fprintf(p.out, "Your answers could not be remembered: %v\n", seenErr)
		}
	}
	for _, plan := range eligible {
		result.Vendors = append(result.Vendors, p.apply(ctx, choice{plan: plan, answer: a}))
	}
	return p.finish(result, "")
}

// apply carries out one choice and reports it without stopping the others.
func (p *prompter) apply(ctx context.Context, c choice) vendorOutcome {
	o := outcomeOf(c.plan)
	switch c.answer {
	case answerNo:
		o.Outcome = "declined"
		return o
	case answerSkip:
		o.Outcome = "skipped"
		return o
	}
	err := context.Cause(ctx)
	if err == nil {
		if c.plan.Action == "uninstall" {
			err = p.ops.Uninstall(ctx, c.plan)
		} else {
			err = p.ops.Install(ctx, c.plan)
		}
	}
	// Past the deadline, keep an error of its own and name the deadline after
	// it; a bare context error says nothing more than the deadline does.
	if cause := context.Cause(ctx); err != nil && cause != nil && !errors.Is(err, cause) {
		if err == context.Canceled || err == context.DeadlineExceeded {
			err = cause
		} else {
			err = fmt.Errorf("%w. %w", err, cause)
		}
	}
	switch {
	case err != nil:
		o.Outcome, o.Error, o.Retry = "failed", err.Error(), retryAfter(c.plan, err)
	case c.plan.Recovery:
		o.Outcome, o.Retry = "recovered", retry(c.plan)
	case c.plan.Action == "uninstall":
		o.Outcome = "removed"
		details := skillOutcomes(c.plan.Files)
		for _, kept := range c.plan.Retained {
			if kept.Reason == skillEditedReason {
				details = append(details, fmt.Sprintf("%s (%s)", kept.Reason, kept.Path))
			}
		}
		o.Detail = strings.Join(details, "\n")
	default:
		o.Outcome = "installed"
		details := skillOutcomes(c.plan.Files)
		for _, skipped := range c.plan.Retained {
			details = append(details, fmt.Sprintf("%s (%s)", skipped.Reason, skipped.Path))
		}
		if len(c.plan.Retained) == 0 && c.plan.resumedLegacyCodex() {
			details = append(details, "The resumed Codex hooks still inherit the state directory. Rerun setup to preview and apply the state-root pin.")
			o.Retry = "crosshall setup codex"
		}
		o.Detail = strings.Join(details, "\n")
	}
	return o
}

// uninstall removes the named vendors (a bare uninstall keeps the rc.6 path).
func (p *prompter) uninstall(ctx context.Context) error {
	plans, err := p.ops.PlanUninstall(ctx, p.req.Adapters)
	if err != nil {
		return p.pending(err)
	}
	result := promptResult{Kind: "uninstall", DryRun: p.req.DryRun}
	for _, plan := range plans {
		o := outcomeOf(plan)
		if plan.Err != nil {
			o.Outcome, o.Error, o.Retry = "failed", plan.Err.Error(), retryAfter(plan, plan.Err)
			result.Vendors = append(result.Vendors, o)
			continue
		}
		if err := p.printPlan(plan); err != nil {
			return err
		}
		if p.req.DryRun {
			o.Outcome, o.Plan = "previewed", &plan
			result.Vendors = append(result.Vendors, o)
			continue
		}
		a := answerYes
		if !p.req.Apply {
			if plan.Resume || plan.Recovery {
				a, err = p.interrupted(plan)
			} else {
				a, err = p.question(fmt.Sprintf("Remove the crosshall hooks for %s from %s?", plan.Name, plan.Path))
			}
			if err != nil {
				return err
			}
		}
		result.Vendors = append(result.Vendors, p.apply(ctx, choice{plan: plan, answer: a}))
	}
	return p.finish(result, "")
}

// interrupted asks about an interrupted operation, naming its recorded
// action and scope.
func (p *prompter) interrupted(plan VendorPlan) (answer, error) {
	scope := strings.Join(scopeOf(plan), ", ")
	// The journal records which action was interrupted; plan.Action is the
	// one requested now, which the outcome and retry keep.
	action := plan.Action
	if plan.journal.Pending != nil {
		action = plan.journal.Pending.Action
	}
	if plan.Resume {
		return p.question(fmt.Sprintf("Resume the interrupted %s of %s?", action, scope))
	}
	return p.question(fmt.Sprintf("Recover the interrupted %s of %s without overwriting configuration?", action, scope))
}

// question asks a yes/no question. Only y or yes is a yes and only n or no a
// no; Enter, other text and end of input are a skip.
func (p *prompter) question(label string) (answer, error) {
	if _, err := fmt.Fprintf(p.out, "%s [y/N] ", label); err != nil {
		return answerSkip, err
	}
	line, err := p.input.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		fmt.Fprintln(p.out)
		return answerSkip, err
	}
	if line == "" {
		fmt.Fprintln(p.out)
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return answerYes, nil
	case "n", "no":
		return answerNo, nil
	}
	return answerSkip, nil
}

// pending explains a pending operation whose scope differs from this one.
func (p *prompter) pending(err error) error {
	var scope *PendingScopeError
	if !errors.As(err, &scope) {
		return err
	}
	return fmt.Errorf("an interrupted %s of %s is pending; finish it first with `crosshall %s %s`",
		scope.Action, strings.Join(scope.Adapters, ", "), scope.Action, strings.Join(scope.Adapters, " "))
}

func (p *prompter) printPlan(vp VendorPlan) error {
	adapters := vp.Adapters
	if len(adapters) == 0 {
		adapters = []string{vp.Adapter}
	}
	return printPlan(p.out, plan{Version: 1, OK: true, Kind: vp.Action, DryRun: p.req.DryRun,
		Resume: vp.Resume, Recovery: vp.Recovery, Adapters: adapters, Files: vp.Files,
		Retained: vp.Retained, disclosure: vp.disclosure, quietConsent: !vp.Resume && !vp.Recovery})
}

// finish reports each vendor's outcome and fails when any vendor failed.
func (p *prompter) finish(result promptResult, note string) error {
	result.Version, result.OK = 1, true
	var failed, retries []string
	for _, o := range result.Vendors {
		if o.Outcome == "installed" && p.req.Installed != nil {
			p.req.Installed()
		}
		if o.Outcome == "failed" {
			result.OK = false
			failed = append(failed, o.Adapter)
			retries = append(retries, "`"+o.Retry+"`")
		}
	}
	if p.req.JSON {
		if result.Vendors == nil {
			result.Vendors = []vendorOutcome{}
		}
		if err := json.NewEncoder(p.req.Output).Encode(result); err != nil {
			return err
		}
	} else {
		for _, o := range result.Vendors {
			line := fmt.Sprintf("%s: %s", o.Adapter, o.Outcome)
			if o.Error != "" {
				line += " (" + o.Error + ")"
			}
			if o.Retry != "" {
				line += "; next: " + o.Retry
			}
			fmt.Fprintln(p.req.Output, line)
			if o.Detail != "" {
				fmt.Fprintln(p.req.Output, o.Detail)
			}
		}
		if note != "" {
			fmt.Fprintln(p.req.Output, note)
		}
	}
	if len(failed) > 0 {
		err := fmt.Errorf("%s did not finish for %s; retry with %s",
			result.Kind, strings.Join(failed, ", "), strings.Join(retries, ", "))
		return reportedError{err: err, reported: p.req.JSON}
	}
	return nil
}

// reportedError is a failure whose JSON result was already written, so the
// command line exits non-zero without writing a second object.
type reportedError struct {
	err      error
	reported bool
}

func (e reportedError) Error() string  { return e.err.Error() }
func (e reportedError) Reported() bool { return e.reported }

func outcomeOf(plan VendorPlan) vendorOutcome {
	return vendorOutcome{Adapter: strings.Join(scopeOf(plan), ","), Path: plan.Path, PathSource: plan.PathSource}
}

// scopeOf names the vendors a plan covers: its recorded scope for a legacy
// interrupted operation, else its one vendor.
func scopeOf(plan VendorPlan) []string {
	if plan.Adapter == "" || len(plan.Adapters) > 1 {
		return plan.Adapters
	}
	return []string{plan.Adapter}
}

type refusalWithNext struct {
	message string
	next    string
}

func (e *refusalWithNext) Error() string { return e.message }

func retryAfter(plan VendorPlan, err error) string {
	var refusal *refusalWithNext
	if errors.As(err, &refusal) {
		return refusal.next
	}
	return retry(plan)
}

// retry is the command that repeats a plan's action over its scope.
func retry(plan VendorPlan) string {
	action := plan.Action
	if action == "" {
		action = "setup"
	}
	return "crosshall " + action + " " + strings.Join(scopeOf(plan), " ")
}

func pathSource(plan VendorPlan) string {
	if plan.PathSource == "override" {
		for _, spec := range hook.InstallSpecs() {
			if spec.Adapter == plan.Adapter {
				return "from " + spec.HomeEnv
			}
		}
	}
	source := "default under HOME"
	if plan.RecordedPath != "" && plan.RecordedPath != plan.Path {
		source += "; recorded earlier at " + plan.RecordedPath
	}
	return source
}
