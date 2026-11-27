package setup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/BeomSeogKim/crosshall/internal/hook/manifests"
	"io"
	"os"
	"reflect"
	"slices"
	"strings"

	"github.com/BeomSeogKim/crosshall/internal/hook"
)

// ClaudeRules reports the read and send approvals in a pending Claude Code plan.
type ClaudeRules struct {
	Read bool `json:"read"`
	Send bool `json:"send"`
}

// RetainedPath identifies a configuration the installer could not safely remove.
type RetainedPath = retainedFile

// VendorPlan presents an installation or removal for confirmation. Its private
// fields bind application to the observed configuration and journal.
type VendorPlan struct {
	Action       string         `json:"action"`
	Adapter      string         `json:"adapter,omitempty"`
	Name         string         `json:"name,omitempty"`
	Path         string         `json:"path,omitempty"`
	PathSource   string         `json:"path_source,omitempty"`
	RecordedPath string         `json:"recorded_path,omitempty"`
	Adapters     []string       `json:"adapters"`
	Files        []PlanFile     `json:"files"`
	Retained     []RetainedPath `json:"retained,omitempty"`
	Resume       bool           `json:"resume,omitempty"`
	Recovery     bool           `json:"recovery,omitempty"`
	Err          error          `json:"-"`

	op         operation
	journal    journal
	digest     string
	disclosure installDisclosure
}

// PendingScopeError reports the recorded scope before any recovery proceeds.
type PendingScopeError struct {
	Action   string
	Adapters []string
	Rules    ClaudeRules
}

func (e *PendingScopeError) Error() string {
	return fmt.Sprintf("interrupted %s for %s needs recovery before a different adapter selection",
		e.Action, strings.Join(e.Adapters, ", "))
}

func pendingClaudeRules(op operation) ClaudeRules {
	added := func(send bool) bool {
		for _, e := range op.Edits {
			if !installerFor(e.Adapter).inlineRules() {
				continue
			}
			for _, rule := range claudeRules(send) {
				if slices.ContainsFunc(e.Add, func(entry Entry) bool {
					return entry.Event == rule.Event && bytes.Equal(entry.Value, rule.Value)
				}) {
					return true
				}
			}
		}
		return false
	}
	return ClaudeRules{Read: added(false), Send: added(true)}
}

// PlanInstall preflights the requested vendors before probing their versions.
func (s Service) PlanInstall(ctx context.Context, adapters []string) ([]VendorPlan, error) {
	return s.vendorPlans(ctx, "setup", adapters)
}

// PlanUninstall inspects journaled ownership without requiring the vendor CLI.
func (s Service) PlanUninstall(ctx context.Context, adapters []string) ([]VendorPlan, error) {
	return s.vendorPlans(ctx, "uninstall", adapters)
}

func (s Service) vendorPlans(ctx context.Context, action string, adapters []string) ([]VendorPlan, error) {
	specs, err := selectedSpecs(adapters)
	if err != nil {
		return nil, err
	}
	j, digest, err := readJournal(s.StateRoot)
	if err != nil {
		return nil, err
	}
	if j.Pending != nil {
		return s.pendingVendorPlan(ctx, action, adapters, j, digest)
	}

	plans := make([]VendorPlan, len(specs))
	if action == "uninstall" {
		for i, spec := range specs {
			op := operation{Action: action}
			for _, retained := range j.Retained {
				if retained.Adapter == spec.Adapter {
					op.Retained = append(op.Retained, retained)
				}
			}
			// Each vendor's removal has its own consent.
			addRemoval(&op, spec, j, nil)
			plans[i] = newVendorPlan(op, nil, j, digest, false)
		}
		return plans, nil
	}

	var targets []installTarget
	var targetIndexes []int
	for i, spec := range specs {
		target, err := s.preflightInstall(spec, j)
		if err == nil {
			targets = append(targets, target)
			targetIndexes = append(targetIndexes, i)
			continue
		}
		op := operation{Action: action, Adapters: []string{spec.Adapter}}
		plans[i] = newVendorPlan(op, nil, j, digest, false)
		plans[i].Err = err
	}
	s.completeTargets(ctx, j, targets)
	// One consent covers the group, so a skill path two vendors share gets
	// one edit, in the first vendor's plan. The later vendor uses it through
	// its recorded hooks path and makes no claim of its own.
	skills := map[string]string{}
	for i, target := range targets {
		single := targets[i : i+1]
		op, err := makeInstall(j, single)
		if err != nil {
			op = operation{Action: action, Adapters: []string{target.Spec.Adapter}}
		}
		var shared *PlanFile
		op.Edits = slices.DeleteFunc(op.Edits, func(e edit) bool {
			first, ok := skills[e.Path]
			if !e.Skill {
				return false
			} else if !ok {
				skills[e.Path] = e.Adapter
				return false
			}
			shared = &PlanFile{Adapter: e.Adapter, Path: e.Path, Skill: "shared", SharedWith: manifests.DisplayName(first)}
			return true
		})
		plan := newVendorPlan(op, single, j, digest, false)
		if shared != nil {
			plan.Files = append(plan.Files, *shared)
		}
		plans[targetIndexes[i]] = plan
		plans[targetIndexes[i]].Err = err
	}
	return plans, nil
}

