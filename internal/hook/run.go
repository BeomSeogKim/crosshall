package hook

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"runtime"
	"slices"
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

// Budget bounds callback work. An enrolling hook can wait for dispatcher startup first.
const Budget = 150 * time.Millisecond

// StartBudget is the deadline of a callback that may enroll its session: a
// start, which takes three round trips to the dispatcher, and a prompt,
// which enrolls a session whose start was missed, repairs it or binds its
// launch. A busy disk can hold each round trip; the vendor's own timeout is
// 1 s.
const StartBudget = 400 * time.Millisecond

// CallbackBudget is the deadline of adapter's callback for event. A prompt
// gets the start budget only until it finds it enrolls nothing (dispatchCallback),
// and so does a turn end or idle prompt that may bind a missed launch.
func CallbackBudget(adapter, event string) time.Duration {
	m, ok := loadManifest(adapter)
	spec, known := m.event(event)
	if !ok || !known {
		return Budget
	}
	if m.bindsLateLaunch(spec) {
		return StartBudget
	}
	return spec.budget()
}

// CallbackBase is the deadline adapter's callback for event runs on until
// it extends to CallbackBudget: a prompt starts on the ordinary budget, as
// does a turn end or idle prompt.
func CallbackBase(adapter, event string) time.Duration {
	m, ok := loadManifest(adapter)
	spec, known := m.event(event)
	if !ok || !known || (spec.RecoverEnroll && !spec.Enroll) || m.bindsLateLaunch(spec) {
		return Budget
	}
	return spec.budget()
}

func (s eventSpec) budget() time.Duration {
	if s.Enroll || s.RecoverEnroll {
		return StartBudget
	}
	return Budget
}

// bindsLateLaunch reports whether the event's callback may bind a launch
// whose start was missed (lateLaunchFact): it extends to the start budget
// when it does, as a start would take, so the route report that follows is
// still current when the dispatcher reads it (#437).
func (m manifest) bindsLateLaunch(spec eventSpec) bool {
	if m.LaunchEnv == "" || spec.Enroll {
		return false
	}
	if lateLaunchFact(spec.Fact) {
		return true
	}
	for _, fact := range spec.FactsByMatch {
		if lateLaunchFact(fact) {
			return true
		}
	}
	return false
}

type hookTransport interface {
	HookLookup(context.Context, ident.Identity, string) (string, wire.Response, error)
	HookCall(context.Context, ident.Identity, string, wire.Request, bool) (spool.Reply, error)
}

