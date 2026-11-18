package hook

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/cli"
	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

// Budget bounds the entire callback from command entry.
const Budget = 150 * time.Millisecond

type hookTransport interface {
	HookLookup(context.Context, ident.Identity, string) (string, wire.Response, error)
	HookCall(context.Context, ident.Identity, string, wire.Request, bool) (spool.Reply, error)
}

// Run serves one callback. Reserve the last few milliseconds for recording a
// timeout even when the vendor keeps stdin open or the dispatcher is busy.
func Run(ctx context.Context, entry time.Time, client spool.Client, args []string, stdin *os.File, stdout *os.File) {
	if len(args) != 2 {
		return
	}
	rv, err := ident.OpenPrivateDir(client.Rendezvous)
	if err != nil {
		return
	}
	defer rv.Close()
	work, cancel := context.WithDeadline(ctx, entry.Add(Budget-5*time.Millisecond))
	defer cancel()
	done := make(chan string, 1)
	go func() {
		done <- dispatchEvent(work, entry, rv, client, args, stdin, stdout, os.LookupEnv)
	}()
	var outcome string
	select {
	case outcome = <-done:
	case <-work.Done():
		outcome = "deadline"
	}
	if work.Err() != nil {
		outcome = "deadline"
	}
	countHook(rv, args[0], args[1], outcome, entry)
}

func dispatchEvent(ctx context.Context, entry time.Time, rv *os.Root, client hookTransport, args []string, stdin io.Reader, stdout io.Writer, lookup func(string) (string, bool)) string {
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
	e, err := parseEvent(stdin, m, spec, lookup)
	if errors.Is(err, errHookIdentity) {
		return "identity"
	}
	if err != nil {
		return "refused"
	}
	leaf, gen, repo, outcome := resolveLeaf(ctx, rv, client, spec, e)
	if outcome != "" {
		return outcome
	}
	defer leaf.Close()
	if ctx.Err() != nil {
		return "deadline"
	}
	e.LaunchRef, e.LaunchVerified, err = bindLaunch(leaf, gen, e, spec, entry)
	if err != nil {
		return "refused"
	}
	var effect, turn string
	switch {
	case spec.Enroll || spec.Notice:
		effect = "notice"
	case spec.Stop && policy.Reminder:
		// Without the reminder a stop has nothing to evaluate: it publishes
		// one-way like any observation, so a quiet turn waits on nothing.
		effect, turn = "reminder", e.TurnRef
	}
	clean := !spec.Enroll && effect != "" && stampClean(rv, leaf, gen, effect, turn)
	want := effect != "" && !clean
	req := makeRequest(entry, m, spec, e, repo, want, policy.Epoch)
	if ctx.Err() != nil {
		return "deadline"
	}
	if dispatcherGone(rv) {
		return "no_dispatcher" // a stopped dispatcher must not cost the turn the deadline
	}
	reply, err := client.HookCall(ctx, e.Identity, gen, req, !want)
	if err != nil {
		return transportOutcome(ctx, err)
	}
	if clean || !want {
		return "clean" // published without waiting for a response
	}
	if reply.Response.Status != core.ExitOK {
		return "refused"
	}
	var result wire.HookResult
	if err := json.Unmarshal(reply.Response.Data, &result); err != nil {
		return "refused"
	}
	output, err := present(spec, result)
	if err != nil {
		return "refused"
	}
	current, err := readPolicy(rv, m.Adapter)
	if err != nil || !current.Enabled || current.Epoch != policy.Epoch || result.Epoch != current.Epoch {
		return "stale_epoch"
	}
	if ctx.Err() != nil {
		return "deadline"
	}
	if len(output) > 0 {
		if _, err := stdout.Write(output); err != nil {
			return "refused"
		}
	}
	if result.Ack != nil && ctx.Err() == nil {
		_ = saveSeen(rv, leaf, gen, effect, turn, *result.Ack)
	}
	return "rpc"
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

func resolveLeaf(ctx context.Context, rv *os.Root, client hookTransport, spec eventSpec, e event) (*os.Root, string, string, string) {
	leaf, gen, err := spool.OpenHookLeaf(rv, e.Identity)
	if err == nil {
		return leaf, gen, enrollmentRoot(spec, e), ""
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, "", "", "refused"
	}
	if !spec.Enroll {
		return nil, "", "", "unmatched"
	}
	if dispatcherGone(rv) {
		return nil, "", "", "no_dispatcher"
	}
	gen, resp, err := client.HookLookup(ctx, e.Identity, "lookup")
	if err != nil {
		return nil, "", "", transportOutcome(ctx, err)
	}
	newSession := resp.Status == core.ExitNotEnrolled && resp.Code == core.CodeNotEnrolled
	if resp.Status != core.ExitOK && !newSession {
		return nil, "", "", "refused"
	}
	repo := enrollmentRoot(spec, e)
	if newSession {
		match, err := hasDefault(ctx, rv, repo)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, "", "", transportOutcome(ctx, err)
		}
		if !match {
			return nil, "", "", "unmatched"
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

func enrollmentRoot(spec eventSpec, e event) string {
	if !spec.Enroll || e.CWD == "" {
		return ""
	}
	root, _ := cli.RepoRoot(e.CWD)
	return root
}

func makeRequest(entry time.Time, m manifest, spec eventSpec, e event, repo string, want bool, epoch uint64) wire.Request {
	ref := e.reference(entry)
	// These synthetic manifests establish neither the launch build nor its mode.
	profile := wire.HookProfile{
		Adapter: m.Adapter, ManifestRev: m.Revision, Platform: runtime.GOOS + "-" + runtime.GOARCH,
		VendorVersion: "unverified", Mode: "unverified",
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
		req.Args, _ = json.Marshal(wire.HookEventArgs{
			Profile: profile, LaunchRef: e.LaunchRef, LaunchVerified: e.LaunchVerified, EventRef: ref,
			Fact: e.Fact, TurnRef: e.TurnRef, HookAtMS: entry.UnixMilli(), WantOutput: want, Epoch: epoch,
		})
	}
	return req
}

// present renders only the fields appropriate for this boundary. Slice one
// never requests a continuation of the vendor's turn.
func present(spec eventSpec, result wire.HookResult) ([]byte, error) {
	if spec.Output != "context" || !(spec.Enroll || spec.Notice) {
		return nil, nil
	}
	var parts []string
	if spec.Enroll {
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
	type contextOutput struct {
		Event   string `json:"hookEventName"`
		Context string `json:"additionalContext"`
	}
	payload := struct {
		Output contextOutput `json:"hookSpecificOutput"`
	}{contextOutput{spec.Name, strings.Join(parts, "\n")}}
	b, err := json.Marshal(payload)
	return append(b, '\n'), err
}

// dispatcherGone reports a missing readiness marker or a marker whose process
// no longer exists. The hook then publishes nothing and does not wait.
func dispatcherGone(rv *os.Root) bool {
	raw, err := ident.ReadPrivateFile(rv, "ready", 4096)
	if err != nil {
		return true
	}
	var marker struct {
		PID int `json:"pid"`
	}
	if json.Unmarshal(raw, &marker) != nil || marker.PID <= 0 {
		return true
	}
	return errors.Is(syscall.Kill(marker.PID, 0), syscall.ESRCH)
}
