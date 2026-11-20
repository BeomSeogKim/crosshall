package setup

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"strings"

	"github.com/BeomSeogKim/crosshall/internal/hook"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

// Service uses the owner transport to apply adapter policy after config edits.
type Service struct {
	StateRoot  string
	Executable string
	SetPolicy  func(context.Context, string, bool) error
}

type Request struct {
	Action      string
	DryRun      bool
	JSON        bool
	Input       io.Reader
	Output      io.Writer
	Prompt      io.Writer
	Adapters    []string
	Apply       bool
	Offer       bool
	Terminal    bool
	PathConsent func(bool) error
}

type plan struct {
	Version   int             `json:"v"`
	OK        bool            `json:"ok"`
	Kind      string          `json:"kind"`
	DryRun    bool            `json:"dry_run"`
	Applied   bool            `json:"applied"`
	Resume    bool            `json:"resume,omitempty"`
	Recovery  bool            `json:"recovery,omitempty"`
	Adapters  []string        `json:"adapters"`
	Files     []PlanFile      `json:"files"`
	Approvals []ApprovalOffer `json:"approvals,omitempty"`
	Retained  []retainedFile  `json:"retained,omitempty"`
}

type PlanFile struct {
	Adapter    string                 `json:"adapter"`
	Path       string                 `json:"path"`
	PathSource string                 `json:"path_source,omitempty"`
	Version    string                 `json:"version,omitempty"`
	Add        []Entry                `json:"add,omitempty"`
	Remove     []Entry                `json:"remove,omitempty"`
	Kept       []Entry                `json:"kept,omitempty"`
	Evidence   []store.DoctorEvidence `json:"capability_evidence,omitempty"`
	Reminder   *reminderStatus        `json:"reminder,omitempty"`
}

type ApprovalOffer struct {
	Adapter string   `json:"adapter"`
	Kind    string   `json:"kind"`
	Rules   []string `json:"rules,omitempty"`
	Manual  bool     `json:"manual,omitempty"`
}

