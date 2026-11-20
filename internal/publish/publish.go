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

// Queue is the vendor process a notice is written to: its PID and start
// time.
type Queue struct {
	PID     int64
	StartMS int64
}

// Target is where one notice goes.
type Target struct {
	Adapter string
	Address string
	Queue   Queue
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
	// Prove identifies the process that owns a route's address, before
	// any notice is reserved for it; ok is false without that proof.
	Prove func(ctx context.Context, target Target) (queue Queue, ok bool)
	// Send writes one notice to a proven target.
	Send func(context.Context, Target, string) Outcome
	// Retired requires proof the queue is gone.
	Retired func(context.Context, Queue) bool
}

// Route is what a vendor's hooks registered for one member session.
type Route struct {
	Mode       string
	SocketPath string
	ThreadID   string
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
	// SessionThread is the thread address an enrolled session's reference
	// names, empty when the adapter registers its address another way.
	SessionThread(sessionRef string) string
	// Notice renders the notice for message IDs; ok is false while the
	// adapter has no notice format.
	Notice(ids []int64) (line string, ok bool)
	// Echo recognizes the adapter's own notice in a submitted prompt and
	// returns the ID list it carries; the shared grammar in internal/notice
	// validates that list and writes the receipt.
	Echo(prompt string) (ids string, ok bool)
	// Transport is the adapter's sender, without Send while it has none.
	Transport() Transport
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
