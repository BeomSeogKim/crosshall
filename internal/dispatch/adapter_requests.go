package dispatch

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

func (s *server) hookContext(ctx context.Context, tx store.Tx, arrival Arrival, req wire.Request, r Resolution) (*domain.AdapterContext, core.Result, error) {
	var adapter, launch, event string
	var epoch uint64
	var recoverEnrollment bool
	switch req.Verb {
	case wire.VerbHookEnroll:
		var args wire.HookEnrollArgs
		if err := json.Unmarshal(req.Args, &args); err != nil {
			return nil, commandError(core.ExitUsage, "", "invalid hook enrollment"), nil
		}
		adapter, launch, event, epoch = args.Profile.Adapter, args.LaunchRef, args.EventRef, args.Epoch
	case wire.VerbHookEvent:
		var args wire.HookEventArgs
		if err := json.Unmarshal(req.Args, &args); err != nil {
			return nil, commandError(core.ExitUsage, "", "invalid hook event"), nil
		}
		adapter, launch, event, epoch = args.Profile.Adapter, args.LaunchRef, args.EventRef, args.Epoch
		recoverEnrollment = !req.OneWayHook() && args.CanRecoverEnrollment()
	case wire.VerbHookRoute:
		var args wire.HookRouteArgs
		if err := json.Unmarshal(req.Args, &args); err != nil {
			return nil, commandError(core.ExitUsage, "", "invalid hook route"), nil
		}
		adapter, launch, event, epoch = args.Adapter, args.LaunchRef, args.EventRef, args.Epoch
	}
	if arrival.Owner || r.Caller.Role == core.RoleOwner || r.Vendor != adapter ||
		!domain.ValidAdapterName(adapter) {
		return nil, commandError(core.ExitRefused, "", "hook identity does not match the member exchange"), nil
	}
	if (req.Verb == wire.VerbHookEvent || req.Verb == wire.VerbHookRoute) && r.Caller.Role != core.RoleMember &&
		(!recoverEnrollment || r.Caller.PrincipalID != nil) {
		return nil, commandError(core.ExitNotEnrolled, core.CodeNotEnrolled, "hook event requires a live member"), nil
	}
	wantOpKey := "hook:" + adapter + ":" + launch + ":" + event
	if req.Verb == wire.VerbHookRoute {
		wantOpKey = "hook:route:" + adapter + ":" + launch + ":" + event
	}
	if launch == "" || len(launch) > 128 || strings.Contains(launch, ":") || event == "" || len(event) > 256 ||
		req.OpKey != wantOpKey {
		return nil, commandError(core.ExitUsage, "", "invalid hook operation key"), nil
	}
	policy, err := store.ReadAdapterPolicy(ctx, tx, adapter)
	if err != nil {
		return nil, core.Result{}, err
	}
	if !policy.Enabled || epoch != policy.Epoch {
		return nil, commandError(core.ExitRefused, "", "adapter policy is off or has changed"), nil
	}
	stamp, _ := s.adapterStamp(arrival.Dir, req.Generation)
	return &domain.AdapterContext{
		Instance: s.ready.Instance, Generation: req.Generation,
		NoticeSeq: stamp.NoticeSeq, ReminderSeq: stamp.ReminderSeq,
	}, core.Result{}, nil
}
