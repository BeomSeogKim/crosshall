// Package publish is the seam between crosshall's publish core and the
// vendors it publishes through. The core keeps switches, claims, budget,
// timing and receipts; an Adapter supplies only what is vendor-specific:
// which registered routes can carry a notice, the notice's text and the
// transport that delivers it.
package publish

import (
	"context"
	"slices"
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

// Transport delivers notices for one adapter. Both callbacks get a context
// canceled at the deadline; a transport that runs a child process should
// kill it then. The publisher does not rely on that: past the deadline a
// send is recorded uncertain and a probe as not retired, and the worker
// waits a bounded time for the callback to return. Retired requires proof
// the queue is gone. A zero Transport means the adapter has no sender.
type Transport struct {
	Send    func(context.Context, Target, string) Outcome
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
	// Complete reports whether a registered route names an address the
	// adapter can deliver to, and why not.
	Complete(Route) (ok bool, reason string)
	// ModeEligible reports whether a session in this permission mode may
	// receive notices.
	ModeEligible(mode string) bool
	// AcceptsSocket reports whether a reported socket path may be stored.
	AcceptsSocket(path string) bool
	// SessionThread is the thread address an enrolled session's reference
	// names, empty when the adapter registers its address another way.
	SessionThread(sessionRef string) string
	// Notice renders the notice for message IDs; ok is false while the
	// adapter has no notice format, and its routes are never eligible.
	Notice(ids []int64) (line string, ok bool)
	// Transport is the adapter's sender, zero while it has none.
	Transport() Transport
}

// Registry maps an adapter name to its Adapter.
type Registry map[string]Adapter

var shipped = Registry{"claude": claude{}, "codex": codex{}}

// Shipped is the registry this build ships.
func Shipped() Registry { return shipped }

// Lookup finds a shipped adapter.
func Lookup(name string) (Adapter, bool) {
	a, ok := shipped[name]
	return a, ok
}

// Publishing names the adapters with a notice format, sorted.
func (r Registry) Publishing() []string {
	var names []string
	for name, a := range r {
		if _, ok := a.Notice(nil); ok {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

// Sends reports whether any adapter has a sender.
func (r Registry) Sends() bool {
	for _, a := range r {
		if a.Transport().Send != nil {
			return true
		}
	}
	return false
}

// WithTransport returns a copy of r in which the named adapter delivers
// through t; the rest of the adapter is unchanged.
func (r Registry) WithTransport(name string, t Transport) Registry {
	out := make(Registry, len(r))
	for n, a := range r {
		out[n] = a
	}
	out[name] = withTransport{Adapter: r[name], transport: t}
	return out
}

type withTransport struct {
	Adapter
	transport Transport
}

func (w withTransport) Transport() Transport { return w.transport }
