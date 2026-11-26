package dispatch

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/link"
	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

const (
	spokeConfigFile = "link.json"
	spokeEnrollFile = "link-enrollment.json"
	spokeStatusFile = "link-status.json"
)

type linkEnrollment struct {
	link.Endpoint
	Hall   string `json:"hall"`
	Name   string `json:"name"`
	Secret string `json:"secret"`
	ID     string `json:"id"`
	// Source is the ID of the stopped profile this enrollment replaces
	// (#400): only an enrollment begun for that very profile is resumed.
	Source string `json:"source,omitempty"`
}

type linkConfig struct {
	linkEnrollment
	Device string `json:"device"`
	// RolledBack marks a profile whose home was restored to an older
	// store: nothing dials with it, every command refuses, and linking
	// again replaces it.
	RolledBack bool `json:"rolled_back,omitempty"`
}

type spokeStatusRecord struct {
	Instance string `json:"instance"`
	ConfigID string `json:"config_id"`
	link.Status
}

func readLinkJSON(state *os.Root, name string, value any) error {
	file, err := spool.OpenServerFile(state, name, os.O_RDONLY)
	if err != nil {
		return err
	}
	defer file.Close()
	const limit = 8192
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return err
	}
	if len(data) > limit || json.Unmarshal(data, value) != nil {
		return fmt.Errorf("invalid %s", name)
	}
	return nil
}

func readLinkConfig(state *os.Root) (linkConfig, bool, error) {
	var cfg linkConfig
	if err := readLinkJSON(state, spokeConfigFile, &cfg); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return linkConfig{}, false, nil
		}
		return linkConfig{}, false, err
	}
	if !cfg.linkEnrollment.valid() || cfg.Device == "" {
		return linkConfig{}, false, errors.New("invalid link.json")
	}
	return cfg, true, nil
}

func (e linkEnrollment) valid() bool {
	secret, err := hex.DecodeString(e.Secret)
	return err == nil && len(secret) == 32 && domain.ValidName(e.Name) && e.Hall != "" && link.ValidNonce(e.ID) &&
		link.ValidateTarget(e.Target) == nil && path.IsAbs(e.Path) && !strings.ContainsFunc(e.Path, unicode.IsControl)
}

func (c linkConfig) spoke(release string) link.Spoke {
	secret, _ := hex.DecodeString(c.Secret)
	return link.Spoke{Target: c.Target, Hall: c.Hall, Device: c.Device, Secret: secret, Release: release, Link: link.Supported()}
}

// LinkStatus reads the configured spoke without contacting the home.
func (cfg LaunchConfig) LinkStatus() (link.Status, error) {
	unlinked := link.Status{State: "unlinked"}
	state, err := spool.OpenServerRoot(cfg.StateRoot)
	if errors.Is(err, os.ErrNotExist) {
		return unlinked, nil
	}
	if err != nil {
		return link.Status{}, err
	}
	defer state.Close()
	c, found, err := readLinkConfig(state)
	if err != nil || !found {
		return unlinked, err
	}
	status := link.Status{Target: c.Target, State: "stopped"}
	held, err := lockState(state)
	if err != nil || !held {
		return status, err
	}
	status.State = "connecting"
	marker := cfg.readinessCandidate()
	var current spokeStatusRecord
	if err := readLinkJSON(state, spokeStatusFile, &current); err != nil && !errors.Is(err, os.ErrNotExist) {
		return status, err
	}
	if marker.Instance != "" && marker.Spoke == c.Target && marker.Instance == current.Instance &&
		current.ConfigID == c.ID && current.Target == c.Target {
		return current.Status, nil
	}
	return status, nil
}

