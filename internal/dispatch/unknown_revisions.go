package dispatch

import (
	"encoding/json"
	"io"
	"os"
	"slices"
	"sync"

	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/spool"
)

const (
	unknownRevisionsFile = "unknown-revisions"
	maxUnknownRevisions  = 8
)

type unknownRevision struct {
	Adapter  string `json:"adapter"`
	Revision string `json:"revision"`
}

// unknownRevisions is the state file owner doctor reads: the revisions the
// dispatcher with PID refused as unknown since it started.
type unknownRevisions struct {
	PID       int               `json:"pid"`
	Revisions []unknownRevision `json:"revisions"`
}

// RevisionNotes keeps the hook manifest revisions this dispatcher refused as
// unknown, once each and at most maxUnknownRevisions, for owner doctor. A
// restarted dispatcher has another PID, so doctor ignores an older file.
type RevisionNotes struct {
	StateRoot string
	mu        sync.Mutex
	seen      []unknownRevision
}

// Note records one refused revision; it is domain.Service.UnknownRevision.
func (n *RevisionNotes) Note(adapter, revision string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	entry := unknownRevision{adapter, revision}
	if slices.Contains(n.seen, entry) || len(n.seen) >= maxUnknownRevisions {
		return
	}
	n.seen = append(n.seen, entry)
	state, err := spool.OpenServerRoot(n.StateRoot)
	if err != nil {
		return
	}
	defer state.Close()
	_ = spool.PublishServerFile(state, unknownRevisionsFile, unknownRevisions{PID: os.Getpid(), Revisions: n.seen})
}

// unknownRevisionWarnings reads the revisions the live dispatcher with pid
// refused as unknown.
func unknownRevisionWarnings(state *os.Root, pid int) []string {
	file, err := spool.OpenServerFile(state, unknownRevisionsFile, os.O_RDONLY)
	if err != nil {
		return nil
	}
	defer file.Close()
	var record unknownRevisions
	if json.NewDecoder(io.LimitReader(file, 4096)).Decode(&record) != nil || record.PID != pid {
		return nil
	}
	var warnings []string
	for _, r := range record.Revisions[:min(len(record.Revisions), maxUnknownRevisions)] {
		warnings = append(warnings, domain.UnknownRevisionMessage(r.Revision))
	}
	return warnings
}
