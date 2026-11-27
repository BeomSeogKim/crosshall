package setup

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"

	"github.com/BeomSeogKim/crosshall/internal/wire"
	"golang.org/x/sys/unix"
)

type fileSnapshot struct {
	Exists      bool
	Bytes       []byte
	Mode        os.FileMode
	Identity    string
	DirIdentity string
}

const (
	missingConfigDir = "missing:"
	configStage      = ".crosshall-config-"
	maxConfigBytes   = 16 << 20
)

type configLocation struct {
	dir            *os.Root
	missing        string
	missingParents []string
}

func (location configLocation) matchesMissingDir(identity string) bool {
	return slices.Contains(location.missingParents, identity)
}

func readConfig(path string) (fileSnapshot, error) {
	location, err := openConfigLocation(path)
	if err != nil {
		return fileSnapshot{}, err
	}
	defer location.dir.Close()
	if location.missing != "" {
		return fileSnapshot{DirIdentity: location.missingParents[len(location.missingParents)-1]}, nil
	}
	return readConfigAt(location.dir, filepath.Base(path))
}

func replaceConfig(path string, before fileSnapshot, data []byte, mode os.FileMode, remove bool) error {
	return replaceConfigJournaled(path, before, data, mode, remove, nil)
}

func replaceConfigJournaled(path string, before fileSnapshot, data []byte, mode os.FileMode, remove bool, beforeWrite func(string, string, string) error, afterMkdir ...func(createdConfigDir) error) error {
	if mode != mode.Perm() || mode&0022 != 0 || len(data) > maxConfigBytes {
		return errors.New("invalid configuration replacement mode or size")
	}
	location, err := openConfigLocation(path)
	if err != nil {
		return err
	}
	defer func() { location.dir.Close() }()
	if location.missing != "" {
		if before.Exists || !location.matchesMissingDir(before.DirIdentity) {
			return errors.New("configuration directory changed after planning")
		}
		if remove {
			return nil
		}
		for name := range strings.SplitSeq(location.missing, "/") {
			if err := location.dir.Mkdir(name, 0700); err != nil {
				return err
			}
			if err := syncConfigDir(location.dir); err != nil {
				return err
			}
			created, err := openConfigChild(location.dir, name, true)
			if err != nil {
				return err
			}
			if len(afterMkdir) != 0 {
				if err := recordCreatedConfigDir(location.dir, created, name, afterMkdir[0]); err != nil {
					created.Close()
					return err
				}
			}
			location.dir.Close()
			location.dir = created
		}
	}

	// Separate Crosshall journals can refer to the same vendor configuration.
	// Use the pinned directory's inode to serialize compare-and-replace.
	lock, err := location.dir.Open(".")
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("lock configuration directory: %w", err)
	}

	name := filepath.Base(path)
	current, err := readConfigAt(location.dir, name)
	if err != nil {
		return err
	}
	if !before.Exists && !current.Exists && location.matchesMissingDir(before.DirIdentity) {
		if err := checkNewConfigDir(location.dir); err != nil {
			return err
		}
		before.DirIdentity = current.DirIdentity
	}
	if !sameConfigSnapshot(current, before) {
		return errors.New("configuration file changed after planning")
	}
	if remove && !current.Exists {
		return nil
	}

	var stage string
	var staged os.FileInfo
	if !remove {
		stage, staged, err = stageConfigBeforeWrite(location.dir, data, mode, func(name string, info os.FileInfo) error {
			if beforeWrite == nil {
				return nil
			}
			return beforeWrite(name, configIdentity(info), current.DirIdentity)
		})
		if err != nil {
			return err
		}
		defer func() {
			_ = unlinkConfigStage(location.dir, stage, configIdentity(staged))
		}()
	} else if beforeWrite != nil {
		if err := beforeWrite("", "", current.DirIdentity); err != nil {
			return err
		}
	}
	observed, err := readConfig(path)
	if err != nil {
		return err
	}
	if !sameConfigSnapshot(observed, before) {
		return errors.New("configuration file changed during replacement")
	}
	if remove {
		err = location.dir.Remove(name)
	} else {
		info, statErr := location.dir.Lstat(stage)
		if statErr != nil || !sameConfigInfo(staged, info) {
			return errors.New("staged configuration changed during replacement")
		}
		err = location.dir.Rename(stage, name)
	}
	if err != nil {
		return err
	}
	return syncConfigDir(location.dir)
}

