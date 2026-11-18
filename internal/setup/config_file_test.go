package setup

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestConfigFileProvidesStagedIdentityBeforePublication(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	original := []byte("{\"untouched\":true}\n")
	writeConfigFixture(t, path, original, 0644)
	before, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	newData := []byte("{\"hooks\":{}}\n")
	journalError := errors.New("journal write failed")
	err = replaceConfigJournaled(path, before, newData, before.Mode, false, func(_, id, _ string) error {
		if id == "" || id == before.Identity {
			t.Errorf("stage identity: %q", id)
		}
		return journalError
	})
	if !errors.Is(err, journalError) {
		t.Fatalf("failed journal operation: %v", err)
	}
	afterFailure, err := readConfig(path)
	if err != nil || !sameConfigSnapshot(before, afterFailure) {
		t.Fatalf("configuration changed before journal was written: %+v, %v", afterFailure, err)
	}

	var stagedID string
	err = replaceConfigJournaled(path, before, newData, before.Mode, false, func(_, id, _ string) error {
		stagedID = id
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	after, err := readConfig(path)
	if err != nil || after.Identity != stagedID || !bytes.Equal(after.Bytes, newData) {
		t.Fatalf("published snapshot: %+v, stage identity: %q, error: %v", after, stagedID, err)
	}
}

func TestConfigFileRechecksTargetAfterJournalWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	writeConfigFixture(t, path, []byte("{\"before\":true}\n"), 0644)
	before, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	edited := []byte("{\"owner_edit\":true}\n")
	err = replaceConfigJournaled(path, before, []byte("{}\n"), before.Mode, false, func(string, string, string) error {
		return os.WriteFile(path, edited, before.Mode)
	})
	if err == nil {
		t.Fatal("replace accepted a configuration edit after writing the journal")
	}
	after, err := readConfig(path)
	if err != nil || !bytes.Equal(after.Bytes, edited) {
		t.Fatalf("owner edit changed: %+v, %v", after, err)
	}
}

func TestConfigFileAtomicReplaceAndRemove(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	original := []byte("{\n  \"other\": true\n}\n")
	writeConfigFixture(t, path, original, 0640)
	before, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !before.Exists || before.Mode != 0640 || before.Identity == "" || before.DirIdentity == "" {
		t.Fatalf("snapshot: %+v", before)
	}
	updated := []byte("{\"other\":true,\"hooks\":{}}\n")
	if err := replaceConfig(path, before, updated, before.Mode, false); err != nil {
		t.Fatal(err)
	}
	after, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after.Bytes, updated) || after.Mode != 0640 || after.Identity == before.Identity ||
		after.DirIdentity != before.DirIdentity {
		t.Fatalf("replacement snapshot: %+v", after)
	}
	if err := replaceConfig(path, after, nil, after.Mode, true); err != nil {
		t.Fatal(err)
	}
	absent, err := readConfig(path)
	if err != nil || absent.Exists || absent.DirIdentity != before.DirIdentity {
		t.Fatalf("after removal: %+v, %v", absent, err)
	}
}

func TestConfigFileRefusesUnsafePaths(t *testing.T) {
	for _, name := range []string{"symlink file", "hard link", "symlink directory", "Git directory", "Git worktree", "writable file", "writable directory"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			realDir := filepath.Join(root, "actual")
			if err := os.Mkdir(realDir, 0700); err != nil {
				t.Fatal(err)
			}
			original := []byte(`{"untouched":true}`)
			realFile := filepath.Join(realDir, "settings.json")
			writeConfigFixture(t, realFile, original, 0600)

			path := realFile
			var err error
			switch name {
			case "symlink file":
				path = filepath.Join(root, "settings.json")
				err = os.Symlink(realFile, path)
			case "hard link":
				path = filepath.Join(root, "settings.json")
				err = os.Link(realFile, path)
			case "symlink directory":
				dir := filepath.Join(root, "elsewhere")
				err = os.Symlink(realDir, dir)
				path = filepath.Join(dir, "settings.json")
			case "Git directory":
				err = os.Mkdir(filepath.Join(root, ".git"), 0700)
			case "Git worktree":
				err = os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: /synthetic/fixture"), 0600)
			case "writable file":
				err = os.Chmod(realFile, 0666)
			case "writable directory":
				err = os.Chmod(realDir, 0777)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = readConfig(path); err == nil {
				t.Fatal("readConfig accepted unsafe path")
			}
			if err = replaceConfig(path, fileSnapshot{}, []byte(`{}`), 0600, false); err == nil {
				t.Fatal("replaceConfig accepted unsafe path")
			}
			unchanged, err := os.ReadFile(realFile)
			if err != nil || !bytes.Equal(unchanged, original) {
				t.Fatalf("real file changed: %q, %v", unchanged, err)
			}
		})
	}
}

