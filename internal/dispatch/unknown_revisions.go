package dispatch

import (
	"encoding/json"
	"io"
	"os"
	"slices"
	"sync"

	"github.com/BeomSeogKim/crosshall/internal/hook/manifests"
	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

const (
	unknownRevisionsFile = "unknown-revisions"
	maxUnknownRevisions  = 8
	maxRevisionToken     = 64
	// unknownRevisionsLimit holds maxUnknownRevisions entries of the longest
	// adapter name and revision token, which need no JSON escaping.
	unknownRevisionsLimit = 4096
)

// UnknownRevisionMessage refuses the enrollment recovery of a hook whose
// manifest revision this dispatcher does not know, and owner doctor repeats it.
func UnknownRevisionMessage(revision string) string {
	return "Hook manifest revision " + revision + " is unknown to this dispatcher. " +
		"If Crosshall was upgraded, restart the dispatcher (crosshall owner stop, then crosshall owner start)."
}

// unknownRevisionRecovery names the revision of a prompt that asks to recover
// its enrollment, of a known adapter, with a revision this build does not know:
// a hook of a newer build than the running dispatcher. Its revision must be a
// short token of letters, digits, '.', '_' and '-'.
func unknownRevisionRecovery(args wire.HookEventArgs) (string, bool) {
	adapter, revision := args.Profile.Adapter, args.Profile.ManifestRev
	if args.EnrollRepoRoot == "" || !args.WantOutput || args.Fact != wire.HookPromptSubmitted || args.TurnRef == "" ||
		!manifests.Has(adapter) || !revisionToken(revision) {
		return "", false
	}
	_, known := manifests.CapabilitiesAt(adapter, revision)
	return revision, !known
}

func revisionToken(s string) bool {
	if s == "" || len(s) > maxRevisionToken {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

type unknownRevision struct {
	Adapter  string `json:"adapter"`
	Revision string `json:"revision"`
}

// unknownRevisions is the state file owner doctor reads: the revisions the
// dispatcher with this ready instance refused as unknown.
type unknownRevisions struct {
	Instance  string            `json:"instance"`
	Revisions []unknownRevision `json:"revisions"`
}

// revisionNotes keeps the revisions this dispatcher refused as unknown, once
// each and at most maxUnknownRevisions, for owner doctor. A restarted
// dispatcher has another ready instance, so doctor ignores an older file.
type revisionNotes struct {
	mu   sync.Mutex
	seen []unknownRevision
}

func (n *revisionNotes) note(state *os.Root, instance, adapter, revision string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	entry := unknownRevision{adapter, revision}
	if slices.Contains(n.seen, entry) || len(n.seen) >= maxUnknownRevisions {
		return
	}
	n.seen = append(n.seen, entry)
	_ = spool.PublishServerFile(state, unknownRevisionsFile, unknownRevisions{Instance: instance, Revisions: n.seen})
}

// unknownRevisionWarnings reads the revisions the live dispatcher with this
// ready instance refused as unknown.
func unknownRevisionWarnings(state *os.Root, instance string) []string {
	file, err := spool.OpenServerFile(state, unknownRevisionsFile, os.O_RDONLY)
	if err != nil {
		return nil
	}
	defer file.Close()
	var record unknownRevisions
	if json.NewDecoder(io.LimitReader(file, unknownRevisionsLimit)).Decode(&record) != nil ||
		instance == "" || record.Instance != instance {
		return nil
	}
	var warnings []string
	for _, r := range record.Revisions[:min(len(record.Revisions), maxUnknownRevisions)] {
		warnings = append(warnings, UnknownRevisionMessage(r.Revision))
	}
	return warnings
}
