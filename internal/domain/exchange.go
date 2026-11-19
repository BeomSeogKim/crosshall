package domain

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

// DefaultCredit is the credit a new exchange starts with.
const DefaultCredit = 8

// exchangeRow is the budget state of one exchange.
type exchangeRow struct {
	id, granted, spent int64
	episodeOpen        bool
}

func (e exchangeRow) balance() int64 { return e.granted - e.spent }

// attribution is where an ask's debit goes: an existing exchange, or a new
// one opened with a root token.
type attribution struct {
	exchange exchangeRow
	isNew    bool
}

// member is the caller's live binding, resolved for a member verb.
type member struct {
	principal, binding, channel int64
	channelName, alias          string
	context                     sql.NullInt64
}

// liveMember resolves the caller to its live binding, or returns a result to
// send back instead (not enrolled, archived channel).
func liveMember(ctx context.Context, tx store.Tx, caller core.Caller) (member, *core.Result, error) {
	var m member
	if caller.Role != core.RoleMember || caller.PrincipalID == nil {
		r := notEnrolled()
		return m, &r, nil
	}
	var state string
	err := tx.QueryRowContext(ctx,
		`SELECT b.id, b.channel_id, c.name, c.state, b.alias, b.context_exchange_id
		 FROM binding b JOIN channel c ON c.id = b.channel_id
		 WHERE b.principal_id = ? AND b.state = 'live' ORDER BY b.id LIMIT 1`, *caller.PrincipalID).
		Scan(&m.binding, &m.channel, &m.channelName, &state, &m.alias, &m.context)
	if errors.Is(err, sql.ErrNoRows) {
		r := notEnrolled()
		return m, &r, nil
	}
	if err != nil {
		return m, nil, err
	}
	m.principal = *caller.PrincipalID
	if state != "active" {
		r := Refuse(core.CodeArchived, fmt.Sprintf("#%s is archived", m.channelName), "")
		return m, &r, nil
	}
	return m, nil, nil
}

func loadExchange(ctx context.Context, tx store.Tx, id, channel int64) (exchangeRow, bool, error) {
	e := exchangeRow{id: id}
	err := tx.QueryRowContext(ctx,
		`SELECT granted, spent, episode_open FROM exchange WHERE id = ? AND channel_id = ?`, id, channel).
		Scan(&e.granted, &e.spent, &e.episodeOpen)
	if errors.Is(err, sql.ErrNoRows) {
		return e, false, nil
	}
	return e, err == nil, err
}

// openObligationExchanges lists the exchanges in which the caller owes an
// answer, with one open ask of each for the hint.
func openObligationExchanges(ctx context.Context, tx store.Tx, m member) (map[int64]int64, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT msg.exchange_id, MIN(msg.id) FROM obligation o JOIN message msg ON msg.id = o.message_id
		 WHERE o.principal_id = ? AND o.state = 'open' AND msg.channel_id = ? AND msg.exchange_id IS NOT NULL
		 GROUP BY msg.exchange_id`, m.principal, m.channel)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int64{}
	for rows.Next() {
		var ex, ask int64
		if err := rows.Scan(&ex, &ask); err != nil {
			return nil, err
		}
		out[ex] = ask
	}
	return out, rows.Err()
}

// attribute decides which exchange an ask belongs to. The first rule that
// applies wins: an explicit --re, an explicit --new, the exchange where the
// caller owes an answer, the exchange it last took part in, else a new one.
// A non-nil result is a refusal to return instead.
func attribute(ctx context.Context, tx store.Tx, m member, cmd Ask) (attribution, *core.Result, error) {
	owed, err := openObligationExchanges(ctx, tx, m)
	if err != nil {
		return attribution{}, nil, err
	}

	if cmd.Re != 0 {
		var exchange sql.NullInt64
		var sender int64
		err := tx.QueryRowContext(ctx,
			`SELECT exchange_id, sender_id FROM message WHERE id = ? AND channel_id = ?`, cmd.Re, m.channel).
			Scan(&exchange, &sender)
		if errors.Is(err, sql.ErrNoRows) {
			r := Refuse(core.CodeNotFound, fmt.Sprintf("no message #%d in #%s", cmd.Re, m.channelName), "")
			return attribution{}, &r, nil
		}
		if err != nil {
			return attribution{}, nil, err
		}
		party := sender == m.principal
		if !party {
			var n int
			if err := tx.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM recipient WHERE message_id = ? AND principal_id = ?`, cmd.Re, m.principal).Scan(&n); err != nil {
				return attribution{}, nil, err
			}
			party = n > 0
		}
		if !exchange.Valid || !party {
			r := Refuse(core.CodeBadRoot,
				fmt.Sprintf("#%d is not a request you sent or received", cmd.Re), "")
			return attribution{}, &r, nil
		}
		if _, ok := owed[exchange.Int64]; len(owed) > 0 && !ok {
			r := Refuse(core.CodeBadRoot,
				fmt.Sprintf("#%d belongs to x%d, but you owe answers elsewhere", cmd.Re, exchange.Int64),
				reHints(owed))
			return attribution{}, &r, nil
		}
		return existing(ctx, tx, m, exchange.Int64)
	}

	if cmd.New {
		return newRoot(ctx, tx, m, owed)
	}

	switch len(owed) {
	case 0:
	case 1:
		for ex := range owed {
			return existing(ctx, tx, m, ex)
		}
	default:
		r := Refuse(core.CodeAmbiguousRoot,
			"you owe answers in more than one exchange; say which one this request belongs to",
			reHints(owed))
		return attribution{}, &r, nil
	}

	if m.context.Valid {
		if e, ok, err := loadExchange(ctx, tx, m.context.Int64, m.channel); err != nil || ok {
			return attribution{exchange: e}, nil, err
		}
	}
	return newRoot(ctx, tx, m, owed)
}