func selectedSpecs(adapters []string) ([]hook.InstallSpec, error) {
	specs := hook.InstallSpecs()
	if len(adapters) == 0 {
		return specs, nil
	}
	selected := make([]hook.InstallSpec, 0, len(adapters))
	for i, adapter := range adapters {
		index := slices.IndexFunc(specs, func(spec hook.InstallSpec) bool { return spec.Adapter == adapter })
		if index < 0 || slices.Contains(adapters[:i], adapter) {
			return nil, fmt.Errorf("unknown or duplicate hook adapter %q", adapter)
		}
		selected = append(selected, specs[index])
	}
	return selected, nil
}

func (s Service) pendingVendorPlan(ctx context.Context, action string, adapters []string, j journal, digest string) ([]VendorPlan, error) {
	original := *j.Pending
	if len(adapters) > 0 && !sameAdapterScope(original.Adapters, adapters) {
		return nil, &PendingScopeError{Action: original.Action, Adapters: slices.Clone(original.Adapters), Rules: pendingClaudeRules(original)}
	}
	pathErr := pendingRecoveryPathError(original)
	if pathErr != nil && action != "uninstall" {
		return nil, pathErr
	}
	if pathErr == nil && pendingCanResume(original, action) {
		var targets []installTarget
		if action == "setup" {
			for _, e := range original.Edits {
				targets = append(targets, installTarget{Spec: hook.InstallSpec{Adapter: e.Adapter}, Path: e.Path, Build: original.Builds[e.Adapter]})
			}
			s.attachEvidence(ctx, targets)
		}
		return []VendorPlan{newVendorPlan(original, targets, j, digest, true)}, nil
	}
	recovery := operation{Action: action, Adapters: original.Adapters}
	if pathErr != nil {
		recovery.Retained = pendingShutdownPlan(original, pathErr).Retained
	}
	p := newVendorPlan(recovery, nil, j, digest, false)
	p.Recovery = true
	return []VendorPlan{p}, nil
}

func sameAdapterScope(a, b []string) bool {
	return slices.Equal(slices.Sorted(slices.Values(a)), slices.Sorted(slices.Values(b)))
}

func (p VendorPlan) resumedLegacyCodex() bool {
	if !p.Resume || p.Action != "setup" {
		return false
	}
	for _, e := range p.op.Edits {
		entries := e.Add
		if e.Record != nil {
			entries = e.Record.Entries
		}
		if slices.ContainsFunc(entries, installerFor(e.Adapter).legacyHook) {
			return true
		}
	}
	return false
}

func newVendorPlan(op operation, targets []installTarget, j journal, digest string, resume bool) VendorPlan {
	view := displayPlan(Request{Action: op.Action}, op, targets, resume)
	p := VendorPlan{
		Action: op.Action, Adapters: view.Adapters, Files: view.Files,
		Retained: view.Retained, Resume: resume, op: op, journal: j, digest: digest,
		disclosure: view.disclosure,
	}
	if len(op.Adapters) == 1 {
		p.Adapter = op.Adapters[0]
		p.Name = manifests.DisplayName(p.Adapter)
		for _, spec := range hook.InstallSpecs() {
			if spec.Adapter == p.Adapter {
				path, _ := resolveConfigPath(spec)
				p.Path, p.PathSource = path.Path, path.Source
				break
			}
		}
		if path := recordedPlanPath(j, p.Adapter); path != p.Path {
			p.RecordedPath = path
		}
		for i := range p.Files {
			if p.Files[i].Path == p.Path {
				p.Files[i].PathSource = p.PathSource
			}
		}
	}
	return p
}

