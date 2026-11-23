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

func TestMemberStatusShowsHighestThreeOwnRounds(t *testing.T) {
	f, a, b, c := channel3(t)
	for i := int64(1); i <= 4; i++ {
		root := f.guardAsk(a, Ask{To: []string{"tax"}, Body: "question"}, "")
		executeGuardFixture(t, f, `UPDATE message SET level_ms = ? WHERE id = ?`, ms(f.now)+i*roundDurationMS, root.ID)
	}
	self := decode[StatusResult](t, f.run(b, Status{}, "")).Self
	if self == nil || self.Alias != "tax" || self.Round != 4 || self.RoundLimit != 8 || len(self.Exchanges) != 3 {
		t.Fatalf("reader rounds: %+v", self)
	}
	for index, round := range self.Exchanges {
		if round.Round != int64(4-index) || round.RoundLimit != 8 {
			t.Fatalf("rounds were not ordered: %+v", self.Exchanges)
		}
	}
	if unrelated := decode[StatusResult](t, f.run(c, Status{}, "")).Self; unrelated.Round != 0 || len(unrelated.Exchanges) != 0 {
		t.Fatalf("unrelated member sees rounds: %+v", unrelated)
	}
	if self := decode[StatusResult](t, f.owner(OwnerStatus{}, "")).Self; self != nil {
		t.Fatalf("owner ledger carries a member's rounds: %+v", self)
	}
}

func TestMemberStatusRoundsStayInItsChannel(t *testing.T) {
	f, a, _, _ := channel3(t)

	wantOK(t, f.ask(a, Ask{To: []string{"tax"}, Body: "one", New: true}, "a1"))
	d := f.newSession("claude")
	wantOK(t, f.join(d, Join{Channel: "inventory", Alias: "stock"}, ""))
	self := decode[StatusResult](t, f.run(d, Status{}, "")).Self
	if self == nil || self.Alias != "stock" || self.Round != 0 || len(self.Exchanges) != 0 {
		t.Fatalf("a member of another channel sees %+v", self)
	}
}