func (s Service) Run(ctx context.Context, req Request) error {
	if req.Action == "uninstall" {
		empty, err := s.noUninstallFootprint(ctx, req.Adapters)
		if err != nil {
			return err
		}
		if !req.JSON {
			scope := "Uninstall removes only crosshall-managed hooks and related settings."
			if empty {
				scope = "No crosshall adapter hooks are installed"
				if len(req.Adapters) > 0 {
					scope += " for " + strings.Join(req.Adapters, ", ")
				}
				scope += "."
			}
			_, err := fmt.Fprintln(req.Output, scope+"\n"+
				"For full removal, run crosshall owner doctor in a plain terminal to find the state directories.\n"+
				"Then follow docs/operations.md#removing-crosshall in your release checkout.")
			if err != nil || empty {
				return err
			}
		} else if empty {
			if len(req.Adapters) > 0 {
				return json.NewEncoder(req.Output).Encode(promptResult{
					Version: 1, OK: true, Kind: "uninstall", DryRun: req.DryRun, Vendors: []vendorOutcome{},
				})
			}
			return printResult(req, displayPlan(req, operation{}, nil, false))
		}
	}
	// The per-vendor forms of issue #25. An unnamed --dry-run keeps its rc.6
	// output, as do a bare uninstall and setup off; the command line stops an
	// unnamed setup without a terminal before it reaches here.
	if req.Action == "status" || req.Offer || len(req.Adapters) > 0 || req.Action == "setup" && req.Terminal && !req.DryRun {
		return s.prompts(ctx, req, s)
	}
	if req.Action == "off" {
		op := operation{Action: req.Action, Adapters: adapterNames()}
		state, lock, err := s.lockState()
		if err != nil {
			return err
		}
		defer state.Close()
		defer lock.Close()
		if err := s.setPolicies(ctx, op.Adapters, false); err != nil {
			return err
		}
		p := displayPlan(req, op, nil, false)
		p.Applied = true
		if req.JSON {
			return printResult(req, p)
		}
		_, err = fmt.Fprintln(req.Output, "Adapter hooks disabled.")
		return err
	}
	if req.Action != "setup" && req.Action != "uninstall" {
		return errors.New("unknown adapter installation action")
	}

	j, digest, err := readJournal(s.StateRoot)
	if err != nil {
		return err
	}
	var op operation
	var targets []installTarget
	prompts := req.Output
	if req.JSON {
		prompts = req.Prompt
	}
	scanner := bufio.NewScanner(req.Input)
	resume := j.Pending != nil
	recovery := false
	if resume {
		pathErr := pendingRecoveryPathError(*j.Pending)
		if pathErr != nil && req.Action != "uninstall" {
			return pathErr
		}
		recovery = pathErr != nil || !pendingCanResume(*j.Pending, req.Action)
		if recovery {
			if req.DryRun {
				if pathErr != nil {
					p := displayPlan(req, pendingShutdownPlan(*j.Pending, pathErr), nil, false)
					p.Recovery = true
					return printResult(req, p)
				}
				reconcilePublished(&j)
			} else {
				prompt := fmt.Sprintf("Recover the interrupted %s without overwriting configuration?", j.Pending.Action)
				if req.Action == "uninstall" {
					prompt = "Recover the interrupted plan and disable all adapter hooks if a path must be retained?"
				}
				if pathErr != nil {
					p := displayPlan(req, pendingShutdownPlan(*j.Pending, pathErr), nil, false)
					p.Recovery = true
					if err := printPlan(prompts, p); err != nil {
						return err
					}
				}
				ok, err := confirm(scanner, prompts, prompt)
				if err != nil {
					return err
				}
				if !ok {
					p := displayPlan(req, *j.Pending, nil, false)
					p.Recovery = true
					return printCompletion(req, p)
				}
				j, digest, err = s.recoverJournal(ctx, digest, req.Action == "uninstall")
				if err != nil {
					return err
				}
				if j.Pending != nil {
					op := operation{Action: "uninstall", Adapters: adapterNames(), Retained: j.Pending.Retained}
					p := displayPlan(req, op, nil, false)
					p.Applied, p.Recovery = true, true
					if req.JSON {
						return printResult(req, p)
					}
					if pathErr == nil {
						if err := printPlan(req.Output, p); err != nil {
							return err
						}
					}
					_, err := fmt.Fprintln(req.Output, "Adapter hooks disabled. Interrupted configuration and journal retained.")
					return err
				}
			}
			resume = false
		}
	}
	switch {
	case resume:
		op = *j.Pending
		if op.Action == "setup" {
			for _, e := range op.Edits {
				targets = append(targets, installTarget{
					Spec: hook.InstallSpec{Adapter: e.Adapter}, Path: e.Path, Build: op.Builds[e.Adapter],
				})
			}
			s.attachEvidence(ctx, targets)
		}
	case req.Action == "uninstall":
		op = makeUninstall(j)
	default:
		targets, err = s.targets(ctx, j)
		if err == nil {
			op, err = makeInstall(j, targets, false, false)
		}
	}
	if err != nil {
		return err
	}
	if err := verifyTargets(op); err != nil {
		return err
	}

	p := displayPlan(req, op, targets, resume)
	p.Recovery = recovery
	if req.DryRun {
		return printResult(req, p)
	}
	if len(op.Adapters) == 0 {
		return printResult(req, p)
	}
	if err := printPlan(prompts, p); err != nil {
		return err
	}
	if !resume {
		ok, err := confirm(scanner, prompts, "Apply this plan?")
		if err != nil {
			return err
		}
		if !ok {
			return printCompletion(req, p)
		}
		read, send, err := confirmClaudeRules(scanner, prompts, p.Approvals)
		if err != nil {
			return err
		}
		if read || send {
			op, err = makeInstall(j, targets, read, send)
			if err != nil {
				return err
			}
			p = displayPlan(req, op, targets, resume)
			p.Recovery = recovery
		}
	}
	if err := s.apply(ctx, digest, op); err != nil {
		return err
	}
	p.Applied = true
	return printCompletion(req, p)
}

func adapterNames() []string {
	var names []string
	for _, spec := range hook.InstallSpecs() {
		names = append(names, spec.Adapter)
	}
	return names
}

func confirm(scanner *bufio.Scanner, out io.Writer, label string) (bool, error) {
	if _, err := fmt.Fprintf(out, "%s [y/N] ", label); err != nil {
		return false, err
	}
	if !scanner.Scan() {
		return false, scanner.Err()
	}
	answer := strings.ToLower(strings.TrimSpace(scanner.Text()))
	return answer == "y" || answer == "yes", nil
}