func recordCreatedConfigDir(parent, child *os.Root, name string, record func(createdConfigDir) error) error {
	parentInfo, err := parent.Stat(".")
	if err != nil {
		return err
	}
	childInfo, err := child.Stat(".")
	if err != nil {
		return err
	}
	return record(createdConfigDir{Name: name, Identity: configIdentity(childInfo), Parent: configIdentity(parentInfo)})
}

func missingConfigIdentity(parentID, relative string) string {
	if strings.ContainsRune(relative, '/') {
		return missingConfigDir + parentID + "/" + relative
	}
	return missingConfigDir + parentID
}

func configCleanupMatchesAbsence(path, dir string, owned createdConfigDir) bool {
	for _, managed := range managedConfigDirectories(path) {
		if filepath.Base(managed) != owned.Name {
			continue
		}
		relative, err := filepath.Rel(filepath.Dir(managed), filepath.Dir(path))
		if err == nil && dir == missingConfigIdentity(owned.Parent, relative) {
			return true
		}
	}
	return false
}

func removeCreatedConfigDir(path string, owned createdConfigDir) error {
	matched := false
	for _, managed := range managedConfigDirectories(path) {
		if filepath.Base(managed) != owned.Name {
			continue
		}
		matched = true
		if err := removeOwnedConfigDir(path, filepath.Dir(managed), owned); err != nil {
			return err
		}
	}
	if !matched {
		return errors.New("invalid configuration directory record")
	}
	return nil
}

