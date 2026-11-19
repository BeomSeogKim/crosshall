package setup

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/setuptest"
)

func offersService(t *testing.T) (Service, setuptest.Fixture) {
	t.Helper()
	f := setuptest.New(t)
	f.CheckTargets(t)
	return Service{StateRoot: f.State}, f
}

func snap(t *testing.T, s Service) map[string]int64 {
	t.Helper()
	seen, err := s.offerSnapshot()
	if err != nil {
		return nil
	}
	return seen
}

func TestAnExplicitNoIsRememberedPerVendorAndAYesClearsIt(t *testing.T) {
	s, _ := offersService(t)
	now := time.UnixMilli(1_000)
	if err := s.rememberAnswers(map[string]answer{"claude": answerNo, "codex": answerSkip}, snap(t, s), now); err != nil {
		t.Fatal(err)
	}
	declined, err := s.declinedOffers()
	if err != nil || !declined["claude"] || declined["codex"] {
		t.Fatalf("declined %v, %v", declined, err)
	}
	if err := s.rememberAnswers(map[string]answer{"claude": answerYes}, snap(t, s), now); err != nil {
		t.Fatal(err)
	}
	if declined, err := s.declinedOffers(); err != nil || declined["claude"] {
		t.Fatalf("a later yes kept the decline: %v, %v", declined, err)
	}
}

// Enter, end of input and an interruption are a skip: nothing is written,
// not even an empty preferences file.
func TestASkipRecordsNothing(t *testing.T) {
	s, f := offersService(t)
	if err := s.rememberAnswers(map[string]answer{"claude": answerSkip, "codex": answerSkip}, snap(t, s), time.UnixMilli(1)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(f.State); !os.IsNotExist(err) {
		t.Fatalf("a skip created setup state: %v", err)
	}
}

// Each update re-reads the file under the setup lock, so a decision made by
// another run in between is kept.
func TestDecisionsFromSeparateRunsAreMerged(t *testing.T) {
	s, _ := offersService(t)
	if err := s.rememberAnswers(map[string]answer{"claude": answerNo}, snap(t, s), time.UnixMilli(1)); err != nil {
		t.Fatal(err)
	}
	if err := s.rememberAnswers(map[string]answer{"codex": answerNo}, snap(t, s), time.UnixMilli(2)); err != nil {
		t.Fatal(err)
	}
	if declined, err := s.declinedOffers(); err != nil || !declined["claude"] || !declined["codex"] {
		t.Fatalf("declined %v, %v", declined, err)
	}
}

func TestPreferencesArePrivateAndSeparateFromTheJournal(t *testing.T) {
	s, f := offersService(t)
	if err := s.rememberAnswers(map[string]answer{"codex": answerNo}, snap(t, s), time.UnixMilli(1)); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(filepath.Join(f.State, offersName))
	if err != nil || info.Mode().Perm() != 0600 || !info.Mode().IsRegular() {
		t.Fatalf("preferences file %v, %v", info, err)
	}
	if _, err := os.Lstat(filepath.Join(f.State, journalName)); !os.IsNotExist(err) {
		t.Fatalf("a decline wrote the install journal: %v", err)
	}
	entries, _ := os.ReadDir(f.State)
	for _, e := range entries {
		if e.Name() != offersName && e.Name() != "install.lock" {
			t.Errorf("unexpected state entry %q", e.Name())
		}
	}
}

func TestAnUnreadablePreferencesFileIsNotAnEmptyOne(t *testing.T) {
	for _, content := range []string{"{broken", `{"v":1}`, `{"v":2,"vendors":{}}`, `{"vendors":{}}`} {
		s, f := offersService(t)
		if err := os.MkdirAll(f.State, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(f.State, offersName), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := s.declinedOffers(); err == nil {
			t.Errorf("%s read as no declines", content)
		}
		if err := s.rememberAnswers(map[string]answer{"claude": answerNo}, snap(t, s), time.UnixMilli(1)); err == nil {
			t.Errorf("%s was overwritten", content)
		}
	}
}

// A run that read the decisions earlier must not
// overwrite a newer decision another run made for the same vendor.
func TestAnOlderAnswerDoesNotEraseANewerDecision(t *testing.T) {
	s, _ := offersService(t)
	seenByA, err := s.offerSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	// Run B, started later, declines Claude and finishes first.
	seenByB, _ := s.offerSnapshot()
	if err := s.rememberAnswers(map[string]answer{"claude": answerNo}, seenByB, time.UnixMilli(2)); err != nil {
		t.Fatal(err)
	}
	// Run A answered yes for Claude and no for Codex before B decided.
	if err := s.rememberAnswers(map[string]answer{"claude": answerYes, "codex": answerNo}, seenByA, time.UnixMilli(3)); err != nil {
		t.Fatal(err)
	}
	if d, err := s.declinedOffers(); err != nil || !d["claude"] || !d["codex"] {
		t.Fatalf("declined %v, %v", d, err)
	}
	// And the other way round: a newer yes is not undone by an older no.
	seenByC, _ := s.offerSnapshot()
	seenByD, _ := s.offerSnapshot()
	if err := s.rememberAnswers(map[string]answer{"codex": answerYes}, seenByD, time.UnixMilli(4)); err != nil {
		t.Fatal(err)
	}
	if err := s.rememberAnswers(map[string]answer{"codex": answerNo}, seenByC, time.UnixMilli(5)); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.declinedOffers(); d["codex"] {
		t.Fatal("an older no undid a newer yes")
	}
}
