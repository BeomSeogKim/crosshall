package hook

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/cli"
	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/notice"
	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

// Budget is the absolute callback deadline from command entry.
const Budget = 150 * time.Millisecond

type hookTransport interface {
	HookLookup(context.Context, ident.Identity, string) (string, wire.Response, error)
	HookCall(context.Context, ident.Identity, string, wire.Request, bool) (spool.Reply, error)
}

// Run serves one callback. Path resolution and counter preparation share the
// callback budget with dispatch. Only the supervisor records an outcome.
func Run(entry time.Time, args []string, stdin io.Reader, stdout io.Writer, locate func() (Paths, error)) {
	if len(args) != 2 {
		return
	}
	began := entry
	startup := prepareStartupRepo(entry, args, stdin, locate)
	entry, startedState := wake(entry, args, locate)
	end := entry.Add(Budget)
	var stateRoot string
	prepare := func() (*os.Root, *hookCounter) {
		paths, err := locate()
		if err != nil || !time.Now().Before(end) {
			return nil, nil
		}
		stateRoot = paths.State
		rv, err := ident.OpenPrivateDir(paths.Rendezvous)
		if err != nil || !time.Now().Before(end) {
			return rv, nil
		}
		return rv, openHookCounter(rv, args[0], args[1], entry)
	}
	var outcome atomic.Value
	wrote := runTimed(entry, stdout, prepare, func(ctx context.Context, rv *os.Root, output io.Writer) string {
		client := spool.Client{Rendezvous: rv.Name()}
		o := dispatchCallback(ctx, entry, rv, client, args, stdin, output, os.LookupEnv, callbackState{stateRoot, startup})
		outcome.Store(o)
		return o
	})
	if startup != nil && time.Now().Before(began.Add(Budget)) {
		timer := time.NewTimer(time.Until(began.Add(Budget)))
		defer timer.Stop()
		select {
		case <-startup.done:
		case <-timer.C:
		}
	}
	if startedState != "" {
		if o, _ := outcome.Load().(string); o == "" || o == "deadline" {
			logLine(startedState, args[0]+" "+args[1], began, "started the dispatcher, but the session start was not answered in time")
		}
	}
	if !wrote && args[0] == "agy" && time.Now().Before(end) {
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = io.WriteString(stdout, "{}\n")
		}()
		timer := time.NewTimer(time.Until(end))
		defer timer.Stop()
		select {
		case <-done:
		case <-timer.C:
		}
	}
}

type callbackState struct {
	root    string
	startup *parsedStartup
}

func dispatchEvent(ctx context.Context, entry time.Time, rv *os.Root, client hookTransport, args []string, stdin io.Reader, stdout io.Writer, lookup func(string) (string, bool)) string {
	return dispatchCallback(ctx, entry, rv, client, args, stdin, stdout, lookup, callbackState{})
}