func removeOwnedConfigDir(path, parent string, owned createdConfigDir) error {
	location, err := openConfigParent(path, parent)
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
	parentFile, err := location.dir.Open(".")
	if err != nil {
		return err
	}
	defer parentFile.Close()
	parentInfo, err := parentFile.Stat()
	if err != nil || configIdentity(parentInfo) != owned.Parent {
		return err
	}
	info, err := location.dir.Lstat(owned.Name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !safeConfigDirectory(info, true) || info.Mode().Perm() != 0700 || configIdentity(info) != owned.Identity {
		return err
	}
	child, err := openConfigChild(location.dir, owned.Name, true)
	if err != nil {
		return err
	}
	defer child.Close()
	lock, err := child.Open(".")
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("lock configuration directory %q for cleanup: %w", owned.Name, err)
	}
	info, err = location.dir.Lstat(owned.Name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !safeConfigDirectory(info, true) || info.Mode().Perm() != 0700 || configIdentity(info) != owned.Identity {
		return err
	}
	err = unix.Unlinkat(int(parentFile.Fd()), owned.Name, unix.AT_REMOVEDIR)
	switch {
	case errors.Is(err, syscall.ENOTEMPTY), errors.Is(err, syscall.EEXIST), errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return err
	}
	return syncConfigDir(location.dir)
}

// removeConfigStage cleans up only the file recorded in the original target
// directory. A completed rename or an owner-replaced path leaves nothing to
// remove at these identities.
func removeConfigStage(path, name, identity, dirIdentity string) error {
	id, prefixed := strings.CutPrefix(name, configStage)
	id, suffixed := strings.CutSuffix(id, ".tmp")
	if !prefixed || !suffixed || len(id) != 32 || identity == "" || dirIdentity == "" {
		return errors.New("invalid staged configuration record")
	}
	if _, err := hex.DecodeString(id); err != nil {
		return errors.New("invalid staged configuration name")
	}
	location, err := openConfigLocation(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
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
	if configIdentity(info) != dirIdentity {
		return nil
	}
	return unlinkConfigStage(location.dir, name, identity)
}

func unlinkConfigStage(dir *os.Root, name, identity string) error {
	info, err := dir.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !safeConfigFile(info) || configIdentity(info) != identity {
		return nil
	}
	if err := dir.Remove(name); err != nil {
		return err
	}
	return syncConfigDir(dir)
}

// openConfigLocation pins the directory and checks each component before use.
func openConfigLocation(path string) (location configLocation, err error) {
	return openConfigParent(path, filepath.Dir(path))
}

func openConfigParent(path, parent string) (location configLocation, err error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.Dir(path) == "/" {
		return location, errors.New("configuration file requires an absolute, clean path under a user directory")
	}
	if err := checkTestVendorPath(path); err != nil {
		return location, err
	}
	guard, err := newSetupPathGuard()
	if err != nil {
		return location, err
	}
	if err := guard.checkPath(path); err != nil {
		return location, err
	}
	configParent := parent == filepath.Dir(path)
	managed := len(managedConfigDirectories(path))
	parent = systemConfigDirectory(parent)
	parts := strings.Split(strings.TrimPrefix(parent, "/"), "/")
	if parent == "/" {
		parts = nil
	}
	location.dir, err = os.OpenRoot("/")
	if err != nil {
		return location, err
	}
	defer func() {
		if err != nil {
			location.dir.Close()
		}
	}()
	for i, name := range parts {
		if err = guard.checkDirectory(location.dir); err != nil {
			return location, err
		}
		final := i == len(parts)-1
		managedAncestor := configParent && managed > 1 && i >= len(parts)-managed
		mayBeMissing := final || managedAncestor
		var relative string
		if mayBeMissing {
			relative = strings.Join(parts[i:], "/")
			info, statErr := location.dir.Stat(".")
			if statErr != nil {
				return location, statErr
			}
			location.missingParents = append(location.missingParents, missingConfigIdentity(configIdentity(info), relative))
		}
		child, openErr := openConfigChild(location.dir, name, final && configParent)
		if mayBeMissing && errors.Is(openErr, os.ErrNotExist) {
			location.missing = relative
			return location, nil
		}
		if openErr != nil {
			return location, openErr
		}
		location.dir.Close()
		location.dir = child
	}
	err = guard.checkDirectory(location.dir)
	return location, err
}

// The root-owned macOS aliases name the same system directories users see in
// TMPDIR and /tmp. Other symlinks in a configuration path are refused.
func systemConfigDirectory(path string) string {
	if runtime.GOOS == "darwin" {
		for _, alias := range []string{"/tmp", "/var"} {
			if path != alias && !strings.HasPrefix(path, alias+"/") {
				continue
			}
			info, err := os.Lstat(alias)
			if err != nil || info.Sys().(*syscall.Stat_t).Uid != 0 || info.Mode()&os.ModeSymlink == 0 {
				break
			}
			target, err := os.Readlink(alias)
			if err == nil && (target == "/private"+alias || target == "private"+alias) {
				return "/private" + path
			}
			break
		}
	}
	return path
}

func openConfigChild(parent *os.Root, name string, final bool) (*os.Root, error) {
	info, err := parent.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !safeConfigDirectory(info, final) {
		return nil, fmt.Errorf("unsafe configuration directory %q", name)
	}
	child, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	actual, err := child.Stat(".")
	if err != nil || !os.SameFile(info, actual) || !safeConfigDirectory(actual, final) {
		child.Close()
		return nil, fmt.Errorf("configuration directory %q changed while opening", name)
	}
	current, err := parent.Lstat(name)
	if err != nil || !os.SameFile(actual, current) || !safeConfigDirectory(current, final) {
		child.Close()
		return nil, fmt.Errorf("configuration directory %q changed while opening", name)
	}
	return child, nil
}

func safeConfigDirectory(info os.FileInfo, final bool) bool {
	if !info.IsDir() {
		return false
	}
	uid := info.Sys().(*syscall.Stat_t).Uid
	ours := uid == uint32(os.Geteuid())
	if final {
		return ours && info.Mode().Perm()&0022 == 0
	}
	return (ours || uid == 0) && (info.Mode().Perm()&0022 == 0 || info.Mode()&os.ModeSticky != 0)
}

func readConfigAt(dir *os.Root, name string) (fileSnapshot, error) {
	var snapshot fileSnapshot
	dirInfo, err := dir.Stat(".")
	if err != nil {
		return snapshot, err
	}
	snapshot.DirIdentity = configIdentity(dirInfo)
	info, err := dir.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return snapshot, nil
	}
	if err != nil {
		return snapshot, err
	}
	if !safeConfigFile(info) {
		return snapshot, fmt.Errorf("unsafe configuration file %q", name)
	}
	file, err := dir.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return snapshot, err
	}
	defer file.Close()
	actual, err := file.Stat()
	if err != nil || !sameConfigInfo(info, actual) {
		return snapshot, fmt.Errorf("configuration file %q changed while opening", name)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxConfigBytes+1))
	if err != nil {
		return snapshot, err
	}
	if len(data) > maxConfigBytes {
		return snapshot, errors.New("configuration file is larger than 16 MiB")
	}
	after, err := dir.Lstat(name)
	if err != nil || !sameConfigInfo(actual, after) || int64(len(data)) != after.Size() {
		return snapshot, fmt.Errorf("configuration file %q changed while reading", name)
	}
	snapshot.Exists = true
	snapshot.Bytes = data
	snapshot.Mode = actual.Mode().Perm()
	snapshot.Identity = configIdentity(actual)
	return snapshot, nil
}

