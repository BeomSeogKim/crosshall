package domain

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/store"
)

// publishSuspendedEvent is the audit event of every publish suspension.
const publishSuspendedEvent = "publish_suspended"

// publishSuspended is the detail of a publish_suspended event. UntilMS is
// zero for a lasting suspension, which ends only with the launch or the
// owner's resume.
type publishSuspended struct {
	Channel    string `json:"channel"`
	Alias      string `json:"alias"`
	BindingID  int64  `json:"binding_id"`
	Suspension int64  `json:"suspension"`
	Notice     int64  `json:"notice"`
	Reason     string `json:"reason"`
	UntilMS    int64  `json:"until_ms,omitempty"`
	Lasting    bool   `json:"lasting"`
}

// recordSuspension writes the audit event of a suspension just inserted
// and, for a lasting one, the owner alert: the owner hears of it on the
// next owner command and on the desktop.
func recordSuspension(ctx context.Context, tx store.Tx, suspension int64, now time.Time) error {
	var d publishSuspended
	var until sql.NullInt64
	var principal int64
	if err := tx.QueryRowContext(ctx, `SELECT s.id, s.binding_id, s.notice, s.reason, s.until_ms, b.alias, b.principal_id, c.name
		FROM publish_suspension s JOIN binding b ON b.id = s.binding_id JOIN channel c ON c.id = b.channel_id
		WHERE s.id = ?`, suspension).Scan(&d.Suspension, &d.BindingID, &d.Notice, &d.Reason, &until, &d.Alias, &principal, &d.Channel); err != nil {
		return err
	}
	d.UntilMS, d.Lasting = until.Int64, !until.Valid
	if err := audit(ctx, tx, publishSuspendedEvent, "binding:"+strconv.FormatInt(d.BindingID, 10), principal, d, now); err != nil {
		return err
	}
	if !d.Lasting {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO owner_alert (event_seq) VALUES (last_insert_rowid())`)
	return err
}

// open holds while this lasting suspension is what keeps the member's
// notices off: not lifted by the owner or by a new launch, and the member
// live with notices otherwise on.
func (d publishSuspended) open(ctx context.Context, tx store.Tx, now time.Time) (bool, error) {
	state, found, err := store.ReadPublishState(ctx, tx, d.BindingID, now)
	return found && state.RecoveryHint.SuspensionID == d.Suspension, err
}

func (d publishSuspended) reason() string { return strings.ReplaceAll(d.Reason, "_", " ") }

func (d publishSuspended) desktopAlert(seq int64) DesktopAlert {
	return DesktopAlert{Seq: seq, Title: "crosshall: notices off in #" + d.Channel,
		Body: fmt.Sprintf("Notices to @%s stay off until its session restarts: notice %d %s, the third suspension. Run: crosshall owner publish resume %d",
			d.Alias, d.Notice, d.reason(), d.BindingID)}
}

func (d publishSuspended) ownerLines(atMS int64) []string {
	return []string{
		fmt.Sprintf("crosshall: notices to @%s in #%s are off until its session restarts (third suspension, notice %d %s at %s).",
			d.Alias, d.Channel, d.Notice, d.reason(), time.UnixMilli(atMS).Format("15:04")),
		fmt.Sprintf("next: crosshall owner publish resume %d turns them on again.", d.BindingID),
	}
}
