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
	var profile wire.HookProfile
	var launch, event string
	var epoch uint64
	switch req.Verb {
	case wire.VerbHookEnroll:
		var args wire.HookEnrollArgs
		if err := json.Unmarshal(req.Args, &args); err != nil {
			return nil, commandError(core.ExitUsage, "", "invalid hook enrollment"), nil
		}
		profile, launch, event, epoch = args.Profile, args.LaunchRef, args.EventRef, args.Epoch
	case wire.VerbHookEvent:
		var args wire.HookEventArgs
		if err := json.Unmarshal(req.Args, &args); err != nil {
			return nil, commandError(core.ExitUsage, "", "invalid hook event"), nil
		}
		profile, launch, event, epoch = args.Profile, args.LaunchRef, args.EventRef, args.Epoch
	}
	if arrival.Owner || r.Caller.Role == core.RoleOwner || r.Vendor != profile.Adapter ||
		(profile.Adapter != "claude" && profile.Adapter != "codex") {
		return nil, commandError(core.ExitRefused, "", "hook identity does not match the member exchange"), nil
	}
	if req.Verb == wire.VerbHookEvent && r.Caller.Role != core.RoleMember {
		return nil, commandError(core.ExitNotEnrolled, core.CodeNotEnrolled, "hook event requires a live member"), nil
	}
	if launch == "" || len(launch) > 128 || strings.Contains(launch, ":") || event == "" || len(event) > 256 ||
		req.OpKey != "hook:"+profile.Adapter+":"+launch+":"+event {
		return nil, commandError(core.ExitUsage, "", "invalid hook operation key"), nil
	}
	policy, err := store.ReadAdapterPolicy(ctx, tx, profile.Adapter)
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
