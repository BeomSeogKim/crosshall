// Package publish is the seam between crosshall's publish core and the
// vendors it publishes through. The core keeps switches, claims, budget,
// timing and receipts; an Adapter supplies only what is vendor-specific:
// which registered routes can carry a notice and where to, how its notice
// reads and how its echo is recognized, and the transport that proves and
// delivers.
package publish

import (
	"context"
	"slices"
	"sync"
)

// Queue is the vendor process a notice is written to: its PID and its
// start identity, a kernel value every reader gets the same for the life
// of the process (proc.Start). It is stored and compared at every send.
type Queue struct {
	PID     int64
	StartMS int64
}

// Proven is a transport's proof of a listener: its queue, and the
// process's wall-clock start estimate, used only to check that it started
// no later than the launch's first hook.
type Proven struct {
	Queue       Queue
	StartedAtMS int64
}

// Target is where one notice goes.
type Target struct {
	Adapter string
	Address string
	Queue   Queue
	// Home is the vendor's configuration home setup installed the hooks
	// into (as Probe.InstalledHome), never a path a hook reported; empty
	// when none is recorded.
	Home string
	// Attempt is the attempt's stable identity, unique across the store's
	// life: a transport may tag the notice with it to find it again.
	Attempt string
}

// Outcome is what a transport reports for one notice.
type Outcome string

const (
	NotSubmitted Outcome = "not_submitted"
	Submitted    Outcome = "submitted"
	Uncertain    Outcome = "uncertain"
)

// Transport proves and delivers for one adapter. Each callback gets a
// context canceled at the deadline; a transport that runs a child process
// should kill it then. The publisher does not rely on that: past the
// deadline a proof fails, a send is recorded uncertain and a probe counts
// as not retired, and the worker waits a bounded time for the callback to
// return. A Transport without Send means the adapter has no sender.
type Transport struct {
	// Prove identifies the process that listens on a route's address,
	// before any notice is reserved for it; ok is false without that
	// proof. The core records it only when that process is the probe's
	// launch, started no later than the launch's first hook.
	Prove func(ctx context.Context, probe Probe) (proven Proven, ok bool)
	// Send writes one notice to a proven target.
	Send func(context.Context, Target, string) Outcome
	// Retired requires proof the queue is gone.
	Retired func(context.Context, Queue) bool
	// Withdraw, for a transport whose queue outlives its process, takes a
	// submitted notice back before it runs. The target names the notice's
	// thread, home and attempt; its Queue is the server it was written to,
	// which may be gone: the transport proves whichever server now holds
	// the home. Nil means notices are never withdrawn.
	Withdraw func(context.Context, Target) Withdrawal
	// ConfirmApproval, for an adapter whose approval fact may be a request
	// the vendor approves on its own, asks the vendor whether the target's
	// thread now waits on its owner's approval. It runs in the publisher
	// only, never in a hook. Nil means the hook fact is all there is.
	ConfirmApproval func(context.Context, Target) Approval
}

// Approval is what a vendor says about a pending approval.
type Approval string

const (
	// ApprovalShown: the thread waits on its owner's approval.
	ApprovalShown Approval = "shown"
	// ApprovalAbsent: it does not; the request was answered or approved
	// by the vendor itself.
	ApprovalAbsent Approval = "absent"
	// ApprovalUnknown: no proven server answered.
	ApprovalUnknown Approval = "unknown"
)

// Withdrawal is what a transport reports for one withdrawal. Only Deleted
// settles the notice; the others leave it to its receipt.
type Withdrawal string

const (
	// Deleted: the notice was pending and is gone; it will not run.
	Deleted Withdrawal = "deleted"
	// NotPending: no such pending notice; it ran, or may still run.
	NotPending Withdrawal = "not_pending"
	// Unreachable: no proven server answered; nothing is known.
	Unreachable Withdrawal = "unreachable"
)

// Probe is what a transport proves: the resolved target (its Queue is not
// known yet), the registered route, and the launch expected to own it.
type Probe struct {
	Target
	Route           Route
	LaunchRef       string
	StartedHookAtMS int64
	// InstalledHome is the vendor's configuration home that setup installed
	// this state root's hooks into, as setup recorded it; empty when none is
	// recorded.
	InstalledHome string
}

// Modes of a launch whose vendor runs a managed server, as its hooks report
// them from the process that runs them.
const (
	ModeDaemon    = "daemon"     // the session is a client of the managed server
	ModeInProcess = "in_process" // the session runs its server itself
)

// Route is what a vendor's hooks registered for one member session. Host
// is the server process that held the session's thread at its start, and
// Home the vendor home that server serves, as that start named them; only
// a thread-routed adapter registers them.
type Route struct {
	Mode       string
	SocketPath string
	ThreadID   string
	Host       Queue
	Home       string
}