func configIdentity(info os.FileInfo) string {
	stat := info.Sys().(*syscall.Stat_t)
	return fmt.Sprintf("%x:%x", uint64(stat.Dev), uint64(stat.Ino))
}

func safeConfigFile(info os.FileInfo) bool {
	stat := info.Sys().(*syscall.Stat_t)
	return info.Mode().IsRegular() && info.Mode() == info.Mode().Perm() &&
		info.Mode().Perm()&0022 == 0 && stat.Nlink == 1 && stat.Uid == uint32(os.Geteuid())
}

func sameConfigInfo(expected, found os.FileInfo) bool {
	return safeConfigFile(found) && os.SameFile(expected, found) && expected.Mode() == found.Mode() &&
		expected.Size() == found.Size() && expected.ModTime().Equal(found.ModTime())
}

func sameConfigSnapshot(a, b fileSnapshot) bool {
	return a.DirIdentity == b.DirIdentity && a.Identity == b.Identity && a.Exists == b.Exists &&
		a.Mode == b.Mode && bytes.Equal(a.Bytes, b.Bytes)
}

func checkNewConfigDir(dir *os.Root) error {
	info, err := dir.Stat(".")
	if err != nil {
		return err
	}
	if info.Mode().Perm() != 0700 {
		return errors.New("new configuration directory does not have mode 0700")
	}
	file, err := dir.Open(".")
	if err != nil {
		return err
	}
	defer file.Close()
	names, err := file.Readdirnames(-1)
	if err != nil {
		return err
	}
	for _, name := range names {
		if !strings.HasPrefix(name, configStage) || !strings.HasSuffix(name, ".tmp") {
			return errors.New("new configuration directory gained unrelated files after planning")
		}
	}
	return nil
}

func stageConfig(dir *os.Root, data []byte, mode os.FileMode) (string, os.FileInfo, error) {
	return stageConfigBeforeWrite(dir, data, mode, nil)
}

func stageConfigBeforeWrite(dir *os.Root, data []byte, mode os.FileMode, beforeWrite func(string, os.FileInfo) error) (string, os.FileInfo, error) {
	name := configStage + wire.NewID() + ".tmp"
	file, err := dir.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return "", nil, err
	}
	reserved, err := file.Stat()
	if err == nil && beforeWrite != nil {
		// Until the reservation is in the journal, the stage is empty.
		err = beforeWrite(name, reserved)
	}
	if err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Chmod(mode)
	}
	if err == nil {
		err = file.Sync()
	}
	var info os.FileInfo
	if err == nil {
		info, err = file.Stat()
	}
	if err = errors.Join(err, file.Close()); err != nil {
		if reserved != nil {
			_ = unlinkConfigStage(dir, name, configIdentity(reserved))
		}
		return "", nil, err
	}
	return name, info, nil
}

func syncConfigDir(dir *os.Root) error {
	file, err := dir.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(file.Sync(), file.Close())
}
