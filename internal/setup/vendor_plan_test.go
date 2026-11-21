package setup

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/setuptest"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

func scopedService(f setuptest.Fixture) Service {
	return Service{
		StateRoot: f.State, Executable: filepath.Join(f.Root, "crosshall"),
		SetPolicy: func(context.Context, string, bool) error { return nil },
	}
}

func configBytes(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestRetainedRecoveryBlocksInstallAtDifferentPath(t *testing.T) {
	f := setuptest.New(t, "claude")
	s := scopedService(f)
	ctx := t.Context()
	oldHome := filepath.Join(f.Home, "old claude")
	t.Setenv("CLAUDE_CONFIG_DIR", oldHome)
	planClaude := func() VendorPlan {
		t.Helper()
		plans, err := s.PlanInstall(ctx, []string{"claude"})
		if err != nil || len(plans) != 1 {
			t.Fatalf("Claude plan: %+v, %v", plans, err)
		}
		return plans[0]
	}
	original := planClaude()
	if original.Err != nil || original.Path != filepath.Join(oldHome, "settings.json") {
		t.Fatalf("override plan: %+v", original)
	}
	setPolicy := s.SetPolicy
	lost := errors.New("interrupted policy update")
	s.SetPolicy = func(context.Context, string, bool) error { return lost }
	f.CheckTargets(t)
	if err := s.Install(ctx, original); !errors.Is(err, lost) {
		t.Fatalf("expected interruption after publishing hooks: %v", err)
	}
	s.SetPolicy = setPolicy

	// An editor replaces the file while preserving the published hooks.
	body := configBytes(t, original.Path)
	replacement := original.Path + ".editor"
	f.CheckTargets(t)
	if err := os.WriteFile(replacement, body, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, original.Path); err != nil {
		t.Fatal(err)
	}
	recovery := planClaude()
	if !recovery.Recovery || recovery.Err != nil {
		t.Fatalf("expected recovery plan: %+v", recovery)
	}
	f.CheckTargets(t)
	if err := s.Install(ctx, recovery); err != nil {
		t.Fatal(err)
	}
	j, digest, err := readJournal(f.State)
	if err != nil || j.Pending != nil || len(j.Files) != 0 ||
		len(j.Retained) != 1 || j.Retained[0].Path != original.Path {
		t.Fatalf("expected a retained-only journal: %+v, %v", j, err)
	}

	if err := os.Unsetenv("CLAUDE_CONFIG_DIR"); err != nil {
		t.Fatal(err)
	}
	resolved := filepath.Join(f.Home, ".claude", "settings.json")
	got := planClaude()
	if got.Err == nil || got.Path != resolved || got.RecordedPath != original.Path ||
		!strings.Contains(got.Err.Error(), original.Path) || !strings.Contains(got.Err.Error(), resolved) {
		t.Fatalf("expected refusal with resolved %q and recorded %q, got %q, %q: %v",
			resolved, original.Path, got.Path, got.RecordedPath, got.Err)
	}
	f.CheckTargets(t)
	if err := s.Install(ctx, got); err == nil {
		t.Fatal("install accepted a plan for a different configuration path")
	}
	if _, err := runSetupTest(t, s, "setup", "", true); err == nil ||
		!strings.Contains(err.Error(), original.Path) || !strings.Contains(err.Error(), resolved) {
		t.Fatalf("unscoped preview must also refuse with both paths: %v", err)
	}
	status := readAdapterStatuses(t, f, s.Executable)[0]
	if status.Path != resolved || status.RecordedPath != original.Path || status.Owned || status.Footprint != "present" {
		t.Fatalf("retained configuration status: %+v", status)
	}
	if _, err := os.Lstat(resolved); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("default config must remain absent: %v", err)
	}
	if _, after, err := readJournal(f.State); err != nil || after != digest ||
		!bytes.Equal(configBytes(t, original.Path), body) {
		t.Fatalf("refusal changed the retained file or journal: %v", err)
	}

	t.Setenv("CLAUDE_CONFIG_DIR", oldHome)
	if same := planClaude(); same.Err != nil || same.Path != original.Path {
		t.Fatalf("explicit reconciliation at the recorded path must remain available: %+v", same)
	}
}