// Adapter is one vendor's part of publish.
type Adapter interface {
	// Resolve gives the address a registered route delivers to, or why it
	// cannot carry a notice (no address, or a permission mode on pull).
	// Selection, status and the final fence all use it.
	Resolve(Route) (address, reason string)
	// ModeEligible reports whether a session in this permission mode may
	// receive notices.
	ModeEligible(mode string) bool
	// AcceptsSocket reports whether a reported socket path may be stored.
	AcceptsSocket(path string) bool
	// SessionRoute is the route an enrolled session registers: the thread
	// its reference names, with the launch's mode, server and home as its
	// start reported them (launch), or nothing when the adapter registers
	// its route another way.
	SessionRoute(sessionRef string, launch Route) Route
	// ThreadRouted reports whether the adapter's routes address a session's
	// thread on the server that hosts it (Route.Host, Route.Home) rather
	// than the launch's own process. Such a route needs no verified launch:
	// its transport proves that server, and its notices queue per thread
	// of that home.
	ThreadRouted() bool
	// Reach says why the adapter's sender cannot reach the session that
	// registered a route at all, whatever its address: the owner's reason,
	// empty when it can.
	Reach(Route) string
	// Notice renders the notice for message IDs; ok is false while the
	// adapter has no notice format.
	Notice(ids []int64) (line string, ok bool)
	// ResumeNotice renders the notice that names requests a member still
	// owes after the hall restarted, which it read before; Echo recognizes
	// it too.
	ResumeNotice(ids []int64) (line string, ok bool)
	// Echo recognizes the adapter's own notice in a submitted prompt and
	// returns the ID list it carries; the shared grammar in internal/notice
	// validates that list and writes the receipt.
	Echo(prompt string) (ids string, ok bool)
	// Transport is the adapter's sender, without Send while it has none.
	Transport() Transport
	// BackgroundWakes reports whether the agent starts its next turn when a
	// background command it ran exits, so an idle session can stand by on a
	// background crosshall wait at no model cost. A session of an adapter
	// without it is told to end its turn instead of running a wait.
	BackgroundWakes() bool
}

// Registry maps an adapter name to its Adapter.
type Registry map[string]Adapter

var shipped = Registry{"claude": claude{}, "codex": codex{}}

var (
	mu      sync.RWMutex
	current = shipped
)

// Shipped is the registry this build ships.
func Shipped() Registry { return shipped }

// Adapters is the one registry selection, status, doctor, the publisher
// and the hooks all read, so they agree on what can publish. It is the
// shipped registry outside tests.
func Adapters() Registry {
	mu.RLock()
	defer mu.RUnlock()
	return current
}

// Swap makes r the registry Adapters returns until restore is called. It
// exists for tests; the build itself never swaps.
func Swap(r Registry) (restore func()) {
	mu.Lock()
	previous := current
	current = r
	mu.Unlock()
	return func() {
		mu.Lock()
		current = previous
		mu.Unlock()
	}
}

// Lookup finds an adapter in Adapters.
func Lookup(name string) (Adapter, bool) { return Adapters().Lookup(name) }

// Lookup finds an adapter in r.
func (r Registry) Lookup(name string) (Adapter, bool) {
	a, ok := r[name]
	return a, ok
}

// Names lists the registered adapters, sorted.
func (r Registry) Names() []string {
	names := make([]string, 0, len(r))
	for name := range r {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// Senders lists, sorted, the adapters that can publish: a notice format and
// a transport that sends. Only their routes are selected and reserved.
func (r Registry) Senders() []string {
	var names []string
	for _, name := range r.Names() {
		if r.Sends(name) {
			names = append(names, name)
		}
	}
	return names
}

// Sends reports whether the named adapter can publish.
func (r Registry) Sends(name string) bool {
	a, ok := r[name]
	if !ok || a.Transport().Send == nil {
		return false
	}
	_, ok = a.Notice(nil)
	return ok
}

// Address resolves a registered route through its adapter: the address a
// notice goes to, or why the route cannot carry one now.
func (r Registry) Address(adapter string, route Route) (address, reason string) {
	if !r.Sends(adapter) {
		return "", "no sender for " + adapter
	}
	if why := r[adapter].Reach(route); why != "" {
		return "", why
	}
	return r[adapter].Resolve(route)
}

// With returns a copy of r that also holds a under name.
func (r Registry) With(name string, a Adapter) Registry {
	out := make(Registry, len(r)+1)
	for n, existing := range r {
		out[n] = existing
	}
	out[name] = a
	return out
}

// WithTransport returns a copy of r in which the named adapter delivers
// through t; the rest of the adapter is unchanged.
func (r Registry) WithTransport(name string, t Transport) Registry {
	return r.With(name, withTransport{Adapter: r[name], transport: t})
}

type withTransport struct {
	Adapter
	transport Transport
}

func (w withTransport) Transport() Transport { return w.transport }
