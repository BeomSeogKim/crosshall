package setup

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"strings"
	"syscall"

	"github.com/BeomSeogKim/crosshall/internal/spool"
)

const journalName = "install-journal.json"

type journal struct {
	Version   int                  `json:"v"`
	Files     map[string]ownedFile `json:"files,omitempty"`
	RuleFiles map[string]ownedFile `json:"rule_files,omitempty"`
	Builds    map[string]build     `json:"builds,omitempty"`
	Pending   *operation           `json:"pending,omitempty"`
	Retained  []retainedFile       `json:"retained,omitempty"`
}

type ownedFile struct {
	Path     string             `json:"path"`
	Original fingerprint        `json:"original"`
	Latest   fingerprint        `json:"latest"`
	Entries  []Entry            `json:"entries"`
	Undo     [][]span           `json:"undo,omitempty"`
	Dirs     []createdConfigDir `json:"created_dirs,omitempty"`
	RuleText string             `json:"rule_text,omitempty"`
}

type createdConfigDir struct {
	Name     string `json:"name"`
	Identity string `json:"directory_id"`
	Parent   string `json:"parent_id"`
}

type fingerprint struct {
	Exists bool        `json:"exists"`
	Hash   string      `json:"sha256,omitempty"`
	Mode   os.FileMode `json:"mode,omitempty"`
	File   string      `json:"file_id,omitempty"`
	Dir    string      `json:"directory_id"`
}

func stamp(s fileSnapshot) fingerprint {
	f := fingerprint{Exists: s.Exists, Dir: s.DirIdentity, File: s.Identity, Mode: s.Mode}
	if s.Exists {
		f.Hash = sum(s.Bytes)
	}
	return f
}

