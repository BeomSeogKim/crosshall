package telemetryconfig

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/ident"
)

// PostGap is the least time between two first attempts of different
// batches; a retry of the same batch may come sooner.
const PostGap = 24 * time.Hour

// MaxReport is the largest report the dispatcher sends or reads back.
const MaxReport = 16 << 10

// State is telemetry/state.json, which the dispatcher writes. Days holds
// every recently frozen day, with or without a batch; a day in it is never
// built again.
type State struct {
	// SuppressedThrough is the latest time a dispatcher saw an opt-out
	// while the setting was on. It only moves forward.
	SuppressedThrough time.Time            `json:"suppressed_through,omitzero"`
	Days              map[string]*DayState `json:"days,omitempty"`
	LastPost          time.Time            `json:"last_post,omitzero"`
	StoppedVersion    string               `json:"stopped_version,omitempty"`
	LastStatus        string               `json:"last_status,omitempty"`
}

// DayState is one frozen day.
type DayState struct {
	Batch bool `json:"batch"`
	// Unobserved is the day's notice.unobserved_10m, for later days' week
	// figures.
	Unobserved int64     `json:"unobserved_10m,omitempty"`
	NotBefore  time.Time `json:"not_before,omitzero"`
	Attempts   int       `json:"attempts,omitempty"`
	// Accepted is set once the receiver answered 204, before the batch is
	// moved to sent/, so a failed move never leads to a second request.
	Accepted   bool      `json:"accepted,omitempty"`
	AcceptedAt time.Time `json:"accepted_at,omitzero"`
}

// EligibleFrom is the first day that lies wholly after the setting turned
// on and after the last opt-out a dispatcher saw. No earlier day is built
// or sent, whatever is still on disk.
func EligibleFrom(setting Setting, st *State) time.Time {
	from := setting.ChangedAt
	if st.SuppressedThrough.After(from) {
		from = st.SuppressedThrough
	}
	return NextFullDay(from)
}

// Sendable reports whether the pending batch of day may still be sent: it
// belongs to the current consent interval and the receiver has not
// accepted it.
func (st *State) Sendable(day string, eligible time.Time) bool {
	d := st.Days[day]
	return d != nil && !d.Accepted && day >= eligible.UTC().Format(time.DateOnly)
}

// Suppress records an opt-out seen at t; the cutoff never moves back.
func (st *State) Suppress(t time.Time) {
	if t.After(st.SuppressedThrough) {
		st.SuppressedThrough = t.UTC()
	}
}

// NextAttempt is when the pending batch of day may be sent.
func (st *State) NextAttempt(day string) time.Time {
	d := st.Days[day]
	if d == nil {
		return time.Time{}
	}
	if gap := st.LastPost.Add(PostGap); d.Attempts == 0 && gap.After(d.NotBefore) {
		return gap
	}
	return d.NotBefore
}

// ReadState reads state.json from the telemetry directory; a missing file
// is an empty state.
func ReadState(dir *os.Root) (*State, error) {
	st := &State{}
	raw, err := ident.ReadPrivateFile(dir, "state.json", 64<<10)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err == nil {
		if err := json.Unmarshal(raw, st); err != nil {
			return nil, fmt.Errorf("telemetry/state.json: %w", err)
		}
	}
	if st.Days == nil {
		st.Days = make(map[string]*DayState)
	}
	return st, nil
}

// WriteState replaces state.json atomically.
func WriteState(dir *os.Root, st *State) error {
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return writeFile(dir, "state.json", raw)
}

// WriteDayFile writes sub/<day>.json, creating the private directory sub.
func WriteDayFile(dir *os.Root, sub, day string, data []byte) error {
	if err := dir.Mkdir(sub, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	child, err := ident.OpenPrivateChild(dir, sub)
	if err != nil {
		return err
	}
	defer child.Close()
	return writeFile(child, day+".json", data)
}

// ReadDayFile reads sub/<day>.json.
func ReadDayFile(dir *os.Root, sub, day string) ([]byte, error) {
	child, err := ident.OpenPrivateChild(dir, sub)
	if err != nil {
		return nil, err
	}
	defer child.Close()
	return ident.ReadPrivateFile(child, day+".json", MaxReport)
}

// ListDays returns the days of sub's day files in ascending order.
func ListDays(dir *os.Root, sub string) ([]string, error) {
	child, err := ident.OpenPrivateChild(dir, sub)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer child.Close()
	entries, err := fs.ReadDir(child.FS(), ".")
	if err != nil {
		return nil, err
	}
	var days []string
	for _, entry := range entries {
		name, ok := strings.CutSuffix(entry.Name(), ".json")
		if _, err := time.Parse(time.DateOnly, name); ok && err == nil && entry.Type().IsRegular() {
			days = append(days, name)
		}
	}
	slices.Sort(days)
	return days, nil
}

// Pending is the batch the dispatcher sends next, for owner telemetry.
type Pending struct {
	Day         string
	Body        []byte
	NextAttempt time.Time
}

// ReadPending returns the dispatcher's state and the batch it sends next,
// chosen as the dispatcher chooses it, without changing anything.
func ReadPending(stateRoot string) (*State, *Pending, error) {
	dir, err := openTelemetryDir(stateRoot, false)
	if errors.Is(err, os.ErrNotExist) {
		return &State{Days: map[string]*DayState{}}, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	defer dir.Close()
	st, err := ReadState(dir)
	if err != nil {
		return nil, nil, err
	}
	setting, err := readMode(dir)
	if err != nil || setting.Mode != On {
		return st, nil, err
	}
	days, err := ListDays(dir, "pending")
	if err != nil {
		return nil, nil, err
	}
	eligible := EligibleFrom(setting, st)
	for _, day := range days {
		if !st.Sendable(day, eligible) {
			continue
		}
		body, err := ReadDayFile(dir, "pending", day)
		if err != nil {
			return nil, nil, err
		}
		return st, &Pending{Day: day, Body: body, NextAttempt: st.NextAttempt(day)}, nil
	}
	return st, nil, nil
}

func writeFile(dir *os.Root, name string, data []byte) error {
	stage := name + "-" + rand.Text() + ".part"
	file, err := dir.OpenFile(stage, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer dir.Remove(stage)
	_, err = file.Write(data)
	if err := errors.Join(err, file.Sync(), file.Close()); err != nil {
		return err
	}
	return dir.Rename(stage, name)
}