func TestScopedSetupAndUninstallPreserveOtherVendor(t *testing.T) {
	f := setuptest.New(t, "claude", "codex")
	s := scopedService(f)
	ctx := t.Context()
	var policy []string
	s.SetPolicy = func(_ context.Context, adapter string, enabled bool) error {
		flag := "off"
		if enabled {
			flag = "on"
		}
		policy = append(policy, adapter+" "+flag)
		return nil
	}
	plans, err := s.PlanInstall(ctx, []string{"claude", "codex"})
	if err != nil || len(plans) != 2 {
		t.Fatalf("two plans: %+v, %v", plans, err)
	}
	claude, codex := plans[0], plans[1]
	if claude.Err != nil || codex.Err != nil || claude.PathSource != "default" || codex.PathSource != "default" ||
		claude.Path != filepath.Join(f.Home, ".claude", "settings.json") ||
		codex.Path != filepath.Join(f.Home, ".codex", "hooks.json") ||
		!slices.Equal(claude.Adapters, []string{"claude"}) || !slices.Equal(codex.Adapters, []string{"codex"}) {
		t.Fatalf("unexpected plan scope: %+v, %+v", claude, codex)
	}
	f.CheckTargets(t)
	if err := s.Install(ctx, claude); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(codex.Path); !errors.Is(err, os.ErrNotExist) || !slices.Equal(policy, []string{"claude on"}) {
		t.Fatalf("Claude changed Codex: stat=%v policy=%v", err, policy)
	}
	// Confirmation was collected in one batch. A completed disjoint first
	// plan changes the shared journal, yet the second approval remains valid.
	f.CheckTargets(t)
	if err := s.Install(ctx, codex); err != nil {
		t.Fatal(err)
	}
	codexBefore := configBytes(t, codex.Path)
	oldClaude := configBytes(t, claude.Path)
	rules, _, err := memberCommandRules("claude", s.Executable)
	if err != nil || len(rules) == 0 || !bytes.Contains(oldClaude, rules[0].Value) || !slices.Equal(policy, []string{"claude on", "codex on"}) {
		t.Fatalf("missing read approval or incorrect policy: %s %v", oldClaude, policy)
	}
	edited := bytes.Replace(oldClaude, []byte(s.Executable), []byte(filepath.Join(f.Root, "owner-edited")), 1)
	f.CheckTargets(t)
	if err := os.WriteFile(claude.Path, edited, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(f.Bin, "claude")); err != nil {
		t.Fatal(err)
	}
	rm, err := s.PlanUninstall(ctx, []string{"claude"})
	if err != nil || len(rm) != 1 || len(rm[0].Files) != 1 || len(rm[0].Files[0].Kept) == 0 {
		t.Fatalf("scope or drift missing from uninstall plan: %+v, %v", rm, err)
	}
	f.CheckTargets(t)
	if err := s.Uninstall(ctx, rm[0]); err != nil {
		t.Fatal(err)
	}
	remainingClaude := configBytes(t, claude.Path)
	if !bytes.Contains(remainingClaude, []byte("owner-edited")) || bytes.Contains(remainingClaude, rules[0].Value) ||
		!bytes.Equal(configBytes(t, codex.Path), codexBefore) ||
		!slices.Equal(policy, []string{"claude on", "codex on", "claude off"}) {
		t.Fatalf("scoped removal affected owner edits or another vendor: %s %v", remainingClaude, policy)
	}
	j, _, err := readJournal(f.State)
	if err != nil || j.Builds["codex"].Version == "" || j.Files["codex"].Path != codex.Path {
		t.Fatalf("unselected journal entry lost: %+v, %v", j, err)
	}
}

