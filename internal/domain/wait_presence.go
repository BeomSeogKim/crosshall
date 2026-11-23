package domain

import (
	"context"
	"sync"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/store"
)

// WaitRunning is a send line's next step for a recipient whose own wait is
// polling and accepts the message: it returns within about a second. It is a
// fact about the wait, not a promise that a model turn follows.
const WaitRunning = "its wait is running"

// waitFresh is how recent a binding's last wait poll must be for its wait to
// count as running. A wait polls at most one second apart.
const waitFresh = 3 * time.Second

// waitPresence remembers, in memory only, each binding's last wait poll that
// left its wait running. A dispatcher restart ends every wait, so nothing is
// stored.
type waitPresence struct {
	mu    sync.Mutex
	polls map[int64]waitPoll
}

type waitPoll struct {
	at         time.Time
	workOnly   bool
	generation int64
}

// saw records a poll that leaves the binding's wait running, and forgets
// polls that are no longer fresh.
func (w *waitPresence) saw(binding, generation int64, workOnly bool, now time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for id, p := range w.polls {
		if !fresh(p, now) {
			delete(w.polls, id)
		}
	}
	if w.polls == nil {
		w.polls = map[int64]waitPoll{}
	}
	w.polls[binding] = waitPoll{at: now, workOnly: workOnly, generation: generation}
}

// forget drops the binding's poll: its wait has ended.
func (w *waitPresence) forget(binding int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.polls, binding)
}

// running reports whether the binding has a running wait whose filter
// accepts a message of kind: a note does not end a --work-only wait. An
// empty kind asks for any running wait. A nil presence knows of no wait.
func (w *waitPresence) running(ctx context.Context, tx store.Tx, binding int64, kind string, now time.Time) (bool, error) {
	if w == nil {
		return false, nil
	}
	w.mu.Lock()
	p, ok := w.polls[binding]
	w.mu.Unlock()
	if !ok || !fresh(p, now) || p.workOnly && kind == "note" {
		return false, nil
	}
	var current bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM binding b JOIN member_selection s ON s.principal_id = b.principal_id
		WHERE b.id = ? AND b.state = 'live' AND s.binding_id = b.id AND s.generation = ?)`, binding, p.generation).Scan(&current)
	return current, err
}

func fresh(p waitPoll, now time.Time) bool {
	age := now.Sub(p.at)
	return age >= 0 && age <= waitFresh
}
