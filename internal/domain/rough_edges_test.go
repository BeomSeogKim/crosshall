package domain

import (
	"reflect"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/core"
)

func TestOwnerLogPagesLikeTheMemberLog(t *testing.T) {
	f, a, _, _ := channel3(t)
	var ids []int64
	for i := 0; i < 5; i++ {
		ids = append(ids, decode[NoteResult](t, f.run(a, Note{To: []string{"tax"}, Body: "n" + itoa(int64(i))}, "")).ID)
	}
	before := f.count(`SELECT COUNT(*) FROM recipient WHERE picked_up_at_ms IS NOT NULL`)

	latest := decode[LogResult](t, f.owner(OwnerLog{Channel: "checkout", Limit: 2}, ""))
	if got := logIDs(latest); !reflect.DeepEqual(got, ids[3:]) || latest.Earlier != 3 || latest.Next == nil || *latest.Next != 0 {
		t.Fatalf("latest page %v earlier %d next %v", got, latest.Earlier, latest.Next)
	}
	first := decode[LogResult](t, f.owner(OwnerLog{Channel: "checkout", Since: latest.Next, Limit: 2}, ""))
	if got := logIDs(first); !reflect.DeepEqual(got, ids[:2]) || first.Later != 3 || first.Next == nil || *first.Next != ids[1] {
		t.Fatalf("first page %v later %d next %v", got, first.Later, first.Next)
	}
	rest := decode[LogResult](t, f.owner(OwnerLog{Channel: "checkout", Since: first.Next}, ""))
	if got := logIDs(rest); !reflect.DeepEqual(got, ids[2:]) || rest.Later != 0 {
		t.Fatalf("rest %v later %d", got, rest.Later)
	}
	if after := f.count(`SELECT COUNT(*) FROM recipient WHERE picked_up_at_ms IS NOT NULL`); after != before {
		t.Fatal("owner log recorded a pickup")
	}
	negative := int64(-1)
	for _, bad := range []OwnerLog{{Channel: "checkout", Limit: -1}, {Channel: "checkout", Limit: MaxLogLimit + 1}, {Channel: "checkout", Since: &negative}} {
		wantCode(t, f.owner(bad, ""), core.ExitUsage, "")
	}
}

func TestMemberStatusShowsOwnRootTokensAndExchanges(t *testing.T) {
	f, a, b, c := channel3(t)
	f.roots(a, 2)
	f.roots(c, 1)
	x1 := decode[AskResult](t, f.ask(a, Ask{To: []string{"tax"}, Body: "one", New: true}, "a1"))
	x2 := decode[AskResult](t, f.ask(c, Ask{To: []string{"form", "tax"}, Body: "two", New: true}, "c1"))
	if x1.Exchange == x2.Exchange {
		t.Fatalf("one exchange for two roots: %+v %+v", x1, x2)
	}

	for _, tc := range []struct {
		s      session
		alias  string
		tokens int64
		want   []ExchangeBalance
	}{
		{a, "form", 1, []ExchangeBalance{{ID: x1.Exchange, Granted: 8, Spent: 1, Remaining: 7}, {ID: x2.Exchange, Granted: 8, Spent: 1, Remaining: 7}}},
		{b, "tax", 0, []ExchangeBalance{{ID: x1.Exchange, Granted: 8, Spent: 1, Remaining: 7}, {ID: x2.Exchange, Granted: 8, Spent: 1, Remaining: 7}}},
		{c, "a11y", 0, []ExchangeBalance{{ID: x2.Exchange, Granted: 8, Spent: 1, Remaining: 7}}},
	} {
		got := decode[StatusResult](t, f.run(tc.s, Status{}, "")).Self
		if got == nil || got.Alias != tc.alias || got.RootTokens != tc.tokens || !reflect.DeepEqual(got.Exchanges, tc.want) {
			t.Errorf("@%s: self %+v, want %d token(s) and %+v", tc.alias, got, tc.tokens, tc.want)
		}
	}
	if self := decode[StatusResult](t, f.owner(OwnerStatus{}, "")).Self; self != nil {
		t.Fatalf("the owner ledger carries a member's budget: %+v", self)
	}
}

func TestMemberStatusBudgetStaysInItsChannel(t *testing.T) {
	f, a, _, _ := channel3(t)
	f.roots(a, 1)
	wantOK(t, f.ask(a, Ask{To: []string{"tax"}, Body: "one", New: true}, "a1"))
	d := f.newSession("claude")
	wantOK(t, f.join(d, Join{Channel: "inventory", Alias: "stock"}, ""))
	self := decode[StatusResult](t, f.run(d, Status{}, "")).Self
	if self == nil || self.Alias != "stock" || self.RootTokens != 0 || len(self.Exchanges) != 0 {
		t.Fatalf("a member of another channel sees %+v", self)
	}
}
