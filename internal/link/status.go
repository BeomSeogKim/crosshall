package link

import "time"

// Preview describes the home before the user confirms the enrollment.
type Preview struct {
	Target, Name, LocalRelease, HomeRelease string
	Version                                 int
	ClockOffsetMS                           *int64
}

// Status is the spoke's current connection and the last negotiated peer.
type Status struct {
	Target        string `json:"target,omitempty"`
	State         string `json:"state"`
	HomeRelease   string `json:"home_release,omitempty"`
	Version       int    `json:"link_version,omitempty"`
	ClockOffsetMS *int64 `json:"clock_offset_ms,omitempty"`
	Error         string `json:"error,omitempty"`
}

// EstimateClockOffset compares the home challenge's clock with the midpoint
// of the local exchange. The estimate is for display only.
func EstimateClockOffset(start time.Time, homeMS int64) *int64 {
	if homeMS == 0 {
		return nil
	}
	offset := time.UnixMilli(homeMS).Sub(start.Add(time.Since(start) / 2)).Milliseconds()
	return &offset
}
