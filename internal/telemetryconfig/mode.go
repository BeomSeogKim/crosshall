package telemetryconfig

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/spool"
)

const (
	Endpoint = ""

	Unset = "unset"
	On    = "on"
	Off   = "off"

	TelemetryEnv  = "CROSSHALL_TELEMETRY"
	DoNotTrackEnv = "DO_NOT_TRACK"

	ManagedOffPath = "/etc/crosshall/telemetry-off"
)

type Setting struct {
	Mode      string
	ChangedAt time.Time
	Source    string
}

func EnvironmentOff() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(TelemetryEnv))) {
	case "off", "0", "false", "no", "disabled":
		return true
	}
	switch strings.ToLower(os.Getenv(DoNotTrackEnv)) {
	case "", "0", "false":
		return false
	default:
		return true
	}
}

func ManagedOff() bool { return ManagedOffAt(ManagedOffPath) }

func ManagedOffAt(path string) bool {
	_, err := os.Lstat(path)
	return !errors.Is(err, os.ErrNotExist)
}

func ReadMode(stateRoot string) (Setting, error) {
	dir, err := openTelemetryDir(stateRoot, false)
	if errors.Is(err, os.ErrNotExist) {
		return Setting{Mode: Unset}, nil
	}
	if err != nil {
		return Setting{}, err
	}
	defer dir.Close()
	return readMode(dir)
}

func readMode(dir *os.Root) (Setting, error) {
	f, err := dir.OpenFile("mode", os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return Setting{Mode: Unset}, nil
	}
	if err != nil {
		return Setting{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return Setting{}, err
	}
	if info.Mode() != 0600 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) || info.Size() > 160 {
		return Setting{}, errors.New("invalid telemetry/mode")
	}
	data, err := io.ReadAll(io.LimitReader(f, 161))
	if err != nil {
		return Setting{}, err
	}
	if len(data) > 160 {
		return Setting{}, errors.New("invalid telemetry/mode")
	}
	parts := strings.Fields(string(data))
	if len(parts) != 3 || (parts[0] != On && parts[0] != Off) {
		return Setting{}, errors.New("invalid telemetry/mode")
	}
	changed, err := time.Parse(time.RFC3339Nano, parts[1])
	if err != nil || changed.IsZero() || !validSource(parts[2]) {
		return Setting{}, errors.New("invalid telemetry/mode")
	}
	return Setting{Mode: parts[0], ChangedAt: changed.UTC(), Source: parts[2]}, nil
}

func validSource(source string) bool {
	switch source {
	case "installer", "owner-prompt", "owner-command", "environment":
		return true
	}
	return false
}

func NextFullDay(after time.Time) time.Time {
	return after.UTC().Truncate(24 * time.Hour).Add(24 * time.Hour)
}

func SetMode(stateRoot, mode, source, onlyIf string) (Setting, bool, error) {
	return setMode(stateRoot, mode, source, onlyIf, time.Now)
}

func setMode(stateRoot, mode, source, onlyIf string, now func() time.Time) (Setting, bool, error) {
	if (mode != On && mode != Off) || !validSource(source) {
		return Setting{}, false, errors.New("invalid telemetry setting")
	}
	dir, err := openTelemetryDir(stateRoot, onlyIf != On)
	if errors.Is(err, os.ErrNotExist) && onlyIf == On {
		return Setting{Mode: Unset}, false, nil
	}
	if err != nil {
		return Setting{}, false, err
	}
	defer dir.Close()

	lock, err := spool.OpenServerFile(dir, "mode.lock", os.O_RDWR|os.O_CREATE)
	if err != nil {
		return Setting{}, false, err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return Setting{}, false, err
	}
	current, err := readMode(dir)
	if err != nil || onlyIf != "" && current.Mode != onlyIf {
		return current, false, err
	}
	if current.Mode == mode {
		if mode == Off {
			err = dir.RemoveAll("pending")
		}
		return current, false, err
	}

	changedAt := now().UTC()
	if changedAt.Before(current.ChangedAt) {
		changedAt = current.ChangedAt
	}
	setting := Setting{Mode: mode, ChangedAt: changedAt, Source: source}
	createOnly := onlyIf == Unset
	if err := writeMode(dir, setting, createOnly); err != nil {
		if createOnly && errors.Is(err, os.ErrExist) {
			return Setting{}, false, nil
		}
		return Setting{}, false, err
	}
	if mode == Off {
		if err := dir.RemoveAll("pending"); err != nil {
			return setting, true, err
		}
	}
	return setting, true, nil
}

func openTelemetryDir(stateRoot string, create bool) (*os.Root, error) {
	if create {
		if err := os.MkdirAll(stateRoot, 0700); err != nil {
			return nil, err
		}
	}
	root, err := ident.OpenPrivateDir(stateRoot)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	if create {
		if err := root.Mkdir("telemetry", 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
	}
	return ident.OpenPrivateChild(root, "telemetry")
}

func writeMode(dir *os.Root, setting Setting, createOnly bool) error {
	stage := "mode-" + rand.Text() + ".part"
	file, err := dir.OpenFile(stage, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer dir.Remove(stage)
	_, err = fmt.Fprintf(file, "%s %s %s\n", setting.Mode, setting.ChangedAt.Format(time.RFC3339Nano), setting.Source)
	if err := errors.Join(err, file.Close()); err != nil {
		return err
	}
	if createOnly {
		return dir.Link(stage, "mode")
	}
	return dir.Rename(stage, "mode")
}
