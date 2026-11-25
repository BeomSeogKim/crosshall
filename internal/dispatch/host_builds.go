package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/hook"
	"github.com/BeomSeogKim/crosshall/internal/hook/manifests"
	"github.com/BeomSeogKim/crosshall/internal/setup"
	"github.com/BeomSeogKim/crosshall/internal/spool"
)

// hostBuildsRefreshed holds, in the state root, the versions the dispatcher
// read again from the executables setup recorded after they changed.
const hostBuildsRefreshed = "host-builds.json"

const (
	hostBuildEvery = 5 * time.Second
	// The version command of a just-updated executable can take a second
	// to start.
	hostBuildProbe = 2 * time.Second
)

// recordedHostBuild is setup.RecordedBuild; tests replace it.
var recordedHostBuild = setup.RecordedBuild

type hostBuilds struct {
	running   atomic.Bool
	workers   sync.WaitGroup
	next      time.Time
	published string
}

// hostBuildAdapters are the adapters whose hooks take their build from the
// executable that runs them.
func hostBuildAdapters() []string {
	var out []string
	for _, adapter := range manifests.Names() {
		if hook.BuildFromHost(adapter) {
			out = append(out, adapter)
		}
	}
	return out
}

func readRefreshedBuilds(state *os.Root) map[string]setup.HostBuild {
	refreshed := map[string]setup.HostBuild{}
	if text, err := readAdapterText(state, hostBuildsRefreshed); err == nil {
		_ = json.Unmarshal([]byte(text), &refreshed)
	}
	return refreshed
}

// effectiveHostBuild is the newest build known for the executable setup
// recorded: a refresh of that same path, else setup's record. current
// reports that the file is still that build.
func effectiveHostBuild(stateRoot string, state *os.Root, adapter string) (b setup.HostBuild, recorded, current bool) {
	b, recorded = recordedHostBuild(stateRoot, adapter)
	if !recorded {
		return b, false, false
	}
	if newer, ok := readRefreshedBuilds(state)[adapter]; ok && newer.Executable == b.Executable && newer.Current() {
		return newer, true, true
	}
	return b, true, b.Current()
}

// hostBuildText lists every recorded build whose file is unchanged.
func hostBuildText(stateRoot string, state *os.Root) string {
	var out strings.Builder
	for _, adapter := range hostBuildAdapters() {
		b, _, current := effectiveHostBuild(stateRoot, state, adapter)
		if current && b.Version != "" && b.Version != "unknown" && !strings.ContainsAny(b.Executable, "\n") {
			out.WriteString(hook.HostBuildLine(adapter, b.Version, b.Identity, b.Size, b.ModifiedNS, b.Executable))
		}
	}
	return out.String()
}

func (s *server) publishHostBuilds(root *os.Root) error {
	text := hostBuildText(s.cfg.StateRoot, s.state)
	if err := publishAdapterText(root, hook.HostBuildsName, text); err != nil {
		return err
	}
	s.builds.published = text
	return nil
}

// hostBuildTick republishes the builds when they change and, off the hook
// path, reads the version again of a recorded executable that changed:
// only the path setup recorded, never one a hook names.
func (s *server) hostBuildTick(ctx context.Context, now time.Time) {
	if now.Before(s.builds.next) {
		return
	}
	s.builds.next = now.Add(hostBuildEvery)
	if text := hostBuildText(s.cfg.StateRoot, s.state); text != s.builds.published {
		if root, err := spool.OpenServerRoot(s.cfg.Rendezvous); err == nil {
			if err := s.publishHostBuilds(root); err != nil {
				s.logFailure(err)
			}
			root.Close()
		}
	}
	var stale []string
	for _, adapter := range hostBuildAdapters() {
		if _, recorded, current := effectiveHostBuild(s.cfg.StateRoot, s.state, adapter); recorded && !current {
			stale = append(stale, adapter)
		}
	}
	if len(stale) == 0 || !s.builds.running.CompareAndSwap(false, true) {
		return
	}
	s.builds.workers.Add(1)
	go func() {
		defer s.builds.workers.Done()
		defer s.builds.running.Store(false)
		refreshed := readRefreshedBuilds(s.state)
		changed := false
		for _, adapter := range stale {
			recorded, ok := recordedHostBuild(s.cfg.StateRoot, adapter)
			if !ok {
				continue
			}
			if b, ok := setup.Reprobe(ctx, adapter, recorded, hostBuildProbe); ok {
				refreshed[adapter], changed = b, true
			}
		}
		if !changed {
			return
		}
		data, err := json.Marshal(refreshed)
		if err == nil {
			err = publishAdapterText(s.state, hostBuildsRefreshed, string(data))
		}
		// The main loop owns s.logFailure's state; this worker logs alone.
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("dispatcher: refresh host builds: %v", err)
		}
	}()
}