func recordedPlanPath(j journal, adapter string) string {
	if path := j.Files[adapter].Path; path != "" {
		return path
	}
	if path := j.RuleFiles[adapter].Path; path != "" {
		return path
	}
	if j.Pending != nil {
		for _, e := range j.Pending.Edits {
			if e.Adapter == adapter {
				return e.Path
			}
		}
	}
	for _, file := range j.Retained {
		if file.Adapter == adapter {
			return file.Path
		}
	}
	return j.SkillFiles[adapter].Path
}

// RenderVendorPlan writes the hook and approval preview for a prepared plan.
func RenderVendorPlan(w io.Writer, p VendorPlan) error {
	return printPlan(w, plan{Kind: p.Action, Adapters: p.Adapters, Files: p.Files,
		Retained: p.Retained, Resume: p.Resume, Recovery: p.Recovery, disclosure: p.disclosure})
}

// Install applies a confirmed plan. A resume always keeps the recorded rules.
func (s Service) Install(ctx context.Context, p VendorPlan) error {
	return s.applyVendorPlan(ctx, "setup", p)
}

// Uninstall disables and removes entries only for the confirmed scope.
func (s Service) Uninstall(ctx context.Context, p VendorPlan) error {
	return s.applyVendorPlan(ctx, "uninstall", p)
}

func (s Service) applyVendorPlan(ctx context.Context, action string, p VendorPlan) error {
	if p.Err != nil {
		return p.Err
	}
	if p.op.Action != action || len(p.op.Adapters) == 0 {
		return errors.New("invalid prepared adapter plan")
	}
	if p.Recovery {
		_, _, err := s.recoverJournal(ctx, p.digest, action == "uninstall", p.op.Adapters...)
		return err
	}
	if p.Resume {
		return s.apply(ctx, p.digest, p.op)
	}

	j, digest, err := readJournal(s.StateRoot)
	if err != nil {
		return err
	}
	if j.Pending != nil {
		return &PendingScopeError{Action: j.Pending.Action, Adapters: slices.Clone(j.Pending.Adapters), Rules: pendingClaudeRules(*j.Pending)}
	}
	vendor := p.op.Adapters[0]
	if !reflect.DeepEqual(j.Files[vendor], p.journal.Files[vendor]) ||
		!reflect.DeepEqual(j.RuleFiles[vendor], p.journal.RuleFiles[vendor]) ||
		!reflect.DeepEqual(j.SkillFiles[vendor], p.journal.SkillFiles[vendor]) ||
		j.Builds[vendor] != p.journal.Builds[vendor] ||
		!slices.Equal(retainedFor(j, vendor), retainedFor(p.journal, vendor)) ||
		!sameSkillConsumers(j, p.journal, p.op) {
		return errors.New("installation changed after planning; rerun to review the new plan")
	}
	return s.apply(ctx, digest, p.op)
}

// sameSkillConsumers reports that every skill edit of op still has the
// recorded consumers it was planned with.
func sameSkillConsumers(now, planned journal, op operation) bool {
	for _, e := range op.Edits {
		if e.Skill && !slices.Equal(skillConsumers(now, e.Adapter, e.Path, nil), skillConsumers(planned, e.Adapter, e.Path, nil)) {
			return false
		}
	}
	return true
}

func retainedFor(j journal, vendor string) []retainedFile {
	var files []retainedFile
	for _, file := range j.Retained {
		if file.Adapter == vendor {
			files = append(files, file)
		}
	}
	return files
}

// WithSetupLock serializes updates to the owner's setup preferences.
func (s Service) WithSetupLock(fn func(state *os.Root) error) error {
	state, lock, err := s.lockState()
	if err != nil {
		return err
	}
	defer state.Close()
	defer lock.Close()
	return fn(state)
}
