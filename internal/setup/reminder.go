package setup

import "github.com/BeomSeogKim/crosshall/internal/store"

type reminderStatus struct {
	ReleaseState       string             `json:"release_state"`
	Available          bool               `json:"available"`
	Reason             string             `json:"reason"`
	ExtraModelRequests int                `json:"extra_model_requests"`
	Provenance         string             `json:"provenance"`
	Evidence           []reminderEvidence `json:"evidence"`
}

type reminderEvidence struct {
	Version     string `json:"version,omitempty"`
	Platform    string `json:"platform,omitempty"`
	Mode        string `json:"mode,omitempty"`
	ManifestRev string `json:"manifest_rev,omitempty"`
	Level       string `json:"level"`
	Reason      string `json:"reason,omitempty"`
}

// The release stays parked independently of any imported evidence. Each
// result names the tested mode without assuming which mode the owner runs.
func releaseReminderStatus(adapter, version, platform, revision string, imported []store.DoctorEvidence) reminderStatus {
	status := reminderStatus{
		ReleaseState: "parked", Reason: "This release has no reminder opt-in.",
		ExtraModelRequests: 1, Provenance: "Fixed crosshall text sent to the model.",
	}
	if version == "" || version == unknown {
		status.Evidence = []reminderEvidence{{Level: "unknown"}}
		return status
	}
	measured := installerFor(adapter).reminderMeasurement()
	hasMeasurement := measured.Version == version && measured.Platform == platform && measured.ManifestRev == revision && measured.Mode != ""
	for _, row := range imported {
		if row.Capability != "reminder" || row.Version != version || row.Platform != platform || row.ManifestRev != revision {
			continue
		}
		if row.Level == "unavailable" && (row.Mode == "" || row.Mode == "unverified" || row.Mode == unknown) {
			continue
		}
		profile := reminderEvidence{Version: row.Version, Platform: row.Platform, Mode: row.Mode, ManifestRev: row.ManifestRev, Level: row.Level}
		if row.Level == "unavailable" {
			profile.Reason = "The imported conformance result marks this profile unavailable."
		}
		if hasMeasurement && row.Mode == measured.Mode {
			if row.Level == measured.Level {
				profile.Reason = measured.Reason
			}
			hasMeasurement = false
		}
		status.Evidence = append(status.Evidence, profile)
	}
	if hasMeasurement {
		status.Evidence = append(status.Evidence, measured)
	}
	if len(status.Evidence) == 0 {
		status.Evidence = []reminderEvidence{{Level: "unknown"}}
	}
	return status
}
