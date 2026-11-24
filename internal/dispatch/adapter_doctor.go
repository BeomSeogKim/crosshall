package dispatch

import (
	"github.com/BeomSeogKim/crosshall/internal/hook/manifests"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/hook"
	"github.com/BeomSeogKim/crosshall/internal/setup"
	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

type doctorAdapter struct {
	setup.InstallStatus
	Policy             *store.AdapterPolicy       `json:"policy,omitempty"`
	Evidence           []store.DoctorEvidence     `json:"evidence,omitempty"`
	UnverifiedLaunches *int                       `json:"unverified_launches,omitempty"`
	UnseenStarts       *int                       `json:"unseen_starts,omitempty"`
	Hooks              *doctorHookActivity        `json:"hooks,omitempty"`
	TurnEndNotice      *store.DoctorTurnEndNotice `json:"turn_end_notice,omitempty"`
	Publish            *store.DoctorPublishState  `json:"publish,omitempty"`
}

type doctorHookActivity struct {
	Date            string         `json:"date"`
	Counts          map[string]int `json:"counts"`
	P95MS           int            `json:"p95_ms"`
	RecordedStarts  int            `json:"recorded_starts"`
	HelperTimeouts  int            `json:"helper_timeouts"`
	MissedDeadlines int            `json:"missed_deadlines"`
	VendorTimeouts  *int           `json:"vendor_timeouts"` // The vendor's backstop is not observable here.
}

func localDoctorAdapters(stateRoot string) []doctorAdapter {
	installed := setup.Inspect(stateRoot)
	adapters := make([]doctorAdapter, len(installed))
	for i, status := range installed {
		adapters[i].InstallStatus = status
	}
	return adapters
}

// hookRoutingWarning explains the state routing of the hooks of an adapter
// whose sessions can run in a shared managed server.
func hookRoutingWarning(adapter, routing string) string {
	name := manifests.DisplayName(adapter)
	shared := name + " hooks can run in a shared managed daemon and inherit its starting environment. "
	switch routing {
	case "pinned":
		return shared + "The hook commands on disk pin the Crosshall state root selected at setup. Use that root for member commands in this " + name + " home."
	case "inherited":
		return shared + "The hook commands on disk use that environment for state routing. Run crosshall setup " + adapter + " to pin a root."
	case "mixed":
		return shared + "The hook commands on disk mix state roots or routing styles. Review them before running crosshall setup " + adapter + "."
	case "uncertain":
		return shared + "A recognized hook command on disk has uncertain state routing. Review it before running crosshall setup " + adapter + "."
	default:
		return ""
	}
}

func attachDoctorHooks(adapters []doctorAdapter, rv *os.Root, now time.Time) {
	const maxCounterBytes = 8 << 20
	dir, err := spool.OpenServerChild(rv, "hooks", false)
	if err != nil {
		return
	}
	defer dir.Close()
	date := now.UTC().Format("20060102")
	file, err := spool.OpenServerFile(dir, date, os.O_RDONLY)
	if err != nil {
		return
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxCounterBytes+1))
	if err != nil || len(raw) > maxCounterBytes {
		return
	}
	counts := make(map[string]map[string]int, len(adapters))
	times := make(map[string][]int, len(adapters))
	missed := make(map[string]int, len(adapters))
	for _, adapter := range adapters {
		counts[adapter.Adapter] = make(map[string]int)
	}
	records := string(raw)
	for records != "" {
		line, remaining, complete := strings.Cut(records, "\n")
		if !complete {
			break
		}
		records = remaining
		if len(line) >= 64 {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 4 || !validHookOutcome(fields[2]) || counts[fields[0]] == nil {
			continue
		}
		ms, err := strconv.Atoi(fields[3])
		if err != nil || ms < 0 {
			continue
		}
		adapter := fields[0]
		counts[adapter][fields[2]]++
		times[adapter] = append(times[adapter], ms)
		// A callback missed its deadline past its own budget, or when it
		// ran out of time past the ordinary one: a prompt that enrolls
		// nothing gives back the start budget, and the counter does not
		// record which budget it ran on.
		if ms >= int(hook.CallbackBudget(adapter, fields[1]).Milliseconds()) ||
			(fields[2] == "deadline" && ms >= int(hook.Budget.Milliseconds())) {
			missed[adapter]++
		}
	}
	for i := range adapters {
		adapter := &adapters[i]
		latencies := times[adapter.Adapter]
		if len(latencies) == 0 {
			continue
		}
		slices.Sort(latencies)
		p95 := latencies[(95*len(latencies)+99)/100-1]
		adapter.Hooks = &doctorHookActivity{
			Date: date, Counts: counts[adapter.Adapter], P95MS: p95,
			RecordedStarts: len(latencies), HelperTimeouts: counts[adapter.Adapter]["deadline"],
			MissedDeadlines: missed[adapter.Adapter],
		}
	}
}

func validHookOutcome(outcome string) bool {
	switch outcome {
	case "off", "unmatched", "identity", "clean", "rpc", "deadline", "refused", "stale_epoch", "no_dispatcher":
		return true
	}
	return false
}
