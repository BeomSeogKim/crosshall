package hook

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/cli"
	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/notice"
	"github.com/BeomSeogKim/crosshall/internal/proc"
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
	lifecycle := prepareRepoLifecycle(args, stdin, locate)
	if lifecycle != nil {
		defer lifecycle.cancel()
	}
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
		o := dispatchCallback(ctx, entry, rv, client, args, stdin, output, os.LookupEnv, callbackState{stateRoot, lifecycle})
		outcome.Store(o)
		return o
	})
	if lifecycle != nil && !lifecycle.waitUntil(began.Add(Budget)) && args[1] == "SessionStart" && startedState == "" {
		lifecycle.waitUntil(time.Now().Add(startupRepoGrace))
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
	root      string
	lifecycle *parsedLifecycle
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
	if state.lifecycle == nil {
		e, err = parseEvent(stdin, m, spec, lookup)
	} else {
		select {
		case <-state.lifecycle.done:
			e, err = state.lifecycle.event, state.lifecycle.err
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
		// With automatic joining on, such a start is outside Git: no line.
		if outcome == "unmatched" && spec.Enroll && !autoJoin(ctx, rv) {
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
	pid := e.LaunchRef
	e.LaunchRef, e.LaunchVerified, err = bindLaunch(leaf, gen, e, spec, entry)
	if err != nil {
		return "refused"
	}
	if spec.Enroll && e.LaunchVerified {
		e.PIDNamespace = proc.PIDNamespace()
		markLaunch(rv, pid)
		// Only a /clear may continue the process's member; it must come from
		// that process, not from a session nested under it.
		if e.Source == "clear" {
			n, _ := strconv.Atoi(pid)
			e.LaunchHosted = proc.HostedBy(n)
		}
	}
	notice := spec.Enroll || spec.Notice
	// The continuation needs a concrete profile. Unknown builds still
	// publish their lifecycle observation without waiting for a reply.
	turnEnd := spec.Stop && e.stopActive == "false" && e.VendorVersion != "unverified" && e.Mode != "unverified" &&
		turnEndListed(rv, m.Adapter, runtime.GOOS+"-"+runtime.GOARCH, e.Mode, m.Revision)
	// A session the hall can publish to asks at every prompt: whether the
	// hall still reaches it changes without a notice to stamp (#121).
	clean := !spec.Enroll && notice && e.SocketPath == "" && stampClean(rv, leaf, gen, "notice", "")
	if spec.RecoverEnroll && !clean && repo.none() && e.CWD != "" && !hasSeenNotice(leaf, gen) {
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
	failure, joinHintSeen := writeHookResultAndConfirmJoin(ctx, rv, leaf, gen, m.Adapter, policy.Epoch, spec, result, stdout)
	if failure != "" {
		return failure
	}
	reachSeen := confirmReach(ctx, leaf, gen, spec, result)
	// The one acknowledgement request applies the start line and the reach line.
	if (joinHintSeen || reachSeen) && !hookExpired(ctx) {
		_, _ = client.HookCall(ctx, e.Identity, gen, wire.Request{Verb: wire.VerbHookJoinHintAck, OpKey: wire.VerbHookJoinHintAck}, true)
	}
	if notice && result.Ack != nil && (result.JoinHintAck == nil || joinHintSeen) && !hookExpired(ctx) {
		_ = saveSeen(rv, leaf, gen, "notice", "", *result.Ack)
	}
	if !hookExpired(ctx) {
		if routeReq, ok := makeRouteRequest(entry, m, e, policy.Epoch); ok {
			_, _ = client.HookCall(ctx, e.Identity, gen, routeReq, true)
		}
	}
	return "rpc"
}

func writeHookResultAndConfirmJoin(ctx context.Context, rv, leaf *os.Root, generation, adapter string, epoch uint64,
	spec eventSpec, result wire.HookResult, stdout io.Writer) (string, bool) {
	ack := result.JoinHintAck
	if ack == nil {
		return writeHookResult(ctx, rv, adapter, epoch, spec, result, stdout), false
	}
	if !ack.ValidFor(generation) || !joinHintBoundary(spec) || result.Hint == "" ||
		!CurrentJoinHintOffer(leaf, *ack) {
		return writeHookResult(ctx, rv, adapter, epoch, spec, withoutJoinHint(result), stdout), false
	}
	lock, err := spool.OpenServerFile(leaf, joinHintLock, os.O_CREATE|os.O_RDWR)
	if err != nil {
		return writeHookResult(ctx, rv, adapter, epoch, spec, withoutJoinHint(result), stdout), false
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return writeHookResult(ctx, rv, adapter, epoch, spec, withoutJoinHint(result), stdout), false
	}
	if prior, ok := ReadJoinHintAck(leaf, generation); ok && prior.SameOffer(*ack) {
		return writeHookResult(ctx, rv, adapter, epoch, spec, withoutJoinHint(result), stdout), true
	}
	output, failure := prepareHookOutput(rv, adapter, epoch, spec, result)
	if failure != "" {
		return failure, false
	}
	if !CurrentJoinHintOffer(leaf, *ack) {
		return writeHookResult(ctx, rv, adapter, epoch, spec, withoutJoinHint(result), stdout), false
	}
	if failure := writeHookOutput(ctx, stdout, output); failure != "" {
		return failure, false
	}
	if hookExpired(ctx) {
		return "deadline", false
	}
	raw, _ := json.Marshal(ack)
	return "", writeRecord(leaf, seenJoinHint, string(raw)+"\n") == nil
}

// confirmReach records a printed reach line for the dispatcher to store as
// told. A line that was not printed stays offered.
func confirmReach(ctx context.Context, leaf *os.Root, generation string, spec eventSpec, result wire.HookResult) bool {
	ack := result.ReachAck
	if ack == nil || !reachShown(spec, result) || !ack.ValidFor(generation) || !CurrentReachOffer(leaf, *ack) || hookExpired(ctx) {
		return false
	}
	raw, _ := json.Marshal(ack)
	return writeRecord(leaf, seenReach, string(raw)+"\n") == nil
}

func reachShown(spec eventSpec, result wire.HookResult) bool {
	return result.ReachLine != "" && !spec.Stop && (spec.Output == "context" || spec.Output == "inject_steps") &&
		(spec.Enroll && result.Enrollment == "restored" || !spec.Enroll && spec.Notice)
}

func withoutJoinHint(result wire.HookResult) wire.HookResult {
	result.Hint, result.Roster, result.Omitted, result.JoinHintAck = "", nil, 0, nil
	return result
}

func joinHintBoundary(spec eventSpec) bool {
	return (spec.Enroll || spec.RecoverEnroll) && (spec.Output == "context" || spec.Output == "inject_steps")
}

func writeHookResult(ctx context.Context, rv *os.Root, adapter string, epoch uint64, spec eventSpec, result wire.HookResult, stdout io.Writer) string {
	output, failure := prepareHookOutput(rv, adapter, epoch, spec, result)
	if failure != "" {
		return failure
	}
	return writeHookOutput(ctx, stdout, output)
}

func prepareHookOutput(rv *os.Root, adapter string, epoch uint64, spec eventSpec, result wire.HookResult) ([]byte, string) {
	output, err := present(spec, result)
	if err != nil {
		return nil, "refused"
	}
	current, err := readPolicy(rv, adapter)
	if err != nil || !current.Enabled || current.Epoch != epoch || result.Epoch != current.Epoch {
		return nil, "stale_epoch"
	}
	return output, ""
}

func writeHookOutput(ctx context.Context, stdout io.Writer, output []byte) string {
	if hookExpired(ctx) {
		return "deadline"
	}
	if len(output) > 0 {
		if n, err := stdout.Write(output); err != nil || n != len(output) {
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

func resolveLeaf(ctx context.Context, rv *os.Root, stateRoot string, client hookTransport, spec eventSpec, e event) (*os.Root, string, enrolment, string) {
	leaf, gen, err := spool.OpenHookLeaf(rv, e.Identity)
	if err == nil {
		var repo enrolment
		if spec.Enroll && startedHere(stateRoot, e, false) {
			repo = enrolTarget(ctx, rv, e)
		}
		return leaf, gen, repo, ""
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, "", enrolment{}, "refused"
	}
	if !spec.Enroll && !spec.RecoverEnroll {
		return nil, "", enrolment{}, "unmatched"
	}
	if dispatcherGone(rv) {
		return nil, "", enrolment{}, "no_dispatcher"
	}
	if hookExpired(ctx) {
		return nil, "", enrolment{}, "deadline"
	}
	intent := "lookup"
	var repo enrolment
	if spec.RecoverEnroll {
		repo = mappedEnrollmentRoot(ctx, rv, stateRoot, e)
		if hookExpired(ctx) {
			return nil, "", enrolment{}, "deadline"
		}
		if repo.none() {
			return nil, "", enrolment{}, "unmatched"
		}
		intent = "join"
	}
	gen, resp, err := client.HookLookup(ctx, e.Identity, intent)
	if err != nil {
		return nil, "", enrolment{}, transportOutcome(ctx, err)
	}
	newSession := resp.Status == core.ExitNotEnrolled && resp.Code == core.CodeNotEnrolled
	if resp.Status != core.ExitOK && !newSession {
		return nil, "", enrolment{}, "refused"
	}
	if spec.Enroll && startedHere(stateRoot, e, false) {
		repo = enrolTarget(ctx, rv, e)
	}
	if newSession && spec.Enroll {
		// A /clear in a process that ran a member may continue that member
		// wherever it joined.
		if repo.none() && (e.Source != "clear" || e.LaunchRef == "" || !launchMarked(rv, e.LaunchRef)) {
			return nil, "", enrolment{}, "unmatched"
		}
		if hookExpired(ctx) {
			return nil, "", enrolment{}, "deadline"
		}
		gen, resp, err = client.HookLookup(ctx, e.Identity, "join")
		if err != nil {
			return nil, "", enrolment{}, transportOutcome(ctx, err)
		}
	}
	if resp.Status != core.ExitOK {
		return nil, "", enrolment{}, "refused"
	}
	leaf, current, err := spool.OpenHookLeaf(rv, e.Identity)
	if err != nil {
		return nil, "", enrolment{}, transportOutcome(ctx, err)
	}
	if current != gen {
		leaf.Close()
		return nil, "", enrolment{}, "refused"
	}
	return leaf, gen, repo, ""
}

// enrolment names where a session may be enrolled: its worktree, its
// repository, and whether the repository may get a channel of its own. The
// dispatcher takes the worktree's default, else the repository's, else
// creates the repository's channel, all inside its transaction.
type enrolment struct {
	worktree, repository string
	create               bool
}

func (t enrolment) none() bool { return t.worktree == "" }

// place is where the event's directory lies: its worktree root, and the
// root that names its repository, the same for every linked worktree. A
// directory inside Git's own directories is in no worktree: it is recorded
// and treated as outside Git.
func place(e event) (worktree, repository string) {
	if e.CWD == "" {
		return "", ""
	}
	if worktree = cli.WorkTree(e.CWD); worktree == "" {
		return "", ""
	}
	repository, _ = cli.RepoIdentity(worktree)
	return worktree, repository
}

// enrollmentRoot is the repository a start is recorded under.
func enrollmentRoot(e event) string {
	if e.startupRepo != nil {
		return *e.startupRepo
	}
	_, repository := place(e)
	return repository
}

// startedHere applies the start record: a session joins only where its
// first start was. A record written before repositories had one identity
// holds the worktree's digest and still counts for that worktree. need says
// a record is required (a prompt), not only respected (a later start).
func startedHere(stateRoot string, e event, need bool) bool {
	if e.startupRepo != nil {
		return true // this start is the recorded one
	}
	first := startupRepoDigest(stateRoot, e.Identity)
	if first == "" {
		return !need
	}
	worktree, repository := place(e)
	return repository != "" && (first == repoDigestRecord(repository) || first == repoDigestRecord(worktree))
}

// enrolTarget says where the event's directory may be enrolled, or nothing
// when it is in no work tree, or has no default and may not get a channel:
// in a directory Git does not accept as a repository, or with the owner's
// automatic joining off.
func enrolTarget(ctx context.Context, rv *os.Root, e event) enrolment {
	worktree, repository := place(e)
	if worktree == "" {
		return enrolment{}
	}
	t := enrolment{worktree: worktree, repository: repository}
	t.create = autoJoin(ctx, rv) && cli.HasObjectStore(worktree)
	if t.create {
		return t
	}
	if match, _ := hasDefault(ctx, rv, worktree); match {
		return t
	}
	if match, _ := hasDefault(ctx, rv, repository); match {
		return t
	}
	return enrolment{}
}

func mappedEnrollmentRoot(ctx context.Context, rv *os.Root, stateRoot string, e event) enrolment {
	if hookExpired(ctx) || !startedHere(stateRoot, e, true) {
		return enrolment{}
	}
	return enrolTarget(ctx, rv, e)
}

// requestRef is the event reference a hook's requests carry. A prompt's
// carries the hook's entry time where the manifest asks for it: a prompt
// typed while a turn runs reports that turn's id (#163).
func requestRef(entry time.Time, m manifest, e event) string {
	ref := e.reference(entry)
	if m.PromptEntryRef && e.EventRef != "" && e.Fact == wire.HookPromptSubmitted {
		ref = wire.PromptEventRef(ref, entry.UnixMilli())
	}
	return ref
}

func makeRequest(entry time.Time, m manifest, spec eventSpec, e event, repo enrolment, want bool, epoch uint64) wire.Request {
	ref := requestRef(entry, m, e)
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
			Profile: profile, LaunchRef: e.LaunchRef, LaunchVerified: e.LaunchVerified, LaunchHosted: e.LaunchHosted, EventRef: ref,
			PIDNamespace: e.PIDNamespace,
			Source:       e.Source, RepoRoot: repo.worktree, Repository: repo.repository, CreateChannel: repo.create,
			HookAtMS: entry.UnixMilli(), Epoch: epoch,
		})
	} else {
		req.Verb = wire.VerbHookEvent
		if !want {
			repo = enrolment{}
		}
		req.Args, _ = json.Marshal(wire.HookEventArgs{
			Profile: profile, LaunchRef: e.LaunchRef, LaunchVerified: e.LaunchVerified, EventRef: ref,
			Fact: e.Fact, TurnRef: e.TurnRef, HookAtMS: entry.UnixMilli(), WantOutput: want, Epoch: epoch,
			EnrollRepoRoot:     repo.worktree,
			EnrollRepository:   repo.repository,
			EnrollCreate:       repo.create,
			Receipt:            e.receipt,
			ContinuationActive: e.stopActive == "true",
		})
	}
	return req
}

func makeRouteRequest(entry time.Time, m manifest, e event, epoch uint64) (wire.Request, bool) {
	// A start reports whether it saw the socket, even when it did not.
	if e.SocketPath == "" && e.PermissionMode == "" && !(e.Start && m.SocketEnv != "") {
		return wire.Request{}, false
	}
	// The same reference as the hook's own request: a typed-ahead prompt's
	// report is its own, not a repeat of its turn's first prompt (#180).
	ref := requestRef(entry, m, e)
	args, _ := json.Marshal(wire.HookRouteArgs{
		Adapter:        m.Adapter,
		LaunchRef:      e.LaunchRef,
		LaunchVerified: e.LaunchVerified,
		EventRef:       ref,
		HookAtMS:       entry.UnixMilli(),
		Epoch:          epoch,
		PermissionMode: e.PermissionMode,
		SocketPath:     e.SocketPath,
		Start:          e.Start,
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
	cue := result.Cue
	if joinHintBoundary(spec) && (result.Enrollment == "joined" || result.JoinHintAck != nil) {
		if result.Hint != "" {
			parts = append(parts, result.Hint)
			if result.CueWithStartLine != nil {
				cue = *result.CueWithStartLine
			}
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
	}
	if spec.Enroll {
		switch result.Enrollment {
		case "joined":
		case "restored":
			// After a context reset the core sends a short cue in place of
			// what the session was already given.
			if cue != "" {
				parts = append(parts, cue)
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
	if reachShown(spec, result) {
		parts = append(parts, result.ReachLine)
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
