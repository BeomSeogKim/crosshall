package ident

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Digest identifies the whole (vendor, key) pair. Only its prefix is a directory name.
func (id Identity) Digest() string {
	sum := sha256.Sum256([]byte(id.Vendor + "\x00" + id.Key))
	return hex.EncodeToString(sum[:])
}

// Rendezvous derives a path from the absolute state directory, without opening it.
func Rendezvous(stateRoot string, uid int) string {
	sum := sha256.Sum256([]byte(filepath.Clean(stateRoot)))
	return filepath.Join("/tmp", fmt.Sprintf("crosshall-%d-%x", uid, sum[:4]))
}

// FileIdentity is the device and inode of the directory held open by the dispatcher.
func FileIdentity(dir *os.Root) (string, error) {
	fi, err := dir.Stat(".")
	if err != nil {
		return "", err
	}
	st := fi.Sys().(*syscall.Stat_t)
	return strconv.FormatUint(uint64(st.Dev), 16) + ":" + strconv.FormatUint(uint64(st.Ino), 16), nil
}

// OpenPrivateDir opens the private root under its configured parent. Continue
// through private descendants with OpenPrivateChild, using the returned handle.
func OpenPrivateDir(path string) (*os.Root, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) == "/" {
		return nil, fmt.Errorf("expected an absolute private directory")
	}
	path = filepath.Clean(path)
	parent, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	return OpenPrivateChild(parent, filepath.Base(path))
}

func component(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\x00")
}

func checkPrivate(fi os.FileInfo, mode os.FileMode) error {
	if fi.Mode() != mode || fi.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("unexpected type, permissions or owner for %q", fi.Name())
	}
	return nil
}

// OpenPrivateChild pins a single private directory entry and verifies its identity after open.
func OpenPrivateChild(parent *os.Root, name string) (*os.Root, error) {
	if !component(name) {
		return nil, fmt.Errorf("expected a directory name")
	}
	before, err := parent.Lstat(name)
	if err != nil {
		return nil, err
	}
	if err := checkPrivate(before, os.ModeDir|0700); err != nil {
		return nil, err
	}
	child, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	after, err := child.Stat(".")
	if err == nil {
		err = checkPrivate(after, os.ModeDir|0700)
	}
	if err == nil && !os.SameFile(before, after) {
		err = fmt.Errorf("directory changed while opening %q", name)
	}
	if err != nil {
		child.Close()
		return nil, err
	}
	return child, nil
}

// ReadPrivateFile reads one regular file without following links or blocking on a FIFO.
func ReadPrivateFile(dir *os.Root, name string, limit int64) ([]byte, error) {
	if !component(name) || limit < 0 {
		return nil, fmt.Errorf("invalid private file name or limit")
	}
	before, err := dir.Lstat(name)
	if err != nil {
		return nil, err
	}
	if err := checkPrivate(before, 0600); err != nil {
		return nil, err
	}
	file, err := dir.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	fi, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(before, fi) {
		return nil, fmt.Errorf("file changed while opening %q", name)
	}
	if err := checkPrivate(fi, 0600); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err == nil && int64(len(data)) > limit {
		err = fmt.Errorf("private file %q exceeds %d bytes", name, limit)
	}
	if err != nil {
		return nil, err
	}
	return data, nil
}
