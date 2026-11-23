package dispatch

import (
	"context"
	"errors"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/telemetry"
)

const (
	notifyEnv     = "CROSSHALL_NOTIFY"
	notifyTimeout = 5 * time.Second
)

// notifier shows a desktop notification for a round stop. Only release
// builds show one, so tests and development halls stay quiet; the store
// reads, the claim and the command run in a worker, never inside a request
// or the dispatcher loop.
type notifier struct {
	db *store.Store
	// path is empty when no notification can be shown.
	path    string
	args    func(title, body string) []string
	timeout time.Duration
	running atomic.Bool
	workers sync.WaitGroup
}

func notifyOff() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv(notifyEnv)), "off")
}

// notifyCommand names the operating system's own notification command.
// The text is passed as arguments, so no alias or channel name becomes
// part of a script.
func notifyCommand(goos string) (string, func(title, body string) []string) {
	switch goos {
	case "darwin":
		return "/usr/bin/osascript", func(title, body string) []string {
			return []string{"-e", "on run argv", "-e", "display notification (item 1 of argv) with title (item 2 of argv)",
				"-e", "end run", body, title}
		}
	case "linux":
		return "/usr/bin/notify-send", func(title, body string) []string {
			return []string{"--app-name=crosshall", title, body}
		}
	}
	return "", nil
}

func newNotifier(db *store.Store, version string, off bool, path string, args func(title, body string) []string) *notifier {
	n := &notifier{db: db, args: args, timeout: notifyTimeout}
	if _, err := os.Stat(path); err == nil && path != "" && !off && telemetry.AppVersion(version) != "dev" {
		n.path = path
	}
	return n
}

func (n *notifier) recover(ctx context.Context) error {
	return n.db.Update(ctx, func(tx store.Tx) error { return domain.FailUnfinishedDesktopAlerts(ctx, tx) })
}

// tick starts the worker that decides the pending stops and runs the
// command; while one runs it does nothing. It never touches the store
// itself, because the dispatcher loop calls it between scans and a long
// read can hold the shared reader.
func (n *notifier) tick(ctx context.Context, now time.Time) {
	if !n.running.CompareAndSwap(false, true) {
		return
	}
	n.workers.Add(1)
	go func() {
		defer n.workers.Done()
		defer n.running.Store(false)
		n.work(ctx, now)
	}()
}

func (n *notifier) work(ctx context.Context, now time.Time) {
	var pending bool
	err := n.db.View(ctx, func(tx store.Tx) error {
		var err error
		pending, err = domain.PendingDesktopAlerts(ctx, tx)
		return err
	})
	var alerts []domain.DesktopAlert
	if err == nil && pending {
		err = n.db.Update(ctx, func(tx store.Tx) error {
			var err error
			alerts, err = domain.ClaimDesktopAlerts(ctx, tx, now, n.path != "")
			return err
		})
	}
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("dispatcher: desktop notification: %v", err)
		}
		return
	}
	for _, alert := range alerts {
		sent := n.show(ctx, alert) == nil
		// A write lost to shutdown leaves the row sending, which the
		// next start records as failed.
		err := n.db.Update(ctx, func(tx store.Tx) error { return domain.FinishDesktopAlert(ctx, tx, alert.Seq, sent) })
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("dispatcher: desktop notification: %v", err)
		}
	}
}

func (n *notifier) show(ctx context.Context, alert domain.DesktopAlert) error {
	ctx, cancel := context.WithTimeout(ctx, n.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, n.path, n.args(alert.Title, alert.Body)...)
	cmd.Dir = "/"
	cmd.WaitDelay = time.Second
	return cmd.Run()
}

func (n *notifier) wait() { n.workers.Wait() }
