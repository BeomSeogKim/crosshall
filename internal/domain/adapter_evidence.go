package domain

import (
	"context"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

// VerbAdapterEvidence records a conformance run's results.
const VerbAdapterEvidence = "owner adapter evidence"

// EvidenceResult is one capability's level as a conformance run observed it.
type EvidenceResult struct {
	Cap   string `json:"cap"`
	Level string `json:"level"`
	Check string `json:"check"`
}

// AdapterEvidence is one conformance run for an exact vendor build, platform,
// mode and manifest revision. ResultRef identifies the run's evidence file.
// Recording it is the owner's attestation of that run: the digest names the
// bytes, it does not prove where they came from.
type AdapterEvidence struct {
	Vendor        string           `json:"vendor"`
	VendorVersion string           `json:"vendor_version"`
	Platform      string           `json:"platform"`
	Mode          string           `json:"mode"`
	ManifestRev   string           `json:"manifest_rev"`
	RunAtMS       int64            `json:"run_at_ms"`
	Results       []EvidenceResult `json:"results"`
	ResultRef     string           `json:"result_ref"`
}

func (AdapterEvidence) command()     {}
func (AdapterEvidence) Verb() string { return VerbAdapterEvidence }

// AdapterEvidenceResult reports what was recorded.
type AdapterEvidenceResult struct {
	Vendor        string `json:"vendor"`
	VendorVersion string `json:"vendor_version"`
	Platform      string `json:"platform"`
	Mode          string `json:"mode"`
	ManifestRev   string `json:"manifest_rev"`
	Rows          int    `json:"rows"`
}

func (e AdapterEvidence) valid() bool {
	if !ValidAdapterName(e.Vendor) || e.RunAtMS <= 0 || len(e.Results) == 0 || len(e.Results) > 8 {
		return false
	}
	for _, v := range []string{e.VendorVersion, e.Platform, e.Mode, e.ManifestRev, e.ResultRef} {
		if v == "" || !validRef(v) {
			return false
		}
	}
	concrete := e.VendorVersion != "unverified" && e.Mode != "unverified"
	seen := map[string]bool{}
	for _, r := range e.Results {
		if seen[r.Cap] {
			return false // one result per capability
		}
		seen[r.Cap] = true
		// Tested needs a concrete build and mode and a stated observation.
		if r.Level == "tested" && (!concrete || r.Check == "") {
			return false
		}
		switch r.Cap {
		case "enroll", "observe", "boundary", "reminder", "turn_end", "publish":
		default:
			return false
		}
		switch r.Level {
		case "tested", "candidate", "unavailable":
		default:
			return false
		}
		if len(r.Check) > 200 || ValidText("check", r.Check) != "" {
			return false
		}
	}
	return true
}

func (s *Service) adapterEvidence(ctx context.Context, tx store.Tx, caller core.Caller, req Request, cmd AdapterEvidence, now time.Time) (core.Result, error) {
	if caller.Role != core.RoleOwner {
		return Refuse(core.CodeOwnerOnly, "only the owner can record conformance evidence", "run it from the owner's own shell, not an agent session"), nil
	}
	if req.OpKey == "" {
		return Fail(core.ExitUsage, "", "an operation key is required for this command", "run it through the crosshall command, which supplies the key"), nil
	}
	if !cmd.valid() {
		return Fail(core.ExitUsage, "", "the evidence file names an unknown vendor, capability or level, or misses a field", "crosshall owner adapter evidence <file>, with the file a conformance run wrote"), nil
	}
	owner, err := ownerPrincipal(ctx, tx, now)
	if err != nil {
		return core.Result{}, err
	}
	scope := "adapter-evidence:" + cmd.Vendor
	if result, found, err := replay(ctx, tx, owner, req.OpKey, scope, cmd); found || err != nil {
		return result, err
	}
	// A newer run replaces a capability's row; an older or identical run
	// changes nothing, so a stale file cannot roll evidence back.
	changed := 0
	for _, r := range cmd.Results {
		res, err := tx.ExecContext(ctx, `INSERT INTO capability_evidence
			(vendor, vendor_version, platform, mode, manifest_rev, cap, level, result_ref, checked_at_ms)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (vendor, vendor_version, platform, mode, manifest_rev, cap) DO UPDATE SET
			level = excluded.level, result_ref = excluded.result_ref, checked_at_ms = excluded.checked_at_ms,
			source = 'owner'
			WHERE excluded.checked_at_ms > capability_evidence.checked_at_ms`,
			cmd.Vendor, cmd.VendorVersion, cmd.Platform, cmd.Mode, cmd.ManifestRev, r.Cap, r.Level, cmd.ResultRef, cmd.RunAtMS)
		if err != nil {
			return core.Result{}, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return core.Result{}, err
		}
		if n != 0 && r.Cap == "publish" && r.Level == "tested" {
			if err := liftPublishBuildForEvidence(ctx, tx, cmd, now); err != nil {
				return core.Result{}, err
			}
		}
		changed += int(n)
	}
	out := AdapterEvidenceResult{Vendor: cmd.Vendor, VendorVersion: cmd.VendorVersion, Platform: cmd.Platform,
		Mode: cmd.Mode, ManifestRev: cmd.ManifestRev, Rows: changed}
	if changed > 0 {
		// Live members of this vendor now take the recorded levels.
		if err := refreshAdapterCapabilities(ctx, tx, cmd.Vendor, nil); err != nil {
			return core.Result{}, err
		}
		if err := audit(ctx, tx, "adapter_evidence", scope, owner, out, now); err != nil {
			return core.Result{}, err
		}
	}
	result, err := OK(out)
	if err != nil {
		return core.Result{}, err
	}
	return result, record(ctx, tx, owner, req.OpKey, scope, cmd, result, now)
}