func TestConfigFileRejectsDrift(t *testing.T) {
	for _, name := range []string{"content", "mode", "same-content replacement", "directory replacement", "file created after plan"} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "config")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "hooks.json")
			original := []byte(`{"before":true}`)
			if name != "file created after plan" {
				writeConfigFixture(t, path, original, 0644)
			}
			before, err := readConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			switch name {
			case "content":
				writeConfigFixture(t, path, []byte(`{"new":true}`), 0644)
			case "mode":
				err = os.Chmod(path, 0600)
			case "same-content replacement":
				replacement := filepath.Join(dir, "replacement")
				writeConfigFixture(t, replacement, original, 0644)
				err = os.Rename(replacement, path)
			case "directory replacement":
				err = os.Rename(dir, dir+"-old")
				if err == nil {
					err = os.Mkdir(dir, 0700)
				}
				if err == nil {
					err = os.Link(filepath.Join(dir+"-old", "hooks.json"), path)
				}
				if err == nil {
					err = os.Remove(filepath.Join(dir+"-old", "hooks.json"))
				}
			case "file created after plan":
				writeConfigFixture(t, path, original, 0644)
			}
			if err != nil {
				t.Fatal(err)
			}
			current, err := readConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := replaceConfig(path, before, []byte(`{}`), 0600, false); err == nil {
				t.Fatal("accepted a stale snapshot")
			}
			after, err := readConfig(path)
			if err != nil || !sameConfigSnapshot(current, after) {
				t.Fatalf("stale replace changed configuration: %+v, %v", after, err)
			}
		})
	}
}

func TestConfigFileCreatesExplicitMissingHome(t *testing.T) {
	for _, interrupted := range []bool{false, true} {
		dir := filepath.Join(t.TempDir(), "selected-config-home")
		path := filepath.Join(dir, "hooks.json")
		before, err := readConfig(path)
		if err != nil || before.Exists {
			t.Fatalf("missing home snapshot: %+v, %v", before, err)
		}
		if _, err := os.Lstat(dir); !os.IsNotExist(err) {
			t.Fatalf("readConfig created config home, lstat: %v", err)
		}
		if interrupted {
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			writeConfigFixture(t, filepath.Join(dir, configStage+"interrupted.tmp"), []byte("partial"), 0600)
		}
		if err := replaceConfig(path, before, nil, 0600, false); err != nil {
			t.Fatal(err)
		}
		after, err := readConfig(path)
		if err != nil || !after.Exists || len(after.Bytes) != 0 || after.Mode != 0600 ||
			after.DirIdentity == before.DirIdentity {
			t.Fatalf("new file snapshot: %+v, %v", after, err)
		}
	}
}

func TestConfigFileMissingHomeRejectsUnrelatedContents(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "selected-config-home")
	path := filepath.Join(dir, "hooks.json")
	before, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	writeConfigFixture(t, filepath.Join(dir, "separate-owner-file"), []byte("unrelated"), 0600)
	if err := replaceConfig(path, before, []byte(`{}`), 0600, false); err == nil {
		t.Fatal("accepted a newly populated configuration directory")
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("unexpected config file: %v", err)
	}
}

func writeConfigFixture(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}
