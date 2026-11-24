package spool

import (
	"cmp"
	"encoding/json"
	"os"
	"slices"

	"github.com/BeomSeogKim/crosshall/internal/wire"
)

type hookScope struct {
	digest, generation, adapter, launch string
	membership                          wire.Membership
}

type queuedHook struct {
	slot       int
	packet     serverPacket
	hookAtMS   int64
	publishSeq uint64
	turn       string
	phase      int
	dependent  bool
}

// orderHookPackets examines both the recovered work and new input. Moving
// packets within the same launcher and turn handles ties or clock steps
// without reordering ordinary commands or unrelated launchers.
func orderHookPackets(in, work *os.Root, packets []serverPacket) {
	if len(packets) < 2 {
		return
	}
	groups := make(map[hookScope][]queuedHook)
	for i, packet := range packets {
		dir := work
		if packet.claim {
			dir = in
		}
		scope, hook, ok := readQueuedHook(dir, packet)
		if ok {
			hook.slot = i
			groups[scope] = append(groups[scope], hook)
		}
	}
	for _, hooks := range groups {
		if len(hooks) < 2 {
			continue
		}
		slots := make([]int, len(hooks))
		for i := range hooks {
			slots[i] = hooks[i].slot
		}
		sortHookGroup(hooks)
		for i, hook := range hooks {
			packets[slots[i]] = hook.packet
		}
	}
}

func sortHookGroup(hooks []queuedHook) {
	slices.SortStableFunc(hooks, func(a, b queuedHook) int {
		if order := cmp.Compare(a.hookAtMS, b.hookAtMS); order != 0 {
			return order
		}
		if order := cmp.Compare(a.publishSeq, b.publishSeq); order != 0 {
			return order
		}
		return cmp.Compare(a.packet.id, b.packet.id)
	})
	turns := make(map[string][]int)
	for i, hook := range hooks {
		if hook.turn != "" && hook.phase != 0 {
			turns[hook.turn] = append(turns[hook.turn], i)
		}
	}
	// Apply the same-turn constraints after sorting by time. Comparing
	// phases and timestamps in one sort would permit comparison cycles
	// when turns overlap or the host clock steps backward.
	for _, indexes := range turns {
		ordered := make([]queuedHook, 0, len(indexes))
		for _, i := range indexes {
			ordered = append(ordered, hooks[i])
		}
		slices.SortStableFunc(ordered, func(a, b queuedHook) int {
			return cmp.Compare(a.phase, b.phase)
		})
		for i, slot := range indexes {
			hooks[slot] = ordered[i]
		}
	}
}

// hookPredecessors takes a fresh snapshot after a dependent hook is admitted.
// Every predecessor already published at admission is visible in in or work.
func hookPredecessors(in, work *os.Root, target serverPacket) ([]serverPacket, error) {
	target.claim = false
	scope, hook, ok := readQueuedHook(work, target)
	if !ok || !hook.dependent {
		return nil, nil
	}
	packets, err := readServerQueue(in, work)
	if err != nil {
		return nil, err
	}
	var hooks []queuedHook
	for _, packet := range packets {
		if packet.name == target.name && packet.claim {
			continue
		}
		dir := work
		if packet.claim {
			dir = in
		}
		candidateScope, candidate, ok := readQueuedHook(dir, packet)
		if ok && candidateScope == scope {
			hooks = append(hooks, candidate)
		}
	}
	sortHookGroup(hooks)
	var predecessors []serverPacket
	for _, hook := range hooks {
		if hook.packet.name == target.name {
			return predecessors, nil
		}
		predecessors = append(predecessors, hook.packet)
	}
	return nil, nil
}

func readQueuedHook(dir *os.Root, packet serverPacket) (hookScope, queuedHook, bool) {
	var scope hookScope
	hook := queuedHook{packet: packet}
	data, err := readServerPacket(dir, packet.name)
	if err != nil {
		return scope, hook, false
	}
	var req wire.Request
	if json.Unmarshal(data, &req) != nil || !req.IsHook() ||
		req.Version != wire.Version || req.Attempt != packet.id {
		return scope, hook, false
	}
	scope.digest, scope.generation = req.KeyDigest, req.Generation
	if req.Membership != nil {
		scope.membership = *req.Membership
	}
	hook.publishSeq = req.PublishSeq
	switch req.Verb {
	case wire.VerbHookEnroll:
		var args wire.HookEnrollArgs
		if json.Unmarshal(req.Args, &args) != nil {
			return scope, hook, false
		}
		scope.adapter, scope.launch, hook.hookAtMS = args.Profile.Adapter, args.LaunchRef, args.HookAtMS
		hook.dependent = true
	case wire.VerbHookEvent:
		var args wire.HookEventArgs
		if json.Unmarshal(req.Args, &args) != nil {
			return scope, hook, false
		}
		scope.adapter, scope.launch, hook.hookAtMS = args.Profile.Adapter, args.LaunchRef, args.HookAtMS
		hook.turn = args.TurnRef
		switch args.Fact {
		case wire.HookPromptSubmitted:
			hook.phase = 1
		case wire.HookPickupSeen:
			hook.phase = 2
			hook.dependent = true
		case wire.HookTurnFinishing:
			hook.phase = 3
			hook.dependent = true
		case wire.HookSessionEnded:
			hook.dependent = true
		}
	}
	return scope, hook, true
}
