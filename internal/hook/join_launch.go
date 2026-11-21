package hook

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

// JoinLaunch is the launch a hand join proposes when the session's adapter
// names its process in launch_env, as a SessionStart would bind it. The
// dispatcher binds it only if that process runs.
func JoinLaunch(id ident.Identity, lookup func(string) (string, bool), at time.Time) *domain.JoinLaunch {
	m, ok := loadManifest(id.Vendor)
	if !ok || m.LaunchEnv == "" {
		return nil
	}
	value, present := lookup(m.LaunchEnv)
	pid, err := strconv.ParseUint(value, 10, 32)
	if !present || err != nil || pid == 0 {
		return nil
	}
	version, mode := launchBuild(m, lookup)
	digest := sha256.Sum256([]byte("join:" + strconv.FormatInt(at.UnixMilli(), 10)))
	return &domain.JoinLaunch{
		Ref:  fmt.Sprintf("pid-%d-%x", pid, digest),
		AtMS: at.UnixMilli(),
		Profile: wire.HookProfile{Adapter: m.Adapter, ManifestRev: m.Revision, VendorVersion: version,
			Platform: runtime.GOOS + "-" + runtime.GOARCH, Mode: mode},
	}
}

// BindJoinLaunch writes the launch the dispatcher bound for a join into the
// session's member directory, where its hooks read it, unless a start bound
// a later one there first.
func BindJoinLaunch(rendezvous string, id ident.Identity, l domain.JoinLaunch) error {
	rest, ok := strings.CutPrefix(l.Ref, "pid-")
	pid, digest, cut := strings.Cut(rest, "-")
	if !ok || !cut {
		return errors.New("invalid launch reference")
	}
	rv, err := ident.OpenPrivateDir(rendezvous)
	if err != nil {
		return err
	}
	defer rv.Close()
	leaf, gen, err := spool.OpenHookLeaf(rv, id)
	if err != nil {
		return err
	}
	defer leaf.Close()
	name := fmt.Sprintf("launch-%s-%x", pid, sha256.Sum256([]byte(gen)))
	guard, err := spool.OpenServerFile(leaf, name+".lock", os.O_CREATE|os.O_RDWR)
	if err != nil {
		return err
	}
	defer guard.Close()
	if err := syscall.Flock(int(guard.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	if snapshot, err := ident.ReadPrivateFile(leaf, name, 96); err == nil {
		when, _, _ := strings.Cut(string(snapshot), " ")
		if latest, err := strconv.ParseInt(when, 10, 64); err == nil && latest >= l.AtMS {
			return nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := writeRecord(leaf, name, fmt.Sprintf("%d %s\n", l.AtMS, digest)); err != nil {
		return err
	}
	markLaunch(rv, pid)
	return nil
}