func dispatchCallback(ctx context.Context, entry time.Time, rv *os.Root, client hookTransport, args []string, stdin io.Reader, stdout io.Writer, lookup func(string) (string, bool), state callbackState) string {
	if len(args) != 2 {
		return "refused"
	}
	m, ok := loadManifest(args[0])
	spec, known := m.event(args[1])
	if !ok || !known {
		return "refused"
	}
	policy, err := readPolicy(rv, m.Adapter)
	if errors.Is(err, os.ErrNotExist) || (err == nil && !policy.Enabled) {
		return "off"
	}
	if err != nil {
		return "refused"
	}
	var e event
	if state.startup == nil {
		e, err = parseEvent(stdin, m, spec, lookup)
	} else {
		select {
		case <-state.startup.done:
			e, err = state.startup.event, state.startup.err
		case <-ctx.Done():
			return "deadline"
		}
	}
	if errors.Is(err, errHookIdentity) {
		return "identity"
	}
	if err != nil {
		return "refused"
	}
	if hookExpired(ctx) {
		return "deadline"
	}
	leaf, gen, repo, outcome := resolveLeaf(ctx, rv, state.root, client, spec, e)
	if outcome != "" {
		if outcome == "unmatched" && spec.Enroll {
			result := domain.UnmappedStart(e.Source, policy.Epoch)
			if failure := writeHookResult(ctx, rv, m.Adapter, policy.Epoch, spec, result, stdout); failure != "" {
				return failure
			}
		}
		return outcome
	}
	defer leaf.Close()
	if hookExpired(ctx) {
		return "deadline"
	}
	e.LaunchRef, e.LaunchVerified, err = bindLaunch(leaf, gen, e, spec, entry)
	if err != nil {
		return "refused"
	}
	notice := spec.Enroll || spec.Notice
	// The continuation needs a concrete profile. Unknown builds still
	// publish their lifecycle observation without waiting for a reply.
	turnEnd := spec.Stop && e.stopActive == "false" && e.VendorVersion != "unverified" && e.Mode != "unverified" &&
		turnEndListed(rv, m.Adapter, e.VendorVersion, runtime.GOOS+"-"+runtime.GOARCH, e.Mode, m.Revision)
	clean := !spec.Enroll && notice && stampClean(rv, leaf, gen, "notice", "")
	if spec.RecoverEnroll && !clean && repo == "" && e.CWD != "" && !hasSeenNotice(leaf, gen) {
		repo = mappedEnrollmentRoot(ctx, rv, state.root, e)
	}
	want := turnEnd || (notice && !clean)
	req := makeRequest(entry, m, spec, e, repo, want, policy.Epoch)
	if dispatcherGone(rv) {
		return "no_dispatcher" // a stopped dispatcher must not cost the turn the deadline
	}
	if hookExpired(ctx) {
		return "deadline"
	}
	reply, err := client.HookCall(ctx, e.Identity, gen, req, !want)
	if err != nil {
		return transportOutcome(ctx, err)
	}
	if clean || !want {
		if !hookExpired(ctx) {
			if routeReq, ok := makeRouteRequest(entry, m, e, policy.Epoch); ok {
				_, _ = client.HookCall(ctx, e.Identity, gen, routeReq, true)
			}
		}
		return "clean" // published without waiting for a response
	}
	if reply.Response.Status != core.ExitOK {
		return "refused"
	}
	var result wire.HookResult
	if err := json.Unmarshal(reply.Response.Data, &result); err != nil {
		return "refused"
	}
	if failure := writeHookResult(ctx, rv, m.Adapter, policy.Epoch, spec, result, stdout); failure != "" {
		return failure
	}
	if notice && result.Ack != nil && !hookExpired(ctx) {
		_ = saveSeen(rv, leaf, gen, "notice", "", *result.Ack)
	}
	if !hookExpired(ctx) {
		if routeReq, ok := makeRouteRequest(entry, m, e, policy.Epoch); ok {
			_, _ = client.HookCall(ctx, e.Identity, gen, routeReq, true)
		}
	}
	return "rpc"
}

func writeHookResult(ctx context.Context, rv *os.Root, adapter string, epoch uint64, spec eventSpec, result wire.HookResult, stdout io.Writer) string {
	output, err := present(spec, result)
	if err != nil {
		return "refused"
	}
	current, err := readPolicy(rv, adapter)
	if err != nil || !current.Enabled || current.Epoch != epoch || result.Epoch != current.Epoch {
		return "stale_epoch"
	}
	if hookExpired(ctx) {
		return "deadline"
	}
	if len(output) > 0 {
		if _, err := stdout.Write(output); err != nil {
			return "refused"
		}
	}
	return ""
}

func transportOutcome(ctx context.Context, err error) string {
	if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
		return "deadline"
	}
	if errors.Is(err, os.ErrNotExist) {
		return "no_dispatcher"
	}
	return "refused"
}

