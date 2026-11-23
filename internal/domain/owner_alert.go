package domain

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/store"
)

const (
	desktopAlertMaxAge = 10 * time.Minute
	desktopAlertGap    = time.Minute
	ownerLineStops     = 3
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
// PendingDesktopAlerts reports whether a stop waits for its desktop
// decision. It only reads, so the dispatcher can ask every second without
// taking the write lock.
func PendingDesktopAlerts(ctx context.Context, tx store.Tx) (bool, error) {
	var pending bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM owner_alert WHERE desktop_state = 'pending')`).Scan(&pending)
	return pending, err
}

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

// ownerAlertLines marks every stop not yet shown as shown, and returns
// lines for the first three that are still open. The command's own effect
// is already applied, so a grant never reports the stop it closed.
// bestEffortOwnerAlerts returns the owner line, or none when building it
// fails: the line is an extra, so its error is logged and its own writes
// are undone without failing or rolling back the owner's command.
func bestEffortOwnerAlerts(ctx context.Context, tx store.Tx, now time.Time) []string {
	if _, err := tx.ExecContext(ctx, `SAVEPOINT owner_alert_lines`); err != nil {
		log.Printf("dispatcher: owner line: %v", err)
		return nil
	}
	lines, err := ownerAlertLines(ctx, tx, now)
	if err != nil {
		log.Printf("dispatcher: owner line: %v", err)
		lines = nil
		if _, err := tx.ExecContext(ctx, `ROLLBACK TO owner_alert_lines`); err != nil {
			log.Printf("dispatcher: owner line: %v", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `RELEASE owner_alert_lines`); err != nil {
		log.Printf("dispatcher: owner line: %v", err)
	}
	return lines
}

func ownerAlertLines(ctx context.Context, tx store.Tx, now time.Time) ([]string, error) {
	stops, err := alertedStops(ctx, tx, `a.shown_at_ms IS NULL`)
	if err != nil || len(stops) == 0 {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE owner_alert SET shown_at_ms = ? WHERE shown_at_ms IS NULL`, ms(now)); err != nil {
		return nil, err
	}
	var lines []string
	open := 0
	for _, stop := range stops {
		if !stop.open {
			continue
		}
		if open++; open > ownerLineStops {
			continue
		}
		more, err := stop.ownerLines(ctx, tx)
		if err != nil {
			return nil, err
		}
		lines = append(lines, more...)
	}
	if open > ownerLineStops {
		lines = append(lines, fmt.Sprintf("and %d more: crosshall owner status", open-ownerLineStops))
	}
	return lines, nil
}

func (s alertedStop) ownerLines(ctx context.Context, tx store.Tx) ([]string, error) {
	d := s.detail
	at := time.UnixMilli(s.atMS).Format("15:04")
	refused := fmt.Sprintf("request to %s was refused: it would be round %d of %d.", RecipientList(d.Recipients), d.Round, d.RoundLimit)
	if raw, ok := strings.CutPrefix(s.subject, "exchange:"); ok {
		exchange, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, err
		}
		return []string{
			fmt.Sprintf("crosshall: round limit reached in #%s on x%d at %s. @%s's %s", d.Channel, exchange, at, d.Sender, refused),
			fmt.Sprintf("next: crosshall owner status --channel %s shows the thread. %s lets it continue.",
				d.Channel, StopGrant(d.Channel, GuardStop{ExchangeID: exchange, RoundLimit: d.RoundLimit})),
		}, nil
	}
	member := GuardStop{RoundLimit: d.RoundLimit}
	if err := tx.QueryRowContext(ctx, `SELECT b.id, b.alias, b.state FROM binding b JOIN channel c ON c.id = b.channel_id
        WHERE b.principal_id = CAST(substr(?, 11) AS INTEGER) AND c.name = ?`, s.subject, d.Channel).
		Scan(&member.BindingID, &member.Alias, &member.BindingState); err != nil {
		return nil, err
	}
	return []string{
		fmt.Sprintf("crosshall: round limit reached in #%s for @%s's new threads at %s. Its %s", d.Channel, d.Sender, at, refused),
		fmt.Sprintf("next: crosshall owner status --channel %s shows it. %s lets @%s open threads again.",
			d.Channel, StopGrant(d.Channel, member), member.Alias),
	}, nil
}

// StopGrant is the command that lets a stopped thread or member continue:
// a member that is not live is named by its binding. A stop under a zero
// limit suggests the default, since a zero grant would not lift it.
func StopGrant(channel string, stop GuardStop) string {
	rounds := stop.RoundLimit
	if rounds == 0 {
		rounds = guardDefaultRounds
	}
	target := fmt.Sprintf("%s/x%d", channel, stop.ExchangeID)
	if stop.ExchangeID == 0 && stop.BindingState == "live" {
		target = channel + "/@" + stop.Alias
	} else if stop.ExchangeID == 0 {
		target = strconv.FormatInt(stop.BindingID, 10)
	}
	return fmt.Sprintf("crosshall owner budget grant %s --rounds %d", target, rounds)
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