func confirmClaudeRules(scanner *bufio.Scanner, out io.Writer, approvals []ApprovalOffer) (bool, bool, error) {
	if len(approvals) == 0 || approvals[0].Adapter != "claude" {
		return false, false, nil
	}
	read, err := confirm(scanner, out, "Add Claude Code read-command allow rules?")
	if err != nil {
		return false, false, err
	}
	send, err := confirm(scanner, out, "Add Claude Code send-command allow rules?")
	return read, send, err
}

func (s Service) setPolicies(ctx context.Context, adapters []string, enabled bool) error {
	if s.SetPolicy == nil {
		return errors.New("adapter policy transport is unavailable")
	}
	for _, adapter := range adapters {
		if err := s.SetPolicy(ctx, adapter, enabled); err != nil {
			return fmt.Errorf("%s adapter policy: %w", adapter, err)
		}
	}
	return nil
}

func (s Service) apply(ctx context.Context, digest string, op operation) error {
	state, lock, err := s.lockState()
	if err != nil {
		return err
	}
	defer state.Close()
	defer lock.Close()
	j, observed, err := readJournalAt(state)
	if err != nil {
		return err
	}
	if observed != digest {
		return errors.New("installation changed after planning; rerun to review the new plan")
	}
	if op.Action == "uninstall" {
		// Confirmation covers disabling all adapter hooks even if a configuration
		// changes after the preview and prevents removal.
		if err := s.setPolicies(ctx, op.Adapters, false); err != nil {
			return err
		}
	}
	if err := verifyTargets(op); err != nil {
		return err
	}
	if j.Pending == nil {
		for _, e := range op.Edits {
			current, err := readConfig(e.Path)
			if err != nil {
				return err
			}
			if stamp(current) != e.Before {
				return errors.New("configuration changed after planning; rerun to review the new plan")
			}
		}
		if op.Action == "setup" && !hasFileEdits(op) && buildsUnchanged(j.Builds, op.Builds) {
			return s.setPolicies(ctx, op.Adapters, true)
		}
		j.Pending = &op
		if err := writeJournal(state, j); err != nil {
			return err
		}
	}
	for i := range j.Pending.Edits {
		e := &j.Pending.Edits[i]
		result, err := applyEdit(state, j, e)
		if err != nil {
			return fmt.Errorf("%s configuration: %w", e.Adapter, err)
		}
		recordApplied(&j, *e, result)
	}
	if op.Action == "setup" {
		if err := s.setPolicies(ctx, op.Adapters, true); err != nil {
			return err
		}
		maps.Copy(j.Builds, op.Builds)
	}
	j.Pending = nil
	return writeJournal(state, j)
}

func hasFileEdits(op operation) bool {
	for _, e := range op.Edits {
		if e.Before != e.After {
			return true
		}
	}
	return false
}

func buildsUnchanged(before, after map[string]build) bool {
	for adapter, next := range after {
		if before[adapter] != next {
			return false
		}
	}
	return true
}

func displayPlan(req Request, op operation, targets []installTarget, resume bool) plan {
	p := plan{Version: 1, OK: true, Kind: req.Action, DryRun: req.DryRun, Resume: resume,
		Adapters: append([]string{}, op.Adapters...), Files: []PlanFile{}, Retained: op.Retained}
	if req.Action == "off" {
		p.Kind = "setup off"
	}
	for _, e := range op.Edits {
		b := op.Builds[e.Adapter]
		f := PlanFile{Adapter: e.Adapter, Path: e.Path, Version: b.Version,
			Add: e.Add, Remove: e.Remove, Kept: e.Kept}
		for _, target := range targets {
			if target.Path == f.Path {
				f.Evidence = target.Evidence
				f.PathSource = target.PathSource
				break
			}
		}
		if p.Kind == "setup" {
			status := releaseReminderStatus(e.Adapter, b.Version, b.Platform, b.Revision, f.Evidence)
			f.Reminder = &status
		}
		p.Files = append(p.Files, f)
	}
	for _, target := range targets {
		p.Approvals = append(p.Approvals, approvalOffers(target.Spec.Adapter)...)
	}
	return p
}

func approvalOffers(adapter string) []ApprovalOffer {
	if adapter != "claude" && adapter != "codex" {
		return nil
	}
	var offers []ApprovalOffer
	for _, kind := range []string{"read", "send"} {
		onSend := kind == "send"
		o := ApprovalOffer{Adapter: adapter, Kind: kind}
		if adapter == "claude" {
			for _, entry := range claudeRules(onSend) {
				var rule string
				_ = json.Unmarshal(entry.Value, &rule)
				o.Rules = append(o.Rules, rule)
			}
		} else {
			o.Manual = true
			verbs := []string{"inbox", "who", "waiting", "status", "read", "log"}
			if onSend {
				verbs = []string{"note", "ask", "reply"}
			}
			for _, verb := range verbs {
				o.Rules = append(o.Rules, fmt.Sprintf(`prefix_rule(pattern=["crosshall", %q], decision="allow")`, verb))
			}
		}
		offers = append(offers, o)
	}
	return offers
}