func (cfg LaunchConfig) LinkSpoke(ctx context.Context, target, name string, confirm func(link.Preview) error) (link.Status, error) {
	if err := link.ValidateTarget(target); err != nil {
		return link.Status{}, err
	}
	state, guard, err := cfg.lockLinkChange(ctx)
	if err != nil {
		return link.Status{}, err
	}
	defer state.Close()
	defer guard.Close()
	var replacing *linkConfig
	if c, found, err := readLinkConfig(state); err != nil {
		return link.Status{}, err
	} else if found {
		if c.Target != target || name != "" && c.Name != name {
			return link.Status{}, fmt.Errorf("already linked to %s as %s; run crosshall unlink first", c.Target, c.Name)
		}
		stopped := c.RolledBack
		if !stopped {
			if stopped, err = cfg.storeRolledBack(ctx, state, c); err != nil {
				return link.Status{}, err
			}
		}
		if !stopped {
			consumeEnrollment(state, c)
			return cfg.startSpoke(ctx)
		}
		// The home went back to an older store: the device enrolls again
		// on a fresh store. The new credential generation lets its
		// sessions register anew and make their first call without the
		// stamps the old store held.
		replacing, name = &c, c.Name
	}
	var pending linkEnrollment
	fresh := false
	switch err := readLinkJSON(state, spokeEnrollFile, &pending); {
	case errors.Is(err, os.ErrNotExist) || err == nil && replacing != nil && pending.Source != replacing.ID:
		// No enrollment, or one that was not begun for the stopped
		// profile (the one it came from, say): a new one.
		fresh = true
		name, err = localLinkName(name)
		if err != nil {
			return link.Status{}, err
		}
	case err != nil:
		return link.Status{}, err
	case !pending.valid():
		return link.Status{}, errors.New("invalid pending link enrollment; run crosshall unlink")
	case pending.Target != target || name != "" && pending.Name != name:
		return link.Status{}, errors.New("another enrollment is pending; retry the original link or run crosshall unlink")
	default:
		name = pending.Name
	}
	remote, err := link.DiscoverSSH(ctx, target)
	if err != nil {
		return link.Status{}, err
	}
	endpoint := link.Endpoint{Target: target, Path: remote}
	started := time.Now()
	challenge, err := link.ProbeSSH(ctx, endpoint)
	if err != nil {
		return link.Status{}, err
	}
	chosen, ok := link.Negotiate(link.Supported(), challenge.Link)
	if !ok {
		return link.Status{}, errors.New(link.Incompatible(
			link.Peer{Name: "this device", Release: cfg.Version, Link: link.Supported()},
			link.Peer{Name: target, Release: challenge.Release, Link: challenge.Link}))
	}
	if !fresh && (endpoint != pending.Endpoint || challenge.Hall != pending.Hall) {
		return link.Status{}, errors.New("the home changed after enrollment began; run crosshall unlink before linking again")
	}
	if err := confirm(link.Preview{Target: target, Name: name, LocalRelease: cfg.Version, HomeRelease: link.Printable(challenge.Release),
		Version: chosen, ClockOffsetMS: link.EstimateClockOffset(started, challenge.TimeMS)}); err != nil {
		return link.Status{}, err
	}
	if replacing != nil {
		// Steps 1 and 2: the profile carries the stop, so nothing dials
		// with it whatever happens next, then the stopped store goes aside.
		flagged := *replacing
		flagged.RolledBack = true
		if err := cfg.changeLinkMode(ctx, state, func() error {
			if err := spool.PublishServerFile(state, spokeConfigFile, flagged); err != nil {
				return err
			}
			if err := linkInterrupt(1); err != nil {
				return err
			}
			return cfg.archiveSpokeStore(state, flagged)
		}); err != nil {
			return link.Status{}, err
		}
		if err := linkInterrupt(2); err != nil {
			return link.Status{}, err
		}
	}
	if fresh {
		var secret [32]byte
		rand.Read(secret[:])
		pending = linkEnrollment{Endpoint: endpoint, Hall: challenge.Hall, Name: name, Secret: hex.EncodeToString(secret[:]), ID: link.NewNonce()}
		if replacing != nil {
			pending.Source = replacing.ID
		}
		if err := spool.PublishServerFile(state, spokeEnrollFile, pending); err != nil {
			return link.Status{}, err
		}
	}
	secret, _ := hex.DecodeString(pending.Secret)
	enrolled, err := link.EnrollSSH(ctx, endpoint, name, secret, pending.ID)
	if err != nil {
		return link.Status{}, fmt.Errorf("enrollment did not finish; retry the same link: %w", err)
	}
	if enrolled.Hall != challenge.Hall {
		return link.Status{}, errors.New("the home hall changed during enrollment; run crosshall link again")
	}
	if err := linkInterrupt(3); err != nil {
		return link.Status{}, err
	}
	c := linkConfig{linkEnrollment: pending, Device: enrolled.Device}
	conn, err := link.DialSSH(ctx, endpoint, c.spoke(cfg.Version))
	if err != nil {
		return link.Status{}, err
	}
	conn.Close()
	_ = conn.Wait()
	// Step 4. A store whose home rolled back (the same hall and device
	// name after an unlink) is not used again: the new profile goes
	// first, and if the move does not happen the store still refuses and
	// linking again enrolls again.
	stopped, err := cfg.storeRolledBack(ctx, state, c)
	if err != nil {
		return link.Status{}, err
	}
	if err := cfg.changeLinkMode(ctx, state, func() error {
		if err := spool.PublishServerFile(state, spokeConfigFile, c); err != nil || !stopped {
			return err
		}
		return cfg.archiveSpokeStore(state, c)
	}); err != nil {
		return link.Status{}, err
	}
	_ = state.Remove(spokeEnrollFile)
	return cfg.startSpoke(ctx)
}