// Run serves one callback. Path resolution and counter preparation share the
// callback budget with dispatch. Only the supervisor records an outcome.
func Run(captured time.Time, args []string, stdin io.Reader, stdout io.Writer, locate func() (Paths, error)) {
	if len(args) != 2 {
		return
	}
	lifecycle := prepareRepoLifecycle(args, stdin, locate)
	if lifecycle != nil {
		defer lifecycle.cancel()
	}
	budgetStart, startedState := wake(captured, args, locate)
	budget := CallbackBudget(args[0], args[1])
	if startedState != "" {
		budget = afterStart(captured, budgetStart, budget)
	}
	// A prompt runs on the ordinary budget until it finds that it recovers
	// or repairs its session or binds its launch (extendBudget).
	base := min(budget, CallbackBase(args[0], args[1]))
	end := budgetStart.Add(base)
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
		return rv, openHookCounter(rv, args[0], args[1], budgetStart)
	}
	var outcome atomic.Value
	wrote := runTimed(budgetStart, base, budget, stdout, prepare, func(ctx context.Context, rv *os.Root, output io.Writer) string {
		client := spool.Client{Rendezvous: rv.Name()}
		o := dispatchCallback(ctx, captured, rv, client, args, stdin, output, os.LookupEnv, callbackState{stateRoot, lifecycle})
		outcome.Store(o)
		return o
	})
	if lifecycle != nil && !lifecycle.waitUntil(captured.Add(budget)) && args[1] == "SessionStart" && startedState == "" {
		lifecycle.waitUntil(time.Now().Add(startupRepoGrace))
	}
	if startedState != "" {
		if o, _ := outcome.Load().(string); o == "" || o == "deadline" {
			logLine(startedState, args[0]+" "+args[1], captured, "started the dispatcher, but the session start was not answered in time")
		}
	}
	if wrote || !time.Now().Before(end) {
		return
	}
	if m, _ := loadManifest(args[0]); m.EmptyOutput != "" {
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = io.WriteString(stdout, m.EmptyOutput+"\n")
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
	writer := &frameWriter{dst: stdout}
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
	if errors.Is(err, errHookSkip) {
		return "clean"
	}
	if err != nil {
		return "refused"
	}
	if m.BuildFromHost {
		e.VendorVersion, e.Mode = hostBuild(rv, m)
	}
	if hookExpired(ctx) {
		return "deadline"
	}
	leaf, gen, repo, outcome := resolveLeaf(ctx, rv, state.root, client, spec, e)
	if outcome != "" {
		// With automatic joining on, such a start is outside Git: no line.
		if outcome == "unmatched" && spec.Enroll && !autoJoin(ctx, rv) {
			result := domain.UnmappedStart(e.Source, policy.Epoch)
			if failure := writeHookResult(ctx, rv, m.Adapter, policy.Epoch, spec, result, writer); failure != "" {
				return failure
			}
		}
		return outcome
	}
	defer leaf.Close()
	if hookExpired(ctx) {
		return "deadline"
	}
	if spec.OpenTurn && e.TurnRef != "" {
		// Best effort: without it the turn's end is only observed.
		_ = writeRecord(leaf, openTurnFile, gen+" "+e.TurnRef+"\n")
	}
	if spec.TurnFromOpen {
		e.TurnRef = readOpenTurn(leaf, gen)
		e.setEventRef(spec)
	}
	membership, err := ident.LookupMembership(leaf)
	if err != nil {
		return "refused"
	}
	pid := e.LaunchRef
	launch, err := bindLaunch(leaf, gen, e, spec, entry)
	e.LaunchRef, e.LaunchVerified = launch.ref, launch.verified
	if err != nil {
		return "refused"
	}
	if lateLaunchFact(e.Fact) && e.LaunchVerified {
		// The proof the dispatcher needs for a launch it has no row for.
		e.LaunchAtMS, e.PIDNamespace = launch.atMS, proc.PIDNamespace()
		if launch.late {
			markLaunch(rv, pid)
			extendBudget(ctx) // this callback binds a launch whose start was missed
		}
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
	// publish their lifecycle observation without waiting for a reply, and
	// so does a Stop the dispatcher recorded no ask to announce for (#261).
	turnEnd := spec.Stop && e.stopActive == "false" && e.VendorVersion != "unverified" && e.Mode != "unverified" &&
		turnEndListed(rv, m.Adapter, runtime.GOOS+"-"+runtime.GOARCH, e.Mode, m.Revision) && !noTurnEndAsk(rv, leaf, gen)
	// A callback that binds a launch whose start was missed asks: while the
	// session had no open launch, new work did not mark its stamp.
	clean := !spec.Enroll && notice && !launch.late && stampClean(rv, leaf, gen, "notice", "")
	if spec.RecoverEnroll && !clean && repo.none() && e.CWD != "" && !hasSeenNotice(leaf, gen) {
		repo = mappedEnrollmentRoot(ctx, rv, state.root, e)
	}
	if spec.RecoverEnroll && !spec.Enroll && !repo.none() {
		extendBudget(ctx) // this prompt repairs its session's enrolment
	} else if end, ok := ctx.Deadline(); ok && spec.RecoverEnroll && !spec.Enroll && !launch.late &&
		end.After(entry.Add(Budget-counterAllowance)) {
		// This prompt enrolls, repairs and binds nothing: no request after
		// the ordinary budget.
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, entry.Add(Budget-counterAllowance))
		defer cancel()
	}
	want := turnEnd || (notice && !clean)
	gone, offers := readDispatcher(rv)
	if !spec.Enroll && !slices.Contains(offers, wire.HelloLaunchProof) {
		e.LaunchAtMS, e.PIDNamespace = 0, "" // an older dispatcher refuses the proof
	}
	// A prompt names its server as a start does; an older dispatcher
	// refuses the fields, and no other callback needs them.
	if !spec.Enroll && (spec.Fact != wire.HookPromptSubmitted || !slices.Contains(offers, wire.HelloPromptHost)) {
		e.Host, e.VendorHome = nil, ""
	}
	req := makeRequest(entry, m, spec, e, repo, want, policy.Epoch)
	req.Membership = membership
	if gone {
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
				routeReq.Membership = membership
				_, _ = client.HookCall(ctx, e.Identity, gen, routeReq, true)
			}
		}
		return "clean" // published without waiting for a response
	}
	if reply.Response.Status != core.ExitOK {
		return "refused"
	}
	if reply.Membership != nil {
		membership = reply.Membership
	}
	if current, err := ident.ReadMembership(leaf); err != nil ||
		membership != nil && current != *membership || membership == nil && current.Generation > 1 {
		return "refused"
	}
	var result wire.HookResult
	if err := json.Unmarshal(reply.Response.Data, &result); err != nil {
		return "refused"
	}
	failure, joinHintSeen := writeHookResultAndConfirmJoin(ctx, rv, leaf, gen, m.Adapter, policy.Epoch, spec, result, writer)
	if failure != "" {
		return failure
	}
	reachSeen := confirmReach(ctx, leaf, gen, spec, result)
	offerEnqueued := false
	if (joinHintSeen || reachSeen || writer.ack.Offer != nil) && outputAckBudget(ctx) {
		reqAck := wire.Request{Verb: wire.VerbHookJoinHintAck, OpKey: wire.VerbHookJoinHintAck, Membership: membership}
		if writer.ack.Offer != nil {
			reqAck.Args, _ = json.Marshal(writer.ack)
		}
		_, err := client.HookCall(ctx, e.Identity, gen, reqAck, true)
		offerEnqueued = writer.ack.Offer != nil && err == nil
	}
	if notice && result.Ack != nil && (result.OfferReceipt == nil || offerEnqueued) &&
		(result.JoinHintAck == nil || joinHintSeen) && (result.ReachAck == nil || reachSeen) && !hookExpired(ctx) {
		_ = saveSeen(rv, leaf, gen, "notice", "", *result.Ack)
	}
	if !hookExpired(ctx) {
		if routeReq, ok := makeRouteRequest(entry, m, e, policy.Epoch); ok {
			routeReq.Membership = membership
			_, _ = client.HookCall(ctx, e.Identity, gen, routeReq, true)
		}
	}
	return "rpc"
}