func resolveLeaf(ctx context.Context, rv *os.Root, stateRoot string, client hookTransport, spec eventSpec, e event) (*os.Root, string, string, string) {
	leaf, gen, err := spool.OpenHookLeaf(rv, e.Identity)
	if err == nil {
		var repo string
		if spec.Enroll {
			repo = enrollmentRoot(e)
		}
		return leaf, gen, repo, ""
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, "", "", "refused"
	}
	if !spec.Enroll && !spec.RecoverEnroll {
		return nil, "", "", "unmatched"
	}
	if dispatcherGone(rv) {
		return nil, "", "", "no_dispatcher"
	}
	if hookExpired(ctx) {
		return nil, "", "", "deadline"
	}
	intent := "lookup"
	var repo string
	if spec.RecoverEnroll {
		repo = mappedEnrollmentRoot(ctx, rv, stateRoot, e)
		if hookExpired(ctx) {
			return nil, "", "", "deadline"
		}
		if repo == "" {
			return nil, "", "", "unmatched"
		}
		intent = "join"
	}
	gen, resp, err := client.HookLookup(ctx, e.Identity, intent)
	if err != nil {
		return nil, "", "", transportOutcome(ctx, err)
	}
	newSession := resp.Status == core.ExitNotEnrolled && resp.Code == core.CodeNotEnrolled
	if resp.Status != core.ExitOK && !newSession {
		return nil, "", "", "refused"
	}
	if spec.Enroll {
		repo = enrollmentRoot(e)
	}
	if newSession && spec.Enroll {
		match, err := hasDefault(ctx, rv, repo)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, "", "", transportOutcome(ctx, err)
		}
		if !match {
			return nil, "", "", "unmatched"
		}
		if hookExpired(ctx) {
			return nil, "", "", "deadline"
		}
		gen, resp, err = client.HookLookup(ctx, e.Identity, "join")
		if err != nil {
			return nil, "", "", transportOutcome(ctx, err)
		}
	}
	if resp.Status != core.ExitOK {
		return nil, "", "", "refused"
	}
	leaf, current, err := spool.OpenHookLeaf(rv, e.Identity)
	if err != nil {
		return nil, "", "", transportOutcome(ctx, err)
	}
	if current != gen {
		leaf.Close()
		return nil, "", "", "refused"
	}
	return leaf, gen, repo, ""
}

func enrollmentRoot(e event) string {
	if e.startupRepo != nil {
		return *e.startupRepo
	}
	if e.CWD == "" {
		return ""
	}
	root, _ := cli.RepoRoot(e.CWD)
	return root
}

func mappedEnrollmentRoot(ctx context.Context, rv *os.Root, stateRoot string, e event) string {
	if hookExpired(ctx) {
		return ""
	}
	startup := startupRepoDigest(stateRoot, e.Identity)
	if len(startup) != sha256.Size*2+1 {
		return ""
	}
	repo := enrollmentRoot(e)
	if repo == "" || startup != repoDigestRecord(repo) {
		return ""
	}
	if match, _ := hasDefault(ctx, rv, repo); !match {
		return ""
	}
	return repo
}

func makeRequest(entry time.Time, m manifest, spec eventSpec, e event, repo string, want bool, epoch uint64) wire.Request {
	ref := e.reference(entry)
	// The launch's build and mode, as the hook's environment states them;
	// "unverified" where the manifest names no source or it does not parse.
	profile := wire.HookProfile{
		Adapter: m.Adapter, ManifestRev: m.Revision, Platform: runtime.GOOS + "-" + runtime.GOARCH,
		VendorVersion: e.VendorVersion, Mode: e.Mode,
	}
	if profile.VendorVersion == "" {
		profile.VendorVersion = "unverified"
	}
	if profile.Mode == "" {
		profile.Mode = "unverified"
	}
	req := wire.Request{OpKey: "hook:" + m.Adapter + ":" + e.LaunchRef + ":" + ref}
	if spec.Enroll {
		req.Verb = wire.VerbHookEnroll
		req.Args, _ = json.Marshal(wire.HookEnrollArgs{
			Profile: profile, LaunchRef: e.LaunchRef, LaunchVerified: e.LaunchVerified, EventRef: ref,
			Source: e.Source, RepoRoot: repo, HookAtMS: entry.UnixMilli(), Epoch: epoch,
		})
	} else {
		req.Verb = wire.VerbHookEvent
		if !want {
			repo = ""
		}
		req.Args, _ = json.Marshal(wire.HookEventArgs{
			Profile: profile, LaunchRef: e.LaunchRef, LaunchVerified: e.LaunchVerified, EventRef: ref,
			Fact: e.Fact, TurnRef: e.TurnRef, HookAtMS: entry.UnixMilli(), WantOutput: want, Epoch: epoch,
			EnrollRepoRoot:     repo,
			Receipt:            e.receipt,
			ContinuationActive: e.stopActive == "true",
		})
	}
	return req
}

func makeRouteRequest(entry time.Time, m manifest, e event, epoch uint64) (wire.Request, bool) {
	if e.SocketPath == "" && e.PermissionMode == "" {
		return wire.Request{}, false
	}
	ref := e.reference(entry)
	args, _ := json.Marshal(wire.HookRouteArgs{
		Adapter:        m.Adapter,
		LaunchRef:      e.LaunchRef,
		LaunchVerified: e.LaunchVerified,
		EventRef:       ref,
		HookAtMS:       entry.UnixMilli(),
		Epoch:          epoch,
		PermissionMode: e.PermissionMode,
		SocketPath:     e.SocketPath,
	})
	return wire.Request{
		OpKey: "hook:route:" + m.Adapter + ":" + e.LaunchRef + ":" + ref,
		Verb:  wire.VerbHookRoute,
		Args:  args,
	}, true
}

