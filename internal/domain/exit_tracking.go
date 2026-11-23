package domain

import (
	"context"

	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

type ExitTracking string

const (
	ExitByProcess ExitTracking = "process"
	ExitByHooks   ExitTracking = "hooks"
	ExitUntracked ExitTracking = "untracked"
)

func (t ExitTracking) Description() string {
	switch t {
	case ExitByProcess:
		return "ends with its process"
	case ExitByHooks:
		return "ends by its hooks"
	default:
		return "exit not tracked"
	}
}

func (s *Service) exitTracking(ctx context.Context, tx store.Tx, principal int64) (ExitTracking, wire.Host, error) {
	var ref, adapter, revision, namespace string
	var enabled, launchProcess bool
	var host wire.Host
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(l.launch_ref, ''),
		COALESCE(json_extract(l.profile, '$.adapter'), ''),
		COALESCE(json_extract(l.profile, '$.manifest_rev'), ''),
		COALESCE(ap.enabled, 0), COALESCE(l.pid_namespace, ''),
		COALESCE(l.verified = 1 AND l.launch_ref GLOB 'pid-*'
			AND json_extract(l.profile, '$.adapter') = 'claude', 0),
		COALESCE(h.pid, 0), COALESCE(h.start_identity, 0), COALESCE(h.pid_namespace, '')
		FROM principal p LEFT JOIN member_host h ON h.principal_id = p.id
		LEFT JOIN adapter_launch l ON l.rowid = (
			SELECT rowid FROM adapter_launch WHERE principal_id = p.id AND ended_hook_at_ms IS NULL AND fenced = 0
			ORDER BY started_hook_at_ms DESC, rowid DESC LIMIT 1)
		LEFT JOIN adapter_policy ap ON ap.adapter = json_extract(l.profile, '$.adapter') WHERE p.id = ?`, principal).
		Scan(&ref, &adapter, &revision, &enabled, &namespace, &launchProcess, &host.PID, &host.Start, &host.NS)
	if err != nil {
		return "", wire.Host{}, err
	}
	if ref != "" {
		if enabled && s.HasSessionEnd != nil && s.HasSessionEnd(adapter, revision) {
			return ExitByHooks, host, nil
		}
		if launchProcess && s.LaunchPIDNamespace != nil && namespace == s.LaunchPIDNamespace() {
			return ExitByProcess, host, nil
		}
	}
	// A launch that neither a hook nor the process sweep will end, such as
	// Antigravity's startup launch, leaves the member to its host, as the
	// sweep does.
	if s.judgeableHost(&host) {
		return ExitByProcess, host, nil
	}
	return ExitUntracked, host, nil
}