// Leave the work deadline intact when there is little time for the spool write.
func outputAckBudget(ctx context.Context) bool {
	const minRemaining = 5 * time.Millisecond
	end, ok := ctx.Deadline()
	return ctx.Err() == nil && (!ok || time.Until(end) >= minRemaining)
}

func writeHookResultAndConfirmJoin(ctx context.Context, rv, leaf *os.Root, generation, adapter string, epoch uint64,
	spec eventSpec, result wire.HookResult, stdout *frameWriter) (string, bool) {
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
	if failure := stdout.write(ctx, output); failure != "" {
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
	joinedWarning := result.Enrollment == "joined" && result.ReachAck != nil && result.ReachAck.Told == 0
	return result.ReachLine != "" && !spec.Stop && (spec.Output == "context" || spec.Output == "inject_steps") &&
		(spec.Enroll && (result.Enrollment == "restored" || joinedWarning) || !spec.Enroll && spec.Notice)
}

func withoutJoinHint(result wire.HookResult) wire.HookResult {
	result.Hint, result.Roster, result.Omitted, result.JoinHintAck = "", nil, 0, nil
	return result
}

func noticeBoundary(spec eventSpec) bool {
	return (spec.Enroll || spec.Notice) && (spec.Output == "context" || spec.Output == "inject_steps")
}

func joinHintBoundary(spec eventSpec) bool {
	return (spec.Enroll || spec.RecoverEnroll) && (spec.Output == "context" || spec.Output == "inject_steps")
}

func writeHookResult(ctx context.Context, rv *os.Root, adapter string, epoch uint64, spec eventSpec, result wire.HookResult, stdout *frameWriter) string {
	output, failure := prepareHookOutput(rv, adapter, epoch, spec, result)
	if failure != "" {
		return failure
	}
	return stdout.write(ctx, output)
}

func prepareHookOutput(rv *os.Root, adapter string, epoch uint64, spec eventSpec, result wire.HookResult) (hookFrame, string) {
	output, err := present(spec, result)
	if err != nil {
		return hookFrame{}, "refused"
	}
	current, err := readPolicy(rv, adapter)
	if err != nil || !current.Enabled || current.Epoch != epoch || result.Epoch != current.Epoch {
		return hookFrame{}, "stale_epoch"
	}
	return output, ""
}

type frameWriter struct {
	dst io.Writer
	ack wire.HookOutputAckArgs
}

func (w *frameWriter) write(ctx context.Context, frame hookFrame) string {
	if hookExpired(ctx) {
		return "deadline"
	}
	if len(frame.data) > 0 {
		if n, err := w.dst.Write(frame.data); err != nil || n != len(frame.data) {
			return "refused"
		}
		if offer := frame.offer; offer != nil && offer.OfferID > 0 && offer.Nonce != "" {
			w.ack = wire.HookOutputAckArgs{Offer: offer, WrittenAtMS: time.Now().UnixMilli()}
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
	members, done, err := memberRoot(rv)
	if err != nil {
		return nil, "", enrolment{}, transportOutcome(ctx, err)
	}
	defer done()
	leaf, gen, err := spool.OpenHookLeaf(members, e.Identity)
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
	gone, offers := readDispatcher(rv)
	if gone {
		return nil, "", enrolment{}, "no_dispatcher"
	}
	if hookExpired(ctx) {
		return nil, "", enrolment{}, "deadline"
	}
	intent := "lookup"
	var repo enrolment
	if spec.Enroll && startedHere(stateRoot, e, false) {
		repo = enrolTarget(ctx, rv, e)
	}
	// A start that would join an unknown key asks for that in one round
	// trip, where the dispatcher accepts it; a retired key is refused as
	// the lookup would refuse it.
	if spec.Enroll && slices.Contains(offers, wire.HelloStart) && (!repo.none() || continuesMember(rv, e)) {
		intent = wire.HelloStart
	}
	if spec.RecoverEnroll {
		repo = mappedEnrollmentRoot(ctx, rv, stateRoot, e)
		if hookExpired(ctx) {
			return nil, "", enrolment{}, "deadline"
		}
		if repo.none() {
			return nil, "", enrolment{}, "unmatched"
		}
		extendBudget(ctx) // this prompt recovers a missed start
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
	if newSession && spec.Enroll {
		if repo.none() && !continuesMember(rv, e) {
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
	leaf, current, err := spool.OpenHookLeaf(members, e.Identity)
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
	req := wire.Request{
		OpKey:         "hook:" + m.Adapter + ":" + e.LaunchRef + ":" + ref,
		OfferReceipts: want && noticeBoundary(spec),
	}
	if spec.Enroll {
		req.Verb = wire.VerbHookEnroll
		req.Args, _ = json.Marshal(wire.HookEnrollArgs{
			Profile: profile, LaunchRef: e.LaunchRef, LaunchVerified: e.LaunchVerified, LaunchHosted: e.LaunchHosted, EventRef: ref,
			PIDNamespace: e.PIDNamespace, Host: e.Host, VendorHome: e.VendorHome,
			Source: e.Source, RepoRoot: repo.worktree, Repository: repo.repository, CreateChannel: repo.create,
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
			LaunchAtMS:         e.LaunchAtMS,
			PIDNamespace:       e.PIDNamespace,
			Host:               e.Host,
			VendorHome:         e.VendorHome,
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

type hookFrame struct {
	data  []byte
	offer *wire.HookOfferReceipt
}

// present renders only the fields appropriate for this boundary.
func present(spec eventSpec, result wire.HookResult) (hookFrame, error) {
	if spec.Stop {
		data, err := presentTurnEnd(spec, result.TurnEndLine)
		return hookFrame{data: data}, err
	}
	if !noticeBoundary(spec) {
		return hookFrame{}, nil
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
				return hookFrame{}, err
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
				return hookFrame{}, nil
			}
			data, err := presentBoundary(spec, result.Hint)
			return hookFrame{data: data}, err
		default:
			return hookFrame{}, nil
		}
	}
	if reachShown(spec, result) {
		parts = append(parts, result.ReachLine)
	}
	var frame hookFrame
	if result.PendingLine != "" {
		parts = append(parts, result.PendingLine)
		frame.offer = result.OfferReceipt
	}
	if len(parts) == 0 {
		return hookFrame{}, nil
	}
	var err error
	frame.data, err = presentBoundary(spec, strings.Join(parts, "\n"))
	return frame, err
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
	case "block", "continue":
		payload := struct {
			Decision string `json:"decision"`
			Reason   string `json:"reason"`
		}{spec.Output, line}
		b, err := json.Marshal(payload)
		return append(b, '\n'), err
	default:
		return nil, nil
	}
}

// dispatcherGone reports whether the dispatcher has stopped serving this
// rendezvous. The hook then publishes nothing and does not wait.
func dispatcherGone(rv *os.Root) bool {
	gone, _ := readDispatcher(rv)
	return gone
}

// readDispatcher reads the ready marker: whether the dispatcher is gone,
// and what it accepts beyond an older dispatcher (wire.HelloStart,
// wire.HelloLaunchProof). An older dispatcher lists none.
func readDispatcher(rv *os.Root) (bool, []string) {
	raw, err := ident.ReadPrivateFile(rv, "ready", 4096)
	if err != nil {
		return true, nil
	}
	var marker struct {
		Instance string   `json:"instance"`
		PID      int      `json:"pid"`
		Lease    bool     `json:"lease"`
		Hello    []string `json:"hello"`
	}
	if json.Unmarshal(raw, &marker) != nil || marker.PID <= 0 {
		return true, nil
	}
	if marker.Lease {
		held, err := spool.ReadyLeaseHeld(rv, marker.Instance)
		if !errors.Is(err, os.ErrPermission) {
			return !held, marker.Hello
		}
	}
	// Older dispatchers omit the lease. A sandbox that denies inspection may
	// still reach the dispatcher, so retain the legacy PID check there too.
	return errors.Is(syscall.Kill(marker.PID, 0), syscall.ESRCH), marker.Hello
}

// continuesMember reports a /clear in a process that ran a member: it may
// continue that member wherever it joined.
func continuesMember(rv *os.Root, e event) bool {
	return e.Source == "clear" && e.LaunchRef != "" && launchMarked(rv, e.LaunchRef)
}

// memberRoot opens the rendezvous that holds the member leaves: rv itself,
// or a linked device's member rendezvous (#400). The ready marker, the
// policy and the counters stay in rv.
func memberRoot(rv *os.Root) (*os.Root, func(), error) {
	path := spool.LinkedMembers(rv)
	if path == "" || path == rv.Name() {
		return rv, func() {}, nil
	}
	members, err := ident.OpenPrivateDir(path)
	if err != nil {
		return nil, nil, err
	}
	return members, func() { members.Close() }, nil
}
