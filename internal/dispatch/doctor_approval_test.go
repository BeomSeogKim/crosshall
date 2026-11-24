package dispatch

import (
	"slices"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/store"
)

func TestDoctorWarnsOfEachMemberThatWaitsOnTheOwnersApproval(t *testing.T) {
	at := time.Date(2026, 11, 24, 21, 18, 0, 0, time.Local).UnixMilli()
	got := approvalWarnings([]store.DoctorApproval{{Alias: "opus", Channel: "dev", AtMS: at, Current: true},
		{Alias: "fable", Channel: "dev", AtMS: at}})
	want := []string{"@opus in #dev waits on your approval since 21:18: only you can answer it, at its session",
		"@fable in #dev asked you for approval at 21:18, no answer reported: look at its session"}
	if !slices.Equal(got, want) || approvalWarnings(nil) != nil {
		t.Fatalf("warnings %q, want %q", got, want)
	}
}
