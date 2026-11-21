package dispatch

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/store"
)

// A default set, before repositories had one identity, in the main checkout
// of a repository whose Git directory lives apart is kept under the
// checkout's path. A starting dispatcher moves it to the repository's
// identity; a default already there stays, and other rows are untouched.
func TestStartupAdoptsOlderMainCheckoutDefaults(t *testing.T) {
	s, _ := adapterFilesFixture(t)
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	write := func(path, text string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	apart := func(name string) (checkout, metadata string) {
		checkout, metadata = filepath.Join(base, name), filepath.Join(base, name+"-metadata")
		write(filepath.Join(metadata, "HEAD"), "ref: refs/heads/main\n")
		write(filepath.Join(metadata, "objects", "info"), "")
		write(filepath.Join(metadata, "refs", "info"), "")
		write(filepath.Join(checkout, ".git"), "gitdir: "+metadata+"\n")
		return checkout, metadata
	}
	moved, movedTo := apart("moved")
	kept, keptAt := apart("kept")
	// The owner's older default meets a channel crosshall created since.
	owned, ownedAt := apart("owned")
	plain := filepath.Join(base, "plain")
	write(filepath.Join(plain, ".git", "HEAD"), "ref: refs/heads/main\n")
	link := func(name string) (worktree, admin string) {
		worktree, admin = filepath.Join(base, name), filepath.Join(movedTo, "worktrees", name)
		write(filepath.Join(admin, "HEAD"), "ref: refs/heads/side\n")
		write(filepath.Join(admin, "commondir"), "../..\n")
		write(filepath.Join(worktree, ".git"), "gitdir: "+admin+"\n")
		return worktree, admin
	}
	linked, _ := link("linked")
	// A linked worktree whose common directory cannot be read, or is lost,
	// must keep its own default: its repository is not known.
	locked, lockedAdmin := link("locked")
	if err := os.Chmod(filepath.Join(lockedAdmin, "commondir"), 0); err != nil {
		t.Fatal(err)
	}
	broken, brokenAdmin := link("broken")
	if err := os.Remove(filepath.Join(brokenAdmin, "commondir")); err != nil {
		t.Fatal(err)
	}
	gone := filepath.Join(base, "gone")

	ctx := t.Context()
	if err := s.db.Update(ctx, func(tx store.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO channel (id, name, state, created_at_ms)
			VALUES (1, 'old', 'active', 1), (2, 'new', 'active', 1)`); err != nil {
			return err
		}
		for root, channel := range map[string]int{moved: 1, kept: 1, keptAt: 2, owned: 1, plain: 1, linked: 2, locked: 2, broken: 2, gone: 1} {
			if _, err := tx.ExecContext(ctx, `INSERT INTO repo_default (repo_root, channel_id) VALUES (?, ?)`, root, channel); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO repo_default (repo_root, channel_id, automatic) VALUES (?, 2, 1)`, ownedAt)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for range 2 { // a second start changes nothing
		if err := s.adoptRepoDefaults(ctx); err != nil {
			t.Fatal(err)
		}
	}
	got := map[string]int{}
	if err := s.db.Update(ctx, func(tx store.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT repo_root, channel_id FROM repo_default`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var root string
			var channel int
			if err := rows.Scan(&root, &channel); err != nil {
				return err
			}
			got[root] = channel
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	want := map[string]int{movedTo: 1, keptAt: 2, ownedAt: 1, plain: 1, linked: 2, locked: 2, broken: 2, gone: 1}
	if len(got) != len(want) {
		t.Fatalf("defaults %v, want %v", got, want)
	}
	for root, channel := range want {
		if got[root] != channel {
			t.Fatalf("defaults %v, want %v", got, want)
		}
	}
	// The adopted default is the owner's, not an automatic one any more.
	var automatic, adopted int
	if err := s.db.Update(ctx, func(tx store.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT (SELECT automatic FROM repo_default WHERE repo_root = ?),
			(SELECT COUNT(*) FROM event WHERE kind = 'channel_default_adopted')`, ownedAt).Scan(&automatic, &adopted)
	}); err != nil || automatic != 0 || adopted != 3 {
		t.Fatalf("automatic %d, adoption events %d (%v)", automatic, adopted, err)
	}
}