func (cfg LaunchConfig) startSpoke(ctx context.Context) (link.Status, error) {
	if err := cfg.StartReady(ctx); err != nil {
		return link.Status{}, fmt.Errorf("link is configured but the spoke dispatcher could not start: %w", err)
	}
	return cfg.LinkStatus()
}

func (cfg LaunchConfig) UnlinkSpoke(ctx context.Context) error {
	state, guard, err := cfg.lockLinkChange(ctx)
	if err != nil {
		return err
	}
	defer state.Close()
	defer guard.Close()
	if err := state.Remove(spokeEnrollFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	profile, err := spool.OpenServerFile(state, spokeConfigFile, os.O_RDONLY)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	profile.Close()
	if err := cfg.changeLinkMode(ctx, state, func() error { return state.Remove(spokeConfigFile) }); err != nil {
		return err
	}
	return cfg.StartReady(ctx)
}

func (cfg LaunchConfig) lockLinkChange(ctx context.Context) (*os.Root, *os.File, error) {
	if !cfg.Owner {
		return nil, nil, errors.New("changing the link requires the owner")
	}
	state, err := cfg.openStartState(ctx)
	if err != nil {
		return nil, nil, err
	}
	guard, err := spool.OpenServerFile(state, "link.lock", os.O_CREATE|os.O_RDWR)
	if err == nil {
		err = syscall.Flock(int(guard.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err != nil {
			guard.Close()
		}
	}
	if err != nil {
		state.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			err = errors.New("another command is changing the link; try again")
		}
		return nil, nil, err
	}
	return state, guard, nil
}

func (cfg LaunchConfig) changeLinkMode(ctx context.Context, state *os.Root, change func() error) error {
	if _, err := cfg.Stop(ctx); err != nil {
		return err
	}
	lock, err := spool.OpenServerFile(state, "dispatcher.lock", os.O_CREATE|os.O_RDWR)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return errors.New("another command started the dispatcher during the link change; try again")
		}
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	root, err := spool.OpenServerRoot(cfg.Rendezvous)
	if err == nil {
		defer root.Close()
		err = root.Remove("ready")
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return change()
}

func localLinkName(name string) (string, error) {
	if name == "" {
		host, err := os.Hostname()
		if err != nil {
			return "", fmt.Errorf("read device hostname: %w", err)
		}
		label, _, _ := strings.Cut(strings.ToLower(host), ".")
		name = strings.Trim(strings.Map(func(r rune) rune {
			if 'a' <= r && r <= 'z' || '0' <= r && r <= '9' {
				return r
			}
			return '-'
		}, label), "-")
		name = strings.TrimRight(name[:min(len(name), 32)], "-")
	}
	if !domain.ValidName(name) {
		return "", errors.New("a device name must use 1–32 lowercase letters, digits or hyphens; set --name")
	}
	return name, nil
}

// storeRolledBack reads whether the profile's spoke store recorded a home
// rollback; a missing store recorded nothing.
func (cfg LaunchConfig) storeRolledBack(ctx context.Context, state *os.Root, c linkConfig) (bool, error) {
	name := filepath.Join("spoke", c.Hall+"-"+c.Device)
	if _, err := state.Lstat(filepath.Join(name, "store.sqlite3")); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	db, err := store.OpenSpoke(ctx, filepath.Join(cfg.StateRoot, name))
	if err != nil {
		return false, err
	}
	defer db.Close()
	return rolledBackFlag(ctx, db)
}

// archiveSpokeStore moves the profile's spoke store out of the way, kept,
// with its member rendezvous, so the dispatcher starts on a fresh one and
// sessions make first contact again (#400, v1). The dispatcher is stopped.
func (cfg LaunchConfig) archiveSpokeStore(state *os.Root, c linkConfig) error {
	name := filepath.Join("spoke", c.Hall+"-"+c.Device)
	suffix := fmt.Sprintf("-rolled-back-%d", time.Now().UnixMilli())
	members := ident.Rendezvous(filepath.Join(cfg.StateRoot, name), os.Geteuid())
	if err := os.Rename(members, members+suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := state.Rename(name, name+suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// consumeEnrollment removes an enrollment record whose result the profile
// already holds: it is done and never resumed.
func consumeEnrollment(state *os.Root, c linkConfig) {
	var done linkEnrollment
	if readLinkJSON(state, spokeEnrollFile, &done) == nil && done.ID == c.ID {
		_ = state.Remove(spokeEnrollFile)
	}
}