func existing(ctx context.Context, tx store.Tx, m member, id int64) (attribution, *core.Result, error) {
	e, ok, err := loadExchange(ctx, tx, id, m.channel)
	if err != nil {
		return attribution{}, nil, err
	}
	if !ok {
		return attribution{}, nil, fmt.Errorf("domain: exchange %d missing", id)
	}
	return attribution{exchange: e}, nil, nil
}

// newRoot checks that the caller may open a new exchange: nothing owed, no
// request addressed to it left unread (even one already answered or
// cancelled), and a root token.
func newRoot(ctx context.Context, tx store.Tx, m member, owed map[int64]int64) (attribution, *core.Result, error) {
	if len(owed) > 0 {
		r := Refuse(core.CodeMustAnswerFirst,
			"you owe an answer; settle it, or continue that exchange", reHints(owed))
		return attribution{}, &r, nil
	}
	var unread sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT MIN(r.message_id) FROM recipient r JOIN message msg ON msg.id = r.message_id
		 WHERE r.principal_id = ? AND r.owes = 1 AND r.picked_up_at_ms IS NULL AND msg.channel_id = ?`,
		m.principal, m.channel).Scan(&unread); err != nil {
		return attribution{}, nil, err
	}
	if unread.Valid {
		r := Refuse(core.CodeMustAnswerFirst,
			fmt.Sprintf("request #%d addressed to you is still unread", unread.Int64),
			fmt.Sprintf("crosshall read %d", unread.Int64))
		return attribution{}, &r, nil
	}
	var tokens int64
	if err := tx.QueryRowContext(ctx, `SELECT root_tokens FROM principal WHERE id = ?`, m.principal).Scan(&tokens); err != nil {
		return attribution{}, nil, err
	}
	if tokens < 1 {
		r := Refuse(core.CodeNoRootCredit,
			"you have no credit to start a new exchange",
			fmt.Sprintf("ask the owner: crosshall owner budget grant %s/@%s --roots 1", m.channelName, m.alias))
		return attribution{}, &r, nil
	}
	return attribution{exchange: exchangeRow{granted: DefaultCredit}, isNew: true}, nil, nil
}

func reHints(owed map[int64]int64) string {
	var hints []string
	for ex, ask := range owed {
		hints = append(hints, fmt.Sprintf("x%d: --re %d", ex, ask))
	}
	sortStrings(hints)
	return strings.Join(hints, "; ")
}

// charge checks that the exchange can pay for one request of n units. An ask
// costs one unit, so a shortfall means a zero balance: it opens an exhaustion
// episode, once, and returns the refusal with the ways on.
func charge(ctx context.Context, tx store.Tx, m member, a attribution, n int64, now time.Time) (*core.Result, error) {
	if n <= a.exchange.balance() {
		return nil, nil
	}
	if !a.exchange.episodeOpen {
		if _, err := tx.ExecContext(ctx,
			`UPDATE exchange SET episode_open = 1, episode_count = episode_count + 1 WHERE id = ?`, a.exchange.id); err != nil {
			return nil, err
		}
		if err := audit(ctx, tx, "budget_exhausted", fmt.Sprintf("exchange:%d", a.exchange.id), m.principal,
			map[string]int64{"needed": n}, now); err != nil {
			return nil, err
		}
	}
	r := Refuse(core.CodeBudgetExhausted,
		fmt.Sprintf("x%d has no units left; a request costs %d", a.exchange.id, n),
		fmt.Sprintf("open a new exchange with crosshall ask --new (needs a root token), ask the owner to run "+
			"crosshall owner budget grant %s/x%d <units>, or send a crosshall reply or crosshall note, which cost nothing",
			m.channelName, a.exchange.id))
	return &r, nil
}

// open creates the exchange for a new root, spends the caller's root token,
// and records both. The root message is linked by finish once it exists.
func open(ctx context.Context, tx store.Tx, m member, now time.Time) (int64, error) {
	if _, err := tx.ExecContext(ctx,
		`UPDATE principal SET root_tokens = root_tokens - 1 WHERE id = ? AND root_tokens > 0`, m.principal); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO budget_event (principal_id, delta, reason, by_id, at_ms) VALUES (?, -1, 'roots', ?, ?)`,
		m.principal, m.principal, ms(now)); err != nil {
		return 0, err
	}
	// The root message does not exist yet; a unique negative placeholder
	// satisfies the deferred foreign key until it is set below.
	res, err := tx.ExecContext(ctx,
		`INSERT INTO exchange (channel_id, root_message_id, opened_by_id, granted)
		 VALUES (?, -1 - (SELECT COALESCE(MAX(id), 0) FROM exchange), ?, ?)`,
		m.channel, m.principal, DefaultCredit)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO budget_event (exchange_id, delta, reason, by_id, at_ms) VALUES (?, ?, 'initial', ?, ?)`,
		id, DefaultCredit, m.principal, ms(now))
	return id, err
}

// debit spends n units of the exchange for message msg.
func debit(ctx context.Context, tx store.Tx, m member, exchange, msg, n int64, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `UPDATE exchange SET spent = spent + ? WHERE id = ?`, n, exchange); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO budget_event (exchange_id, delta, reason, message_id, by_id, at_ms) VALUES (?, ?, 'debit', ?, ?, ?)`,
		exchange, -n, msg, m.principal, ms(now))
	return err
}

func setContext(ctx context.Context, tx store.Tx, binding, exchange int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE binding SET context_exchange_id = ? WHERE id = ?`, exchange, binding)
	return err
}
