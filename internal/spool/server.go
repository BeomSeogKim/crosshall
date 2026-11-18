package spool

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

const maxPacketBytes = 1 << 20

// BatchLimit caps work from one spool before the dispatcher checks its peers.
const BatchLimit = 32

// OpenServerRoot opens an absolute private directory without following its final component.
func OpenServerRoot(path string) (*os.Root, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) == "/" {
		return nil, fmt.Errorf("invalid private directory %q", path)
	}
	path = filepath.Clean(path)
	parent, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	return OpenServerChild(parent, filepath.Base(path), false)
}

// OpenServerChild opens one private directory with ownership and identity checks.
func OpenServerChild(parent *os.Root, name string, create bool) (*os.Root, error) {
	if name != filepath.Base(name) || name == "." || name == ".." || name == "" {
		return nil, fmt.Errorf("invalid private child %q", name)
	}
	if create {
		err := parent.Mkdir(name, 0700)
		if err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
	}
	fi, err := parent.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !privateMode(fi, os.ModeDir|0700) {
		return nil, fmt.Errorf("directory %s must be owned by this user and have mode 0700", name)
	}
	root, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	actual, err := root.Stat(".")
	if err != nil || !os.SameFile(fi, actual) || !privateMode(actual, os.ModeDir|0700) {
		root.Close()
		return nil, fmt.Errorf("directory %s changed while opening", name)
	}
	return root, nil
}

func privateMode(fi os.FileInfo, mode os.FileMode) bool {
	return fi.Mode() == mode && fi.Sys().(*syscall.Stat_t).Uid == uint32(os.Geteuid())
}

