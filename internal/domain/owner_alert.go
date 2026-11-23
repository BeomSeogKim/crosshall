package domain

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/store"
)

const (
	desktopAlertMaxAge = 10 * time.Minute
	desktopAlertGap    = time.Minute
)

// DesktopAlert is a round stop the dispatcher shows as a notification.
type DesktopAlert struct {
	Seq   int64
	Title string
	Body  string
}

type alertedStop struct {
	seq     int64
	subject string
	atMS    int64
	open    bool
	detail  guardStop
}

// A stop is open while its flag is set and no later stop replaced it.
const openStop = `COALESCE(CASE
        WHEN e.subject LIKE 'exchange:%' THEN (SELECT episode_open FROM exchange WHERE id = CAST(substr(e.subject, 10) AS INTEGER))
        ELSE (SELECT guard_told FROM principal WHERE id = CAST(substr(e.subject, 11) AS INTEGER)) END, 0) = 1
    AND NOT EXISTS (SELECT 1 FROM event later WHERE later.kind = e.kind AND later.subject = e.subject AND later.seq > e.seq)`

func alertedStops(ctx context.Context, tx store.Tx, where string) ([]alertedStop, error) {
	rows, err := tx.QueryContext(ctx, `SELECT a.event_seq, e.subject, e.at_ms, `+openStop+`, e.detail
        FROM owner_alert a JOIN event e ON e.seq = a.event_seq WHERE `+where+` ORDER BY a.event_seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []alertedStop
	for rows.Next() {
		var stop alertedStop
		var detail string
		if err := rows.Scan(&stop.seq, &stop.subject, &stop.atMS, &stop.open, &detail); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(detail), &stop.detail); err != nil {
			return nil, err
		}
		out = append(out, stop)
	}
	return out, rows.Err()
}

// ClaimDesktopAlerts decides every pending notification and returns the
// ones it marked sending. A stop that is closed, older than ten minutes or
// within a minute of the last notification is skipped; without a command
// the rest are unavailable.
func ClaimDesktopAlerts(ctx context.Context, tx store.Tx, now time.Time, available bool) ([]DesktopAlert, error) {
	stops, err := alertedStops(ctx, tx, `a.desktop_state = 'pending'`)
	if err != nil || len(stops) == 0 {
		return nil, err
	}
	var last *int64
	if err := tx.QueryRowContext(ctx, `SELECT MAX(desktop_at_ms) FROM owner_alert`).Scan(&last); err != nil {
		return nil, err
	}
	at := ms(now)
	var out []DesktopAlert
	for _, stop := range stops {
		state := "sending"
		switch {
		case !stop.open, at-stop.atMS > desktopAlertMaxAge.Milliseconds(), last != nil && at-*last < desktopAlertGap.Milliseconds():
			state = "skipped"
		case !available:
			state = "unavailable"
		}
		if state == "sending" {
			last = &at
			out = append(out, stop.desktopAlert())
		}
		if _, err := tx.ExecContext(ctx, `UPDATE owner_alert SET desktop_state = ?1,
            desktop_at_ms = CASE WHEN ?1 = 'sending' THEN ?2 END WHERE event_seq = ?3`, state, at, stop.seq); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// FinishDesktopAlert records whether the command exited 0.
func FinishDesktopAlert(ctx context.Context, tx store.Tx, seq int64, sent bool) error {
	state := "failed"
	if sent {
		state = "sent"
	}
	_, err := tx.ExecContext(ctx, `UPDATE owner_alert SET desktop_state = ? WHERE event_seq = ? AND desktop_state = 'sending'`, state, seq)
	return err
}

// FailUnfinishedDesktopAlerts runs when the dispatcher starts: a
// notification it may have shown before it stopped is not shown again.
func FailUnfinishedDesktopAlerts(ctx context.Context, tx store.Tx) error {
	_, err := tx.ExecContext(ctx, `UPDATE owner_alert SET desktop_state = 'failed' WHERE desktop_state = 'sending'`)
	return err
}

func (s alertedStop) desktopAlert() DesktopAlert {
	d := s.detail
	opening := fmt.Sprintf("@%s's new threads stopped. ", d.Sender)
	if exchange, ok := strings.CutPrefix(s.subject, "exchange:"); ok {
		opening = "x" + exchange + " stopped. "
	}
	return DesktopAlert{Seq: s.seq, Title: "crosshall: round limit in #" + d.Channel,
		Body: opening + fmt.Sprintf("@%s's request to %s would be round %d of %d. Run: crosshall owner status",
			d.Sender, RecipientList(d.Recipients), d.Round, d.RoundLimit)}
}

// RecipientList names up to three aliases and counts the rest.
func RecipientList(aliases []string) string {
	var named []string
	for _, alias := range aliases[:min(len(aliases), 3)] {
		named = append(named, "@"+alias)
	}
	switch {
	case len(aliases) > 3:
		return fmt.Sprintf("%s and %d more", strings.Join(named, ", "), len(aliases)-3)
	case len(named) > 1:
		return strings.Join(named[:len(named)-1], ", ") + " and " + named[len(named)-1]
	}
	return strings.Join(named, "")
}
