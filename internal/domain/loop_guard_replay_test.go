package domain

import (
	"cmp"
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

type replayMessage struct {
	id, channel, sender, exchange, parent, handoff, at int64
	kind, alias                                        string
	recipients                                         int
	root, level                                        int64
}

type replayObligation struct {
	message *replayMessage
	settled sql.NullInt64
}

type replayEvent struct {
	message    *replayMessage
	at, reader int64
}

type replayMember struct {
	any     int64
	threads map[int64]int64
	owed    []replayObligation
	sends   []*replayMessage
}

func (m *replayMember) fold(msg *replayMessage) {
	m.any = max(m.any, msg.level)
	if msg.root != 0 {
		m.threads[msg.root] = max(m.threads[msg.root], msg.level)
	}
}

func (m *replayMember) base(root, at int64) int64 {
	position := m.threads[root]
	if root == 0 {
		position = m.any
	}
	for _, owed := range m.owed {
		if (!owed.settled.Valid || owed.settled.Int64 > at) && (root == 0 || owed.message.root == root) {
			position = max(position, owed.message.level)
		}
	}
	return position
}

func (m *replayMember) path(msg *replayMessage) int64 {
	var roots, inExchange []int64
	for _, owed := range m.owed {
		if owed.message.channel != msg.channel || owed.settled.Valid && owed.settled.Int64 <= msg.at {
			continue
		}
		root := owed.message.root
		if !slices.Contains(roots, root) {
			roots = append(roots, root)
		}
		if owed.message.exchange == msg.exchange && !slices.Contains(inExchange, root) {
			inExchange = append(inExchange, root)
		}
	}
	if len(roots) == 1 {
		return roots[0]
	}
	if len(inExchange) == 1 {
		return inExchange[0]
	}
	return 0
}

func TestLoopGuardReplayHistory(t *testing.T) {
	path := os.Getenv("CROSSHALL_REPLAY_DB")
	if path == "" {
		t.Skip("set CROSSHALL_REPLAY_DB to a scratch backup")
	}
	path, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	uri := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro&immutable=1"}
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rows, err := db.QueryContext(t.Context(), `SELECT m.id, m.channel_id, m.sender_id, m.kind,
		COALESCE(m.exchange_id, 0), COALESCE(m.in_reply_to, 0), COALESCE(e.parent_request_id, 0),
		m.created_at_ms, COALESCE(b.alias, '')
		FROM message m LEFT JOIN exchange e ON e.root_message_id = m.id
		LEFT JOIN binding b ON b.channel_id = m.channel_id AND b.principal_id = m.sender_id`)
	if err != nil {
		t.Fatal(err)
	}
	var events []replayEvent
	messages := map[int64]*replayMessage{}
	for rows.Next() {
		msg := new(replayMessage)
		if err := rows.Scan(&msg.id, &msg.channel, &msg.sender, &msg.kind, &msg.exchange,
			&msg.parent, &msg.handoff, &msg.at, &msg.alias); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		messages[msg.id] = msg
		events = append(events, replayEvent{message: msg, at: msg.at})
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 {
		t.Fatal("replay copy contains no messages")
	}

	rows, err = db.QueryContext(t.Context(), `SELECT message_id, principal_id, picked_up_at_ms FROM recipient`)
	if err != nil {
		t.Fatal(err)
	}
	pickups := map[[2]int64]int64{}
	for rows.Next() {
		var id, principal int64
		var at sql.NullInt64
		if err := rows.Scan(&id, &principal, &at); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		if at.Valid {
			pickups[[2]int64{id, principal}] = at.Int64
			events = append(events, replayEvent{message: messages[id], at: at.Int64, reader: principal})
		}
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	type addObligation struct {
		principal int64
		owed      replayObligation
	}
	rows, err = db.QueryContext(t.Context(), `SELECT message_id, principal_id, settled_at_ms FROM obligation`)
	if err != nil {
		t.Fatal(err)
	}
	obligations := map[int64][]addObligation{}
	for rows.Next() {
		var id, principal int64
		var settled sql.NullInt64
		if err := rows.Scan(&id, &principal, &settled); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		msg := messages[id]
		msg.recipients++
		obligations[id] = append(obligations[id], addObligation{principal, replayObligation{msg, settled}})
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	slices.SortFunc(events, func(a, b replayEvent) int {
		return cmp.Or(cmp.Compare(a.at, b.at), cmp.Compare(a.message.id, b.message.id), cmp.Compare(a.reader, b.reader))
	})
	members := map[int64]*replayMember{}
	memberFor := func(id int64) *replayMember {
		if members[id] == nil {
			members[id] = &replayMember{threads: map[int64]int64{}}
		}
		return members[id]
	}
	byReason, byDay := map[string]int{}, map[string]int{}
	var evaluated int
	var highestRound, highestRequests, highestAnswers int64
	for _, event := range events {
		msg := event.message
		if event.reader != 0 {
			memberFor(event.reader).fold(msg)
			continue
		}
		sender := memberFor(msg.sender)
		switch msg.kind {
		case "note":
			msg.level = sender.any
		case "reply":
			parent := messages[msg.parent]
			msg.root, msg.level = parent.root, parent.level
			pickup, read := pickups[[2]int64{msg.parent, msg.sender}]
			if !read || pickup > msg.at {
				sender.fold(parent)
			}
		case "ask":
			root := sender.path(msg)
			base := sender.base(root, msg.at)
			if msg.handoff != 0 {
				parent := messages[msg.handoff]
				base = max(sender.base(0, msg.at), sender.base(parent.root, msg.at), parent.level)
				root = 0
			}
			for len(sender.sends) > 0 && sender.sends[0].at <= msg.at-roundDurationMS {
				sender.sends = sender.sends[1:]
			}
			answers := msg.recipients
			for _, previous := range sender.sends {
				answers += previous.recipients
			}
			decision := evaluateGuard(msg.at, base, guardDefaultRounds, int64(len(sender.sends)+1), int64(answers), msg.recipients)
			evaluated++
			highestRound = max(highestRound, roundAt(decision.LevelMS, msg.at))
			highestRequests = max(highestRequests, decision.Requests)
			highestAnswers = max(highestAnswers, decision.OwedAnswers)
			if decision.Reason != "" {
				byReason[decision.Reason]++
				day := time.UnixMilli(msg.at).Local().Format(time.DateOnly)
				byDay[day+" "+decision.Reason]++
				t.Logf("would refuse id=%d alias=%q reason=%s round=%d requests=%d answers=%d",
					msg.id, msg.alias, decision.Reason, decision.Round, decision.Requests, decision.OwedAnswers)
			}
			if root == 0 {
				root = msg.id
			}
			msg.root, msg.level = root, decision.LevelMS
			sender.sends = append(sender.sends, msg)
			for _, item := range obligations[msg.id] {
				member := memberFor(item.principal)
				member.owed = append(member.owed, item.owed)
			}
		}
	}
	t.Logf("period=%s to %s evaluated=%d refusals_by_reason=%v refusals_by_day=%v highest_round=%d highest_requests=%d highest_answers=%d",
		time.UnixMilli(events[0].at).Local().Format(time.RFC3339), time.UnixMilli(events[len(events)-1].at).Local().Format(time.RFC3339),
		evaluated, byReason, byDay, highestRound, highestRequests, highestAnswers)
}