func printPlan(out io.Writer, p plan) error {
	var contents strings.Builder
	if len(p.Adapters) == 0 {
		fmt.Fprintln(&contents, "No supported vendor executable found.")
	}
	if p.Resume {
		fmt.Fprintln(&contents, "Recovering the previously confirmed plan.")
	}
	if p.Recovery {
		fmt.Fprintln(&contents, "Recover the interrupted plan before applying new edits.")
	}
	for _, item := range p.Retained {
		fmt.Fprintf(&contents, "  retain %s %s (%s)\n", item.Adapter, item.Path, item.Reason)
	}
	for _, f := range p.Files {
		if f.Version == "" {
			fmt.Fprintf(&contents, "%s %s\n", f.Adapter, f.Path)
		} else {
			fmt.Fprintf(&contents, "%s %s %s\n", f.Adapter, f.Version, f.Path)
			if len(f.Evidence) == 0 {
				fmt.Fprintln(&contents, "  Capability evidence for this build: unknown")
			}
			for _, e := range f.Evidence {
				fmt.Fprintf(&contents, "  %s %s/%s %s\n", e.Capability, e.Platform, e.Mode, e.Level)
			}
		}
		if f.Reminder != nil {
			fmt.Fprintf(&contents, "  Reply reminder %s (no opt-in)\n", f.Reminder.ReleaseState)
			for _, evidence := range f.Reminder.Evidence {
				if evidence.Mode == "" {
					fmt.Fprintf(&contents, "  Reminder evidence %s\n", evidence.Level)
					continue
				}
				fmt.Fprintf(&contents, "  Reminder evidence %s %s/%s %s — %s", evidence.Version,
					evidence.Platform, evidence.Mode, evidence.ManifestRev, evidence.Level)
				if evidence.Reason != "" {
					fmt.Fprintf(&contents, " — %s", evidence.Reason)
				}
				fmt.Fprintln(&contents)
			}
			fmt.Fprintf(&contents, "  If delivered, the reminder adds %d model request. %s\n",
				f.Reminder.ExtraModelRequests, f.Reminder.Provenance)
		}
		for _, group := range []struct {
			label   string
			entries []Entry
		}{{"add", f.Add}, {"remove", f.Remove}, {"keep edited, missing or ambiguous", f.Kept}} {
			for _, e := range group.entries {
				kind := e.Event
				if kind == "" {
					kind = "permissions.allow"
				}
				fmt.Fprintf(&contents, "  %s %s %s\n", group.label, kind, e.Value)
			}
		}
	}
	if p.Kind == "setup" {
		for _, adapter := range p.Adapters {
			fmt.Fprintf(&contents, "Enable %s adapter policy after installation.\n", adapter)
			if adapter == "codex" {
				fmt.Fprintln(&contents, "Review and trust these hooks with Codex /hooks.")
			}
		}
	} else {
		for _, adapter := range p.Adapters {
			fmt.Fprintf(&contents, "Disable %s adapter policy.\n", adapter)
		}
	}
	for _, o := range p.Approvals {
		fmt.Fprintf(&contents, "Optional %s %s-command allow rules\n", o.Adapter, o.Kind)
		if o.Manual {
			fmt.Fprintln(&contents, "  Review manually in your Codex user rules; an allow rule runs its match outside the sandbox.")
		}
		for _, rule := range o.Rules {
			fmt.Fprintf(&contents, "  %s\n", rule)
		}
	}
	_, err := io.WriteString(out, contents.String())
	return err
}

func printResult(req Request, p plan) error {
	if req.JSON {
		return json.NewEncoder(req.Output).Encode(p)
	}
	return printPlan(req.Output, p)
}

func printCompletion(req Request, p plan) error {
	if req.JSON {
		return printResult(req, p)
	}
	if p.Applied {
		_, err := fmt.Fprintln(req.Output, "Adapter policy and configuration updated.")
		return err
	}
	_, err := fmt.Fprintln(req.Output, "Installation changes declined.")
	return err
}