// present renders only the fields appropriate for this boundary.
func present(spec eventSpec, result wire.HookResult) ([]byte, error) {
	if spec.Stop {
		return presentTurnEnd(spec, result.TurnEndLine)
	}
	if (spec.Output != "context" && spec.Output != "inject_steps") || !(spec.Enroll || spec.Notice) {
		return nil, nil
	}
	var parts []string
	if spec.Enroll || spec.RecoverEnroll && result.Enrollment == "joined" {
		switch result.Enrollment {
		case "joined":
			if result.Hint != "" {
				parts = append(parts, result.Hint)
			}
			if len(result.Roster) != 0 {
				var members []domain.Member
				if err := json.Unmarshal(result.Roster, &members); err != nil {
					return nil, err
				}
				if len(members) > 0 {
					var roster strings.Builder
					cli.RenderRoster(&roster, result.Channel, "", members, result.Omitted)
					parts = append(parts, strings.TrimRight(roster.String(), "\n"))
				}
			}
		case "restored":
			// After a context reset the core sends a short cue in place of
			// what the session was already given.
			if result.Cue != "" {
				parts = append(parts, result.Cue)
			}
		case "none":
			if result.Hint == "" {
				return nil, nil
			}
			return presentBoundary(spec, result.Hint)
		default:
			return nil, nil
		}
	}
	if result.PendingLine != "" {
		parts = append(parts, result.PendingLine)
	}
	if len(parts) == 0 {
		return nil, nil
	}
	return presentBoundary(spec, strings.Join(parts, "\n"))
}

func presentBoundary(spec eventSpec, text string) ([]byte, error) {
	if spec.Output == "inject_steps" {
		type systemMessage struct {
			Text string `json:"systemMessage"`
		}
		type step struct {
			SystemMessage systemMessage `json:"systemMessage"`
		}
		payload := struct {
			Steps []step `json:"injectSteps"`
		}{Steps: []step{{SystemMessage: systemMessage{Text: text}}}}
		b, err := json.Marshal(payload)
		return append(b, '\n'), err
	}
	return presentContext(spec.Name, text)
}

func presentContext(event, text string) ([]byte, error) {
	type contextOutput struct {
		Event   string `json:"hookEventName"`
		Context string `json:"additionalContext"`
	}
	payload := struct {
		Output contextOutput `json:"hookSpecificOutput"`
	}{contextOutput{event, text}}
	b, err := json.Marshal(payload)
	return append(b, '\n'), err
}

func presentTurnEnd(spec eventSpec, line string) ([]byte, error) {
	if line == "" {
		return nil, nil
	}
	ids, ok := strings.CutPrefix(line, notice.PendingPrefix)
	if !ok || notice.ParseIDs(ids) == nil {
		return nil, errors.New("invalid turn-end line")
	}
	switch spec.Output {
	case "context":
		return presentContext(spec.Name, line)
	case "block":
		payload := struct {
			Decision string `json:"decision"`
			Reason   string `json:"reason"`
		}{"block", line}
		b, err := json.Marshal(payload)
		return append(b, '\n'), err
	default:
		return nil, nil
	}
}

// dispatcherGone reports whether the dispatcher has stopped serving this
// rendezvous. The hook then publishes nothing and does not wait.
func dispatcherGone(rv *os.Root) bool {
	raw, err := ident.ReadPrivateFile(rv, "ready", 4096)
	if err != nil {
		return true
	}
	var marker struct {
		Instance string `json:"instance"`
		PID      int    `json:"pid"`
		Lease    bool   `json:"lease"`
	}
	if json.Unmarshal(raw, &marker) != nil || marker.PID <= 0 {
		return true
	}
	if marker.Lease {
		held, err := spool.ReadyLeaseHeld(rv, marker.Instance)
		if !errors.Is(err, os.ErrPermission) {
			return !held
		}
	}
	// Older dispatchers omit the lease. A sandbox that denies inspection may
	// still reach the dispatcher, so retain the legacy PID check there too.
	return errors.Is(syscall.Kill(marker.PID, 0), syscall.ESRCH)
}
