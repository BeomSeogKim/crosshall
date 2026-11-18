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

// ServerFiles opens read-only private files within a pinned server root.
type ServerFiles struct{ Root *os.Root }

func (f ServerFiles) Open(name string) (fs.File, error) {
	return OpenServerFile(f.Root, name, os.O_RDONLY)
}

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
	return drainServerSpool(root, false, func(id string, raw []byte, _ bool) (core.Result, error) {
		return handle(id, raw)
	})
}

// DrainMemberSpool orders authenticated-leaf hook packets before processing
// dependent events. The handle receives whether a one-way hook was admitted
// before expiry, including when recovering a claim from an earlier sweep.
func DrainMemberSpool(root *os.Root, handle func(string, []byte, bool) (core.Result, error)) error {
	return drainServerSpool(root, true, handle)
}

type serverPacket struct {
	name, id string
	claim    bool
}

func drainServerSpool(root *os.Root, member bool, handle func(string, []byte, bool) (core.Result, error)) error {
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
	packets, err := readServerQueue(in, work)
	if err != nil {
		return err
	}
	if member {
		orderHookPackets(in, work, packets)
	}
	remaining := BatchLimit
	var process func(serverPacket, int) error
	process = func(packet serverPacket, depth int) error {
		if depth == BatchLimit {
			// New publications can add predecessors while a claim waits.
			// Yield to other spools and resume these claims next sweep.
			remaining = 0
			return nil
		}
		claimed, err := claimServerPacket(root, in, work, packet, member)
		if err != nil || !claimed {
			return err
		}
		for member {
			predecessors, err := hookPredecessors(in, work, packet)
			if err != nil {
				return err
			}
			before := remaining
			for _, predecessor := range predecessors {
				if err := process(predecessor, depth+1); err != nil {
					return err // Keep the dependent claim for the next sweep.
				}
				if remaining != before {
					break // Recheck the scope after each processed predecessor.
				}
			}
			if remaining == before {
				break
			}
			if remaining == 0 {
				return nil
			}
		}
		remaining--
		return answerClaim(work, out, packet, member, handle)
	}
	var failures error
	for _, packet := range packets {
		if remaining == 0 {
			break
		}
		failures = errors.Join(failures, process(packet, 0))
	}
	return failures
}

func readServerQueue(in, work *os.Root) ([]serverPacket, error) {
	var packets []serverPacket
	for _, area := range []struct {
		dir   *os.Root
		claim bool
	}{{work, false}, {in, true}} {
		entries, err := fs.ReadDir(area.dir.FS(), ".")
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			name := entry.Name()
			id, ok := serverAttempt(name)
			if ok && !entry.IsDir() {
				packets = append(packets, serverPacket{name: name, id: id, claim: area.claim})
			}
		}
	}
	return packets, nil
}

func claimServerPacket(root, in, work *os.Root, packet serverPacket, member bool) (bool, error) {
	_, err := work.Lstat(packet.name)
	if !packet.claim {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil // Already consumed as another hook's predecessor.
		}
		return err == nil, err
	}
	if err == nil {
		return false, nil // A recovered claim takes precedence over duplicate input.
	}
	if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if member {
		data, err := readServerPacket(in, packet.name)
		if err == nil {
			req, oneWay := oneWayHookPacket(data, packet.id)
			if oneWay && req.ExpiresAtMS <= time.Now().UnixMilli() {
				err := in.Remove(packet.name)
				if errors.Is(err, os.ErrNotExist) {
					err = nil
				}
				return false, err
			}
		}
	}
	err = root.Rename(filepath.Join("in", packet.name), filepath.Join("work", packet.name))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil // The client withdrew it before admission.
	}
	return err == nil, err
}

func oneWayHookPacket(data []byte, id string) (wire.Request, bool) {
	var req wire.Request
	valid := json.Unmarshal(data, &req) == nil && req.Version == wire.Version && req.Attempt == id && req.OneWayHook()
	return req, valid
}

func answerClaim(work, out *os.Root, packet serverPacket, member bool, handle func(string, []byte, bool) (core.Result, error)) error {
	name := packet.name
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
		var oneWay bool
		if member {
			_, oneWay = oneWayHookPacket(data, packet.id)
		}
		result, err = handle(packet.id, data, oneWay)
		if err != nil {
			return err
		}
		if oneWay {
			return work.Remove(name)
		}
	}
	if err := PublishServerFile(out, name, wire.Response{Version: wire.Version, Attempt: packet.id, Result: result}); err != nil {
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
