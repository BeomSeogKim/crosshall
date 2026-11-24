package domain

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/store"
)

// publishHeldEvent records that a submitted notice has had no receipt for
// publishHeldAlertAfter: its member gets no further notice until it is
// observed or released, so the owner hears of it once.
const publishHeldEvent = "publish_held"

const publishHeldAlertAfter = 15 * time.Minute

// publishHeld is the detail of a publish_held event.
type publishHeld struct {
	Channel   string `json:"channel"`
	Alias     string `json:"alias"`
	BindingID int64  `json:"binding_id"`
	Notice    int64  `json:"notice"`
	SinceMS   int64  `json:"since_ms"`
}

// AlertLongHolds writes one publish_held event and owner alert for each
// live member's notice submitted at least publishHeldAlertAfter ago that
// is still neither observed nor released.
func AlertLongHolds(ctx context.Context, tx store.Tx, now time.Time) error {
	rows, err := tx.QueryContext(ctx, `SELECT a.id, a.binding_id, a.principal_id, COALESCE(a.submitting_at_ms, a.reserved_at_ms), b.alias, c.name
		FROM publish_attempt a JOIN binding b ON b.id = a.binding_id JOIN channel c ON c.id = b.channel_id
		WHERE a.observed_at_ms IS NULL AND a.retired_at_ms IS NULL AND a.state IN ('submitting', 'submitted', 'uncertain') AND b.state = 'live'
			AND COALESCE(a.submitting_at_ms, a.reserved_at_ms) <= ?
			AND NOT EXISTS (SELECT 1 FROM owner_alert o JOIN event e ON e.seq = o.event_seq
				WHERE e.kind = ? AND e.subject = 'binding:' || a.binding_id AND json_extract(e.detail, '$.notice') = a.id)
		ORDER BY a.id`, ms(now.Add(-publishHeldAlertAfter)), publishHeldEvent)
	if err != nil {
		return err
	}
	type held struct {
		detail    publishHeld
		principal int64
	}
	var found []held
	for rows.Next() {
		var h held
		if err := rows.Scan(&h.detail.Notice, &h.detail.BindingID, &h.principal, &h.detail.SinceMS, &h.detail.Alias, &h.detail.Channel); err != nil {
			rows.Close()
			return err
		}
		found = append(found, h)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, h := range found {
		if err := audit(ctx, tx, publishHeldEvent, "binding:"+strconv.FormatInt(h.detail.BindingID, 10), h.principal, h.detail, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO owner_alert (event_seq) VALUES (last_insert_rowid())`); err != nil {
			return err
		}
	}
	return nil
}

// open holds while the notice still has no receipt and was not released,
// and its member is live.
func (d publishHeld) open(ctx context.Context, tx store.Tx) (bool, error) {
	var open bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM publish_attempt a JOIN binding b ON b.id = a.binding_id
		WHERE a.id = ? AND a.observed_at_ms IS NULL AND a.retired_at_ms IS NULL AND b.state = 'live')`, d.Notice).Scan(&open)
	return open, err
}

func (d publishHeld) since() string { return time.UnixMilli(d.SinceMS).Format("15:04") }

func (d publishHeld) desktopAlert(seq int64) DesktopAlert {
	return DesktopAlert{Seq: seq, Title: "crosshall: notices held in #" + d.Channel,
		Body: fmt.Sprintf("@%s gets no notices: notice %d has had no receipt since %s. Run: crosshall owner publish resume %d",
			d.Alias, d.Notice, d.since(), d.BindingID)}
}

func (d publishHeld) ownerLines() []string {
	return []string{
		fmt.Sprintf("crosshall: @%s in #%s gets no notices: notice %d has had no receipt since %s.", d.Alias, d.Channel, d.Notice, d.since()),
		fmt.Sprintf("next: crosshall owner publish resume %d releases it.", d.BindingID),
	}
}
