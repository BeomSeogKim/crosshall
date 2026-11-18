package setup

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestConfigStageIsEmptyUntilJournalWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	writeConfigFixture(t, path, []byte(`{"old":true}`), 0600)
	before, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}

	interrupt := errors.New("journal interrupted")
	var stagedPath string
	err = replaceConfigJournaled(path, before, []byte(`{"replacement":true}`), 0600, false, func(name, identity, dirIdentity string) error {
		stagedPath = filepath.Join(dir, name)
		info, err := os.Lstat(stagedPath)
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() != 0 || info.Mode().Perm() != 0600 {
			t.Errorf("stage before journaling: size %d, mode %o", info.Size(), info.Mode())
		}
		if identity != configIdentity(info) || dirIdentity != before.DirIdentity {
			t.Errorf("stage record has wrong identities: %q, %q", identity, dirIdentity)
		}
		return interrupt
	})
	if !errors.Is(err, interrupt) {
		t.Fatalf("interrupted journal write: %v", err)
	}
	if _, err := os.Lstat(stagedPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stage still exists after handling journal error: %v", err)
	}
}

func TestConfigStageRecoveryChecksRecordedIdentities(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	original := []byte(`{"old":true}`)
	writeConfigFixture(t, path, original, 0600)
	before, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	name, staged, err := stageConfig(root, []byte(`{"replacement":true}`), 0600)
	if err != nil {
		t.Fatal(err)
	}
	identity := configIdentity(staged)
	if err := removeConfigStage(path, name, identity, "different-directory"); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Lstat(name); err != nil {
		t.Fatalf("cleanup removed stage in a different directory: %v", err)
	}
	if err := removeConfigStage(path, name, identity, before.DirIdentity); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Lstat(name); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cleanup retained matching stage: %v", err)
	}

	name, staged, err = stageConfig(root, []byte(`{"replacement":true}`), 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := root.Rename(name, "saved-stage.json"); err != nil {
		t.Fatal(err)
	}
	replacement := []byte(`{"owner":true}`)
	writeConfigFixture(t, filepath.Join(dir, name), replacement, 0600)
	if err := removeConfigStage(path, name, configIdentity(staged), before.DirIdentity); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil || string(got) != string(replacement) {
		t.Fatalf("cleanup touched owner's replacement: %s, %v", got, err)
	}
}