// OpenServerFile opens a private regular file without following the leaf name.
// Writes can begin only after the directory entry and handle agree.
func OpenServerFile(root *os.Root, name string, flags int) (*os.File, error) {
	if name != filepath.Base(name) || name == "." || name == ".." || name == "" ||
		flags&os.O_TRUNC != 0 {
		return nil, fmt.Errorf("invalid private file name or flags for %q", name)
	}
	before, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) && flags&os.O_CREATE != 0 {
		file, err := root.OpenFile(name, flags|os.O_EXCL|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
		if errors.Is(err, os.ErrExist) {
			return OpenServerFile(root, name, flags&^os.O_CREATE)
		}
		if err != nil {
			return nil, err
		}
		return verifyServerFile(root, name, file, nil)
	}
	if err != nil {
		return nil, err
	}
	if !privateMode(before, 0600) {
		return nil, fmt.Errorf("%q must be a regular 0600 file owned by this user", name)
	}
	file, err := root.OpenFile(name, flags&^os.O_CREATE|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	return verifyServerFile(root, name, file, before)
}

func verifyServerFile(root *os.Root, name string, file *os.File, before os.FileInfo) (*os.File, error) {
	opened, err := file.Stat()
	if err == nil {
		var current os.FileInfo
		current, err = root.Lstat(name)
		if err == nil && (!privateMode(opened, 0600) || !os.SameFile(opened, current) ||
			(before != nil && !os.SameFile(opened, before))) {
			err = fmt.Errorf("%q changed while opening", name)
		}
	}
	if err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

// DirectoryIdentity identifies the open directory, independent of its pathname.
func DirectoryIdentity(root *os.Root) (string, error) {
	fi, err := root.Stat(".")
	if err != nil {
		return "", err
	}
	st := fi.Sys().(*syscall.Stat_t)
	return fmt.Sprintf("%x:%x", uint64(st.Dev), uint64(st.Ino)), nil
}

// PrepareServerSpool creates the three worker-owned states of a spool.
func PrepareServerSpool(root *os.Root) error {
	for _, name := range []string{"in", "work", "out"} {
		child, err := OpenServerChild(root, name, true)
		if err != nil {
			return err
		}
		child.Close()
	}
	return nil
}

// PublishServerFile atomically publishes a JSON response or readiness marker.
func PublishServerFile(root *os.Root, name string, value any) error {
	if name != filepath.Base(name) || name == "." || name == ".." || name == "" {
		return fmt.Errorf("invalid publication name %q", name)
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	staged := strings.TrimSuffix(name, ".json") + ".part"
	if err := root.Remove(staged); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := root.OpenFile(staged, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(staged)
	_, err = f.Write(data)
	if err = errors.Join(err, f.Close()); err != nil {
		return err
	}
	return root.Rename(staged, name)
}

func serverAttempt(name string) (string, bool) {
	id, ok := strings.CutSuffix(name, ".json")
	if !ok || len(id) != 32 || id != strings.ToLower(id) {
		return "", false
	}
	_, err := hex.DecodeString(id)
	return id, err == nil
}

func readServerPacket(root *os.Root, name string) ([]byte, error) {
	f, err := OpenServerFile(root, name, os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if fi.Size() > maxPacketBytes {
		return nil, errors.New("request exceeds the packet size limit")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxPacketBytes+1))
	if err == nil && len(data) > maxPacketBytes {
		err = errors.New("request exceeds the packet size limit")
	}
	return data, err
}

// DrainServerSpool recovers claimed files first and claims new files by rename.
// A processing or publication error leaves the claim for a later sweep.
func DrainServerSpool(root *os.Root, handle func(string, []byte) (core.Result, error)) error {
	in, err := OpenServerChild(root, "in", false)
	if err != nil {
		return err
	}
	defer in.Close()
	work, err := OpenServerChild(root, "work", false)
	if err != nil {
		return err
	}
	defer work.Close()
	out, err := OpenServerChild(root, "out", false)
	if err != nil {
		return err
	}
	defer out.Close()
	var failures error
	remaining := BatchLimit
	for _, area := range []struct {
		dir   *os.Root
		claim bool
	}{{work, false}, {in, true}} {
		entries, err := fs.ReadDir(area.dir.FS(), ".")
		if err != nil {
			return errors.Join(failures, err)
		}
		for _, entry := range entries {
			if remaining == 0 {
				return failures
			}
			name := entry.Name()
			id, ok := serverAttempt(name)
			if !ok || entry.IsDir() {
				continue
			}
			if area.claim {
				if _, err := work.Lstat(name); err == nil {
					continue
				} else if !errors.Is(err, os.ErrNotExist) {
					return errors.Join(failures, err)
				}
				err := root.Rename(filepath.Join("in", name), filepath.Join("work", name))
				if errors.Is(err, os.ErrNotExist) {
					continue // The client withdrew it before admission.
				}
				if err != nil {
					return errors.Join(failures, err)
				}
			}
			remaining--
			failures = errors.Join(failures, answerClaim(work, out, name, id, handle))
		}
	}
	return failures
}

func answerClaim(work, out *os.Root, name, id string, handle func(string, []byte) (core.Result, error)) error {
	if _, err := out.Lstat(name); err == nil {
		return work.Remove(name)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	data, err := readServerPacket(work, name)
	var result core.Result
	if err != nil {
		result = core.Result{Status: core.ExitUsage, Data: json.RawMessage(`{"message":"invalid request file"}`)}
	} else {
		result, err = handle(id, data)
		if err != nil {
			return err
		}
	}
	if err := PublishServerFile(out, name, wire.Response{Version: wire.Version, Attempt: id, Result: result}); err != nil {
		return err
	}
	return work.Remove(name)
}

// ReapServerSpool expires abandoned packets and staged files from this spool.
func ReapServerSpool(root *os.Root, before time.Time) error {
	for _, section := range []string{"in", "work", "out"} {
		dir, err := OpenServerChild(root, section, false)
		if err != nil {
			return err
		}
		err = reapServerDir(dir, before)
		dir.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func reapServerDir(dir *os.Root, before time.Time) error {
	entries, err := fs.ReadDir(dir.FS(), ".")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		final := strings.TrimSuffix(name, ".part")
		if final != name {
			final += ".json"
		}
		if _, ok := serverAttempt(final); !ok || entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err == nil && privateMode(info, 0600) && info.ModTime().Before(before) {
			if err := dir.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}
