package store

import (
	"strings"

	"github.com/BeomSeogKim/crosshall/internal/hook/manifests"
)

// ProcessLaunchProfile holds when the launch profile in column (a JSON
// HookProfile) is of a manifest revision that declares process_launch: the
// session's process is named by the launch, so the process sweep may end
// the launch when that process is gone. A launch of any other revision,
// unknown ones included, has no exit authority however its reference
// reads.
func ProcessLaunchProfile(column string) string {
	byAdapter := map[string][]string{}
	var adapters []string
	for _, r := range manifests.RevisionsWith(func(c manifests.Capabilities) bool { return c.ProcessLaunch }) {
		if byAdapter[r.Adapter] == nil {
			adapters = append(adapters, r.Adapter)
		}
		byAdapter[r.Adapter] = append(byAdapter[r.Adapter], r.Revision)
	}
	if len(adapters) == 0 {
		return "0"
	}
	terms := make([]string, len(adapters))
	for i, adapter := range adapters {
		terms[i] = "(" + adapterIn("json_extract("+column+", '$.adapter')", []string{adapter}) + " AND " +
			adapterIn("json_extract("+column+", '$.manifest_rev')", byAdapter[adapter]) + ")"
	}
	return "(" + strings.Join(terms, " OR ") + ")"
}