func sum(raw []byte) string {
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

type operation struct {
	Action   string           `json:"action"`
	Adapters []string         `json:"adapters"`
	Edits    []edit           `json:"edits,omitempty"`
	Builds   map[string]build `json:"builds,omitempty"`
	Retained []retainedFile   `json:"retained,omitempty"`
}

type retainedFile struct {
	Adapter string `json:"adapter"`
	Path    string `json:"path"`
	Reason  string `json:"reason"`
}

type edit struct {
	Adapter        string             `json:"adapter"`
	Path           string             `json:"path"`
	Rules          bool               `json:"rules,omitempty"`
	AddText        string             `json:"add_text,omitempty"`
	RemoveText     string             `json:"remove_text,omitempty"`
	Before         fingerprint        `json:"before"`
	After          fingerprint        `json:"after"`
	Add            []Entry            `json:"add,omitempty"`
	Remove         []Entry            `json:"remove,omitempty"`
	Kept           []Entry            `json:"kept,omitempty"`
	Restore        [][]span           `json:"restore,omitempty"`
	Record         *ownedFile         `json:"record,omitempty"`
	Stage          string             `json:"stage,omitempty"`
	StageDir       string             `json:"stage_dir,omitempty"`
	CleanupDirs    []createdConfigDir `json:"cleanup_dirs,omitempty"`
	CleanupStarted bool               `json:"cleanup_started,omitempty"`
}

type span struct {
	Start int `json:"offset"`
	End   int `json:"end"`
}

// The ordered editor inserts bytes without rewriting existing ones. The
// inverse records only ranges, keeping unrelated settings out of the journal.
func insertionSpans(before, after []byte) ([]span, error) {
	var inserted []span
	for i, j := 0, 0; ; {
		for i < len(before) && j < len(after) && before[i] == after[j] {
			i++
			j++
		}
		if i == len(before) {
			if j < len(after) {
				inserted = append(inserted, span{j, len(after)})
			}
			return inserted, nil
		}
		start := j
		for j < len(after) && after[j] != before[i] {
			j++
		}
		if j == len(after) {
			return nil, errors.New("configuration edit would rewrite existing content")
		}
		inserted = append(inserted, span{start, j})
	}
}

func undoInsertions(raw []byte, steps [][]span) ([]byte, error) {
	for i := len(steps) - 1; i >= 0; i-- {
		var reduced []byte
		start := 0
		for _, s := range steps[i] {
			if s.Start < start || s.End < s.Start || s.End > len(raw) {
				return nil, errors.New("invalid install journal restore range")
			}
			reduced = append(reduced, raw[start:s.Start]...)
			start = s.End
		}
		raw = append(reduced, raw[start:]...)
	}
	return raw, nil
}

func readJournal(stateRoot string) (journal, string, error) {
	state, err := openSetupState(stateRoot, false)
	if errors.Is(err, os.ErrNotExist) {
		return emptyJournal(), "", nil
	}
	if err != nil {
		return journal{}, "", err
	}
	defer state.Close()
	return readJournalAt(state)
}

func emptyJournal() journal {
	return journal{Version: 1, Files: make(map[string]ownedFile), RuleFiles: make(map[string]ownedFile), Builds: make(map[string]build)}
}

func readJournalAt(state *os.Root) (journal, string, error) {
	j := emptyJournal()
	file, err := spool.OpenServerFile(state, journalName, os.O_RDONLY)
	if errors.Is(err, os.ErrNotExist) {
		return j, "", nil
	}
	if err != nil {
		return j, "", err
	}
	defer file.Close()
	const maxJournal = 1 << 20
	data, err := io.ReadAll(io.LimitReader(file, maxJournal+1))
	if err != nil {
		return j, "", err
	}
	// Require an explicit supported version; maps default to initialized.
	j.Version = 0
	if len(data) > maxJournal || json.Unmarshal(data, &j) != nil ||
		j.Version != 1 || j.Files == nil || j.RuleFiles == nil || j.Builds == nil {
		return j, "", errors.New("invalid install journal")
	}
	return j, sum(data), nil
}

func writeJournal(state *os.Root, j journal) error {
	data, err := json.Marshal(j)
	if err != nil {
		return err
	}
	if len(data) > 1<<20 {
		return errors.New("install journal exceeds size limit")
	}
	stage, _, err := stageConfig(state, data, 0600)
	if err != nil {
		return err
	}
	defer state.Remove(stage)
	if err := state.Rename(stage, journalName); err != nil {
		return err
	}
	return syncConfigDir(state)
}

func (s Service) lockState() (*os.Root, *os.File, error) {
	state, err := openSetupState(s.StateRoot, true)
	if err != nil {
		return nil, nil, err
	}
	lock, err := spool.OpenServerFile(state, "install.lock", os.O_RDWR|os.O_CREATE)
	if err != nil {
		state.Close()
		return nil, nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		state.Close()
		return nil, nil, fmt.Errorf("cannot lock adapter installation: %w", err)
	}
	return state, lock, nil
}

func (e edit) matchesAfter(now fingerprint) bool {
	return now.Exists == e.After.Exists && now.Hash == e.After.Hash && now.Mode == e.After.Mode &&
		(!now.Exists || (e.After.File != "" && now.File == e.After.File)) &&
		(e.publicationDirectoryMatches(now.Dir) || !now.Exists && e.matchesCleanupAbsence(now.Dir))
}

func (e edit) matchesCleanupAbsence(dir string) bool {
	if !e.ownsDirectories() || !e.CleanupStarted {
		return false
	}
	for _, owned := range e.CleanupDirs {
		if configCleanupMatchesAbsence(e.Path, dir, owned) {
			return true
		}
	}
	return false
}

func (e edit) ownsDirectories() bool { return e.Adapter == "agy" || e.Rules }

func (e edit) publicationDirectoryMatches(dir string) bool {
	if e.StageDir != "" {
		return dir == e.StageDir
	}
	// Earlier journals recorded a missing directory but no staged directory.
	return dir == e.After.Dir || strings.HasPrefix(e.After.Dir, missingConfigDir)
}

func (e edit) matchesBefore(now fingerprint) bool {
	// An interrupted prepare can leave only the newly created directory.
	// Replacement still verifies the original parent before retrying.
	return now == e.Before ||
		(!e.Before.Exists && !now.Exists && strings.HasPrefix(e.Before.Dir, missingConfigDir))
}

func (e edit) stageCleanup() error {
	if e.Stage != "" {
		return removeConfigStage(e.Path, e.Stage, e.After.File, e.StageDir)
	}
	if e.After.File == "" || e.Before == e.After {
		return nil
	}
	// Pending plans written by the previous installer have only the inode.
	// Recover its name under the confirmed directory without taking a new
	// claim on other staged files.
	location, err := openConfigLocation(e.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer location.dir.Close()
	if location.missing != "" {
		return nil
	}
	info, err := location.dir.Stat(".")
	if err != nil {
		return err
	}
	dir := configIdentity(info)
	if dir != e.After.Dir && !location.matchesMissingDir(e.After.Dir) {
		return nil
	}
	files, err := fs.ReadDir(location.dir.FS(), ".")
	if err != nil {
		return err
	}
	for _, candidate := range files {
		name := candidate.Name()
		if !strings.HasPrefix(name, configStage) || !strings.HasSuffix(name, ".tmp") {
			continue
		}
		info, err := candidate.Info()
		if err == nil && configIdentity(info) == e.After.File {
			return removeConfigStage(e.Path, name, e.After.File, dir)
		}
	}
	return nil
}

func (e edit) materialize(raw []byte) ([]byte, error) {
	if !e.After.Exists {
		return nil, nil
	}
	if e.AddText != "" || e.RemoveText != "" {
		if e.RemoveText != "" {
			var removed bool
			raw, removed = removeRuleText(raw, e.RemoveText)
			if !removed {
				return nil, errors.New("managed command rule changed since planning")
			}
		}
		return append(raw, e.AddText...), nil
	}
	if len(e.Restore) != 0 {
		restored, err := undoInsertions(raw, e.Restore)
		if err != nil || len(e.Add) == 0 {
			return restored, err
		}
		raw = restored
	}
	if len(e.Add) != 0 {
		updated, _, err := addEntries(raw, e.Add)
		return updated, err
	}
	updated, _, err := removeEntries(raw, e.Remove)
	return updated, err
}

func applyEdit(state *os.Root, j journal, e *edit) (fingerprint, error) {
	if err := e.stageCleanup(); err != nil {
		return fingerprint{}, err
	}
	current, err := readConfig(e.Path)
	if err != nil {
		return fingerprint{}, err
	}
	now := stamp(current)
	if e.matchesAfter(now) {
		return finishEdit(state, j, e, now)
	}
	if !e.matchesBefore(now) {
		return fingerprint{}, errors.New("configuration changed since the recorded plan")
	}
	next, err := e.materialize(current.Bytes)
	if err != nil {
		return fingerprint{}, err
	}
	if e.After.Exists && sum(next) != e.After.Hash {
		return fingerprint{}, errors.New("configuration differs from the recorded result")
	}
	current.DirIdentity = e.Before.Dir
	var recordMkdir []func(createdConfigDir) error
	if e.ownsDirectories() && e.Record != nil {
		recordMkdir = append(recordMkdir, func(dir createdConfigDir) error {
			e.Record.Dirs = slices.DeleteFunc(e.Record.Dirs, func(owned createdConfigDir) bool {
				return owned.Name == dir.Name && owned.Parent == dir.Parent
			})
			e.Record.Dirs = append(e.Record.Dirs, dir)
			return writeJournal(state, j)
		})
	}
	err = replaceConfigJournaled(e.Path, current, next, e.After.Mode, !e.After.Exists, func(name, ident, dir string) error {
		e.Stage, e.After.File, e.StageDir = name, ident, dir
		return writeJournal(state, j)
	}, recordMkdir...)
	if err != nil {
		return fingerprint{}, err
	}
	result, err := readConfig(e.Path)
	if err != nil {
		return fingerprint{}, err
	}
	observed := stamp(result)
	if !e.matchesAfter(observed) {
		return fingerprint{}, errors.New("configuration changed after replacement")
	}
	return finishEdit(state, j, e, observed)
}

func finishEdit(state *os.Root, j journal, e *edit, observed fingerprint) (fingerprint, error) {
	if !e.ownsDirectories() || observed.Exists || len(e.CleanupDirs) == 0 {
		return observed, nil
	}
	if err := cleanupEditDirectories(state, j, e, e.CleanupDirs); err != nil {
		return fingerprint{}, err
	}
	current, err := readConfig(e.Path)
	if err != nil {
		return fingerprint{}, err
	}
	now := stamp(current)
	if !e.matchesAfter(now) {
		return fingerprint{}, errors.New("configuration changed during directory cleanup")
	}
	return now, nil
}

func cleanupEditDirectories(state *os.Root, j journal, e *edit, dirs []createdConfigDir) error {
	if !e.CleanupStarted {
		e.CleanupDirs, e.CleanupStarted = dirs, true
		if err := writeJournal(state, j); err != nil {
			return err
		}
	}
	for _, dir := range slices.Backward(e.CleanupDirs) {
		if err := removeCreatedConfigDir(e.Path, dir); err != nil {
			return err
		}
	}
	return nil
}
