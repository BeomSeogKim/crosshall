package domain

import "testing"

func TestRepoChannelNameIsAValidChannelName(t *testing.T) {
	for root, want := range map[string]string{
		"/work/crosshall": "crosshall",
		"/work/Shop API":  "shop-api",
		"/work/store.git": "store",
		"/work/__init__":  "init",
		"/work/한글":        "repo",
		"/work/a--b..c":   "a-b-c",
		"/work/x-0123456789-0123456789-0123456789-0123456789": "x-0123456789-0123456789-01234567",
	} {
		got := repoChannelName(root)
		if got != want || !ValidName(got) {
			t.Errorf("repoChannelName(%q) = %q, want %q (valid %v)", root, got, want, ValidName(got))
		}
	}
}
