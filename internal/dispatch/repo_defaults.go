package dispatch

import (
	"context"

	"github.com/BeomSeogKim/crosshall/internal/cli"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

// adoptRepoDefaults moves a default kept under a main checkout's path to the
// repository's identity, where it is kept now, so that the repository's
// linked worktrees find it. The two differ only for a repository whose Git
// directory lives apart (git init --separate-git-dir). A default already
// kept under the identity is newer and stays.
func (s *server) adoptRepoDefaults(ctx context.Context) error {
	var roots []string
	if err := s.db.Update(ctx, func(tx store.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT repo_root FROM repo_default`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var root string
			if err := rows.Scan(&root); err != nil {
				return err
			}
			roots = append(roots, root)
		}
		return rows.Err()
	}); err != nil {
		return err
	}
	moves := map[string]string{}
	for _, root := range roots {
		// A move needs the repository known for certain: its Git directory
		// read without error and holding the objects. Anything else keeps
		// the row for a later start.
		if identity, linked := cli.RepoIdentity(root); !linked && identity != root && cli.HasObjectStore(root) {
			moves[root] = identity
		}
	}
	if len(moves) == 0 {
		return nil
	}
	return s.db.Update(ctx, func(tx store.Tx) error {
		for root, identity := range moves {
			if _, err := tx.ExecContext(ctx, `UPDATE OR IGNORE repo_default SET repo_root = ? WHERE repo_root = ?`, identity, root); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM repo_default WHERE repo_root = ?`, root); err != nil {
				return err
			}
		}
		return nil
	})
}
