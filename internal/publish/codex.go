package publish

// codex is Codex: a thread in the owning daemon. It stays a candidate until
// trusted capture is shown (design section 3), so it has no notice format
// and no transport.
type codex struct{}

func (codex) Complete(r Route) (bool, string) {
	if r.ThreadID == "" {
		return false, "no thread"
	}
	return true, ""
}

func (codex) ModeEligible(string) bool { return true }

func (codex) AcceptsSocket(string) bool { return false }

// SessionThread: a Codex session reference is its thread.
func (codex) SessionThread(sessionRef string) string { return sessionRef }

func (codex) Notice([]int64) (string, bool) { return "", false }

func (codex) Transport() Transport { return Transport{} }