func TestScopedConcurrentInstallRequiresFreshPlan(t *testing.T) {
	f := setuptest.New(t, "claude")
	s := scopedService(f)
	first, err := s.PlanInstall(t.Context(), []string{"claude"})
	if err != nil || len(first) != 1 || first[0].Err != nil {
		t.Fatalf("first plan: %+v, %v", first, err)
	}
	second, err := s.PlanInstall(t.Context(), []string{"claude"})
	if err != nil || len(second) != 1 || second[0].Err != nil {
		t.Fatalf("second plan: %+v, %v", second, err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, p := range []VendorPlan{first[0], second[0]} {
		f.CheckTargets(t)
		go func() {
			<-start
			results <- s.Install(t.Context(), p)
		}()
	}
	close(start)
	a, b := <-results, <-results
	if a == nil {
		a, b = b, a
	}
	var pending *PendingScopeError
	if b != nil || a == nil || (!strings.Contains(a.Error(), "changed after planning") &&
		!errors.Is(a, syscall.EWOULDBLOCK) && !errors.As(a, &pending)) {
		t.Fatalf("wanted one approval to apply and one stale plan to fail: %v, %v", a, b)
	}
	f.CheckTargets(t)
	if err := s.Install(t.Context(), first[0]); err == nil || !strings.Contains(err.Error(), "changed after planning") {
		t.Fatalf("stale approval reused after the other install finished: %v", err)
	}
}

func TestScopedPlanRejectsAnOwnerEditAfterPreview(t *testing.T) {
	f := setuptest.New(t, "claude")
	s := scopedService(f)
	plans, err := s.PlanInstall(t.Context(), []string{"claude"})
	if err != nil || len(plans) != 1 || plans[0].Err != nil {
		t.Fatalf("plan: %+v, %v", plans, err)
	}
	path := plans[0].Path
	owner := []byte(`{"owner":"new settings"}`)
	f.CheckTargets(t)
	if err := os.Mkdir(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	f.CheckTargets(t)
	if err := os.WriteFile(path, owner, 0600); err != nil {
		t.Fatal(err)
	}
	f.CheckTargets(t)
	if err := s.Install(t.Context(), plans[0]); err == nil {
		t.Fatal("a stale approval overwrote a configuration")
	}
	if got := configBytes(t, path); !bytes.Equal(got, owner) {
		t.Fatalf("changed owner configuration: %s", got)
	}
}

func TestScopedSelectionCannotResumeSubsetOfLegacyJournal(t *testing.T) {
	f := setuptest.New(t, "claude", "codex")
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(f.Home, ".claude"))
	t.Setenv("CODEX_HOME", filepath.Join(f.Home, ".codex"))
	s := scopedService(f)
	ctx := t.Context()
	stopped := errors.New("fixture interrupted before policy")
	s.SetPolicy = func(context.Context, string, bool) error { return stopped }
	f.CheckTargets(t)
	j, digest, err := readJournal(s.StateRoot)
	if err != nil {
		t.Fatal(err)
	}
	targets, err := s.targets(ctx, j)
	if err != nil {
		t.Fatal(err)
	}
	for i := range targets {
		targets[i].CommandRules = nil
		targets[i].Wanted = slices.DeleteFunc(targets[i].Wanted, func(e Entry) bool { return e.Event == "" })
		if targets[i].Spec.Adapter == "claude" {
			targets[i].Wanted = append(targets[i].Wanted, claudeRules(false)...)
		}
	}
	op, err := makeInstall(j, targets)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.apply(ctx, digest, op); !errors.Is(err, stopped) {
		t.Fatalf("could not leave an rc.6-style pending plan: %v", err)
	}
	j, before, err := readJournal(f.State)
	if err != nil || j.Pending == nil {
		t.Fatalf("legacy journal: %+v, %v", j, err)
	}
	for _, plan := range []func(context.Context, []string) ([]VendorPlan, error){s.PlanInstall, s.PlanUninstall} {
		_, err := plan(ctx, []string{"codex"})
		var scope *PendingScopeError
		if !errors.As(err, &scope) || !slices.Equal(scope.Adapters, []string{"claude", "codex"}) ||
			scope.Action != "setup" || scope.Rules != (ClaudeRules{Read: true}) {
			t.Fatalf("expected original pending scope: %v", err)
		}
	}
	if _, after, err := readJournal(f.State); err != nil || before != after {
		t.Fatalf("scope refusal changed journal: %q -> %q (%v)", before, after, err)
	}
	resume, err := s.PlanInstall(ctx, []string{"codex", "claude"})
	if err != nil || len(resume) != 1 || !resume[0].Resume || len(resume[0].Adapters) != 2 {
		t.Fatalf("legacy resume: %+v, %v", resume, err)
	}
	var enabled []string
	s.SetPolicy = func(_ context.Context, adapter string, on bool) error {
		if on {
			enabled = append(enabled, adapter)
		}
		return nil
	}
	f.CheckTargets(t)
	if err := s.Install(ctx, resume[0]); err != nil {
		t.Fatal(err)
	}
	claude := configBytes(t, filepath.Join(f.Home, ".claude", "settings.json"))
	if !slices.Equal(enabled, []string{"claude", "codex"}) || !bytes.Contains(claude, claudeRules(false)[0].Value) ||
		bytes.Contains(claude, claudeRules(true)[0].Value) {
		t.Fatalf("resume changed the approved scope or rules: %v %s", enabled, claude)
	}
}

func TestScopedBlockedUninstallDisablesOnlySelectedVendor(t *testing.T) {
	f := setuptest.New(t, "codex")
	s := scopedService(f)
	ctx := t.Context()
	stopped := errors.New("fixture interrupted before policy")
	s.SetPolicy = func(context.Context, string, bool) error { return stopped }
	plans, err := s.PlanInstall(ctx, []string{"codex"})
	if err != nil || len(plans) != 1 || plans[0].Err != nil {
		t.Fatalf("plan: %+v, %v", plans, err)
	}
	f.CheckTargets(t)
	if err := s.Install(ctx, plans[0]); !errors.Is(err, stopped) {
		t.Fatalf("could not create pending scoped setup: %v", err)
	}
	before := configBytes(t, plans[0].Path)
	j, digest, err := readJournal(f.State)
	if err != nil || j.Pending == nil {
		t.Fatalf("interrupted journal: %+v, %v", j, err)
	}
	t.Setenv("CODEX_HOME", filepath.Join(f.Home, "other codex"))
	rm, err := s.PlanUninstall(ctx, []string{"codex"})
	if err != nil || len(rm) != 1 || !rm[0].Recovery || len(rm[0].Retained) != 2 {
		t.Fatalf("expected a scope-preserving recovery: %+v, %v", rm, err)
	}
	disabled := map[string]bool{}
	s.SetPolicy = func(_ context.Context, adapter string, on bool) error {
		disabled[adapter] = on
		return nil
	}
	f.CheckTargets(t)
	if err := s.Uninstall(ctx, rm[0]); err != nil {
		t.Fatal(err)
	}
	_, after, err := readJournal(f.State)
	if !reflect.DeepEqual(disabled, map[string]bool{"codex": false}) || err != nil || digest != after ||
		!bytes.Equal(configBytes(t, plans[0].Path), before) {
		t.Fatalf("blocked recovery touched another vendor or the journal: %+v (%v), digest changed=%t", disabled, err, digest != after)
	}
}

func TestScopedPlanEvidenceMatchesVendorBuildAndMode(t *testing.T) {
	f := setuptest.New(t, "claude", "codex")
	ctx := t.Context()
	f.CheckTargets(t)
	if err := os.Mkdir(f.State, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(ctx, f.State)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	platform := runtime.GOOS + "-" + runtime.GOARCH
	if err := db.Update(ctx, func(tx store.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO capability_evidence
			(vendor, vendor_version, platform, mode, manifest_rev, cap, level, result_ref, checked_at_ms) VALUES
			('claude', '2.1.327', ?1, 'print', 'claude-2', 'reminder', 'candidate', 'matching', 1),
			('claude', '2.1.327', ?1, 'print', 'other-rev', 'reminder', 'unavailable', 'revision', 1),
			('claude', '2.1.327', 'other-platform', 'print', 'claude-2', 'reminder', 'unavailable', 'platform', 1),
			('claude', '1.0.0', ?1, 'print', 'claude-2', 'reminder', 'unavailable', 'build', 1),
			('codex', '0.170.0', ?1, 'exec', 'codex-1', 'boundary', 'tested', 'vendor', 1)`, platform)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	plans, err := scopedService(f).PlanInstall(ctx, []string{"claude"})
	if err != nil || len(plans) != 1 || plans[0].Err != nil || len(plans[0].Files) != 1 {
		t.Fatalf("scoped preview: %+v, %v", plans, err)
	}
	file := plans[0].Files[0]
	if len(file.Evidence) != 1 || file.Evidence[0].Mode != "print" || file.Evidence[0].Level != "candidate" ||
		file.Reminder == nil || file.Reminder.Available || len(file.Reminder.Evidence) != 1 ||
		file.Reminder.Evidence[0].Level != "candidate" {
		t.Fatalf("out-of-scope evidence or capability: %+v, %+v", file.Evidence, file.Reminder)
	}
}
