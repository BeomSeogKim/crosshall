package spool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

const pollInterval = 4 * time.Millisecond

// Deadline counts startup and enrollment against the command's total allowance.
func Deadline(entry time.Time, spawned bool) time.Time {
	if spawned {
		return entry.Add(3 * time.Second)
	}
	return entry.Add(time.Second)
}

// Client uses paths supplied by the caller's state-root selection.
type Client struct {
	StateRoot  string
	Rendezvous string
}

// Reply keeps the operation key available even if a response was lost.
type Reply struct {
	wire.Response
	OpKey string
	// PriorRosterAck is the last successfully displayed cursor sent with this request.
	PriorRosterAck uint64

	rendezvous string
	digest     string
	generation string
	cursor     uint64
	rules      bool // the reply carries the member rules
	// rotated says the request registered the directory again; the
	// dispatcher may publish the new generation just after its reply.
	rotated bool
}

// RulesReceipt names the file in a member's directory that holds the roster
// cursor of the last reply the CLI rendered with the member rules in it.
const RulesReceipt = "rules"

// Member sends a command through the registered member leaf. The caller may
// set a deadline at CLI entry so discovery and startup use the same budget.
func (c Client) Member(ctx context.Context, id ident.Identity, req wire.Request) (Reply, error) {
	ctx, cancel := commandContext(ctx)
	defer cancel()
	req = prepare(ctx, req)
	rv, err := ident.OpenPrivateDir(c.Rendezvous)
	if err != nil {
		return c.failure(req, core.CodeUnsent, err)
	}
	defer rv.Close()

	digest := id.Digest()
	leaf, generation, err := openLeaf(rv, digest)
	needsHello := errors.Is(err, fs.ErrNotExist)
	if err == nil && id.Version != "" {
		observed, readErr := ident.ReadPrivateFile(leaf, "version", 4096)
		if readErr != nil && !errors.Is(readErr, fs.ErrNotExist) {
			leaf.Close()
			return c.failure(req, core.CodeUnsent, readErr)
		}
		needsHello = string(observed) != id.Version
		if needsHello {
			leaf.Close()
		}
	}
	if needsHello {
		intent := "lookup"
		if req.Verb == "join" {
			intent = "join"
		}
		resp, sendErr := firstContact(ctx, rv, id, intent)
		if sendErr != nil {
			return c.failure(req, core.CodeUnsent, sendErr)
		}
		if resp.Status == core.ExitUnavailable {
			return c.failure(req, core.CodeUnsent, fmt.Errorf("first contact failed: %s", resp.Code))
		}
		if resp.Status != core.ExitOK {
			return Reply{Response: resp, OpKey: req.OpKey}, nil
		}
		leaf, generation, err = openLeaf(rv, digest)
		// The hello response is published only after the leaf can accept commands.
		if err != nil {
			return c.failure(req, core.CodeUnsent, err)
		}
	} else if err != nil {
		return c.failure(req, core.CodeUnsent, err)
	}
	defer leaf.Close()

	req.VendorVersion = id.Version
	req.KeyDigest, req.Generation = digest, generation
	req.Ack, err = readSequence(leaf, "ack")
	if err != nil {
		return c.failure(req, core.CodeUnsent, err)
	}
	resp, code, err := exchange(ctx, leaf, req.Attempt, req)
	if err != nil {
		return c.failure(req, code, err)
	}
	reply := Reply{Response: resp, OpKey: req.OpKey, PriorRosterAck: req.Ack}
	var trailer struct {
		Cursor uint64 `json:"roster_seq"`
		Rules  bool   `json:"rules_given"`
		// Generation is set when the request registered the directory again
		// (a cleared conversation joining as a new member).
		Generation string `json:"generation"`
	}
	if err := json.Unmarshal(resp.Data, &trailer); err == nil && trailer.Cursor > 0 {
		if trailer.Generation != "" {
			generation, reply.rotated = trailer.Generation, true
		}
		reply.cursor, reply.rendezvous, reply.digest, reply.generation = trailer.Cursor, c.Rendezvous, digest, generation
		reply.rules = trailer.Rules
	}
	return reply, nil
}

func firstContact(ctx context.Context, rv *os.Root, id ident.Identity, intent string) (wire.Response, error) {
	first, err := ident.OpenPrivateChild(rv, "first")
	if err != nil {
		return wire.Response{}, err
	}
	defer first.Close()
	hello := wire.Hello{Version: wire.Version, Attempt: wire.NewID(), Vendor: id.Vendor,
		Key: id.Key, VendorVersion: id.Version, Intent: intent}
	resp, _, err := exchange(ctx, first, hello.Attempt, hello)
	return resp, err
}

// Owner sends through the home-state spool after the caller's owner gate passed.
func (c Client) Owner(ctx context.Context, req wire.Request) (Reply, error) {
	ctx, cancel := commandContext(ctx)
	defer cancel()
	req = prepare(ctx, req)
	state, err := ident.OpenPrivateDir(c.StateRoot)
	if err != nil {
		return c.failure(req, core.CodeUnsent, err)
	}
	defer state.Close()
	owner, err := ident.OpenPrivateChild(state, "owner")
	if err != nil {
		return c.failure(req, core.CodeUnsent, err)
	}
	defer owner.Close()
	req.VendorVersion = ""
	req.Generation, req.KeyDigest, req.Ack = "", "", 0
	resp, code, err := exchange(ctx, owner, req.Attempt, req)
	if err != nil {
		return c.failure(req, code, err)
	}
	return Reply{Response: resp, OpKey: req.OpKey}, nil
}

func commandContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return context.WithCancel(ctx)
	}
	return context.WithDeadline(ctx, Deadline(time.Now(), false))
}

func prepare(ctx context.Context, req wire.Request) wire.Request {
	req.Version = wire.Version
	req.Attempt = wire.NewID()
	if req.OpKey == "" {
		req.OpKey = wire.NewID()
	}
	if len(req.Args) == 0 {
		req.Args = json.RawMessage(`{}`)
	}
	req.IssuedAtMS = time.Now().UnixMilli()
	deadline, _ := ctx.Deadline()
	req.ExpiresAtMS = deadline.UnixMilli()
	return req
}

func openLeaf(rv *os.Root, digest string) (*os.Root, string, error) {
	members, err := ident.OpenPrivateChild(rv, "m")
	if err != nil {
		return nil, "", err
	}
	defer members.Close()
	leaf, err := ident.OpenPrivateChild(members, digest[:16])
	if err != nil {
		return nil, "", err
	}
	data, err := ident.ReadPrivateFile(leaf, "gen", 1024)
	gen := strings.TrimSpace(string(data))
	if err == nil && gen == "" {
		err = fmt.Errorf("empty exchange generation")
	}
	if err != nil {
		leaf.Close()
		return nil, "", err
	}
	return leaf, gen, nil
}

func readSequence(leaf *os.Root, name string) (uint64, error) {
	data, err := ident.ReadPrivateFile(leaf, name, 32)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
}

// Acknowledge records a cursor only after the caller rendered the reply.
// Replies without a cursor have nothing to acknowledge.
func (r Reply) Acknowledge() error {
	if r.cursor == 0 {
		return nil
	}
	rv, err := ident.OpenPrivateDir(r.rendezvous)
	if err != nil {
		return err
	}
	defer rv.Close()
	leaf, _, err := openLeaf(rv, r.digest)
	if err != nil {
		return err
	}
	defer leaf.Close()
	// Another renderer, or the dispatcher publishing a generation, may hold
	// the directory for a moment; a reply that registered the directory
	// again waits for its generation to be published.
	deadline := time.Now().Add(acknowledgeWait)
	for {
		recorded, err := r.record(leaf)
		if err != nil && !errors.Is(err, syscall.EWOULDBLOCK) || recorded {
			return err
		}
		if time.Now().After(deadline) {
			// The wait is bounded: what could not be recorded in time is left
			// for the dispatcher to apply with the next request, once its
			// generation is the published one.
			if !errors.Is(err, syscall.EWOULDBLOCK) && !r.rotated {
				return err
			}
			var rules uint64
			if r.rules {
				rules = r.cursor
			}
			return ident.LeavePendingReceipt(leaf, r.generation, r.cursor, rules)
		}
		if err == nil && !r.rotated {
			return nil // a reply of an older generation has nothing to record
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// acknowledgeWait bounds how long rendering waits to record its receipts.
const acknowledgeWait = 3 * time.Second

// record writes the reply's receipts under the directory lock if the
// directory carries the reply's generation, and says whether it did.
func (r Reply) record(leaf *os.Root) (bool, error) {
	// Lock the directory inode, which remains stable across atomic ack
	// replacements; the dispatcher takes it to publish a generation.
	lock, err := leaf.Open(".")
	if err != nil {
		return false, err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return false, err
	}
	generation, err := ident.ReadPrivateFile(leaf, "gen", 1024)
	if err != nil || strings.TrimSpace(string(generation)) != r.generation {
		return false, err
	}
	if r.rules {
		// Before the cursor, which an older version may already have set as far.
		record := []byte(strconv.FormatUint(r.cursor, 10) + "\n")
		if err := publishFile(context.Background(), leaf, wire.NewID()+".part", RulesReceipt, record); err != nil {
			return false, err
		}
	}
	last, err := readSequence(leaf, "ack")
	if err != nil || last >= r.cursor {
		return err == nil, err
	}
	return true, publishFile(context.Background(), leaf, wire.NewID()+".part", "ack", []byte(strconv.FormatUint(r.cursor, 10)+"\n"))
}

func exchange(ctx context.Context, dir *os.Root, attempt string, packet any) (wire.Response, core.Code, error) {
	data, err := json.Marshal(packet)
	if err != nil {
		return wire.Response{}, core.CodeUnsent, err
	}
	return exchangeWithPublisher(ctx, dir, attempt, func(in *os.Root) error {
		return publishFile(ctx, in, attempt+".part", attempt+".json", data)
	})
}

func exchangeWithPublisher(ctx context.Context, dir *os.Root, attempt string, publish func(*os.Root) error) (wire.Response, core.Code, error) {
	var empty wire.Response
	in, err := ident.OpenPrivateChild(dir, "in")
	if err != nil {
		return empty, core.CodeUnsent, err
	}
	defer in.Close()
	out, err := ident.OpenPrivateChild(dir, "out")
	if err != nil {
		return empty, core.CodeUnsent, err
	}
	defer out.Close()
	if err := ctx.Err(); err != nil {
		return empty, core.CodeUnsent, err
	}
	name := attempt + ".json"
	if err := publish(in); err != nil {
		return empty, core.CodeUnsent, err
	}

	tick := time.NewTicker(pollInterval)
	defer tick.Stop()
	for {
		resp, err := readResponse(out, attempt)
		if err == nil {
			return resp, "", nil
		}
		if errors.Is(err, fs.ErrNotExist) {
			select {
			case <-tick.C:
				continue
			case <-ctx.Done():
				err = ctx.Err()
			}
		}
		// A successful removal and the dispatcher's claim rename are mutually exclusive.
		if in.Remove(name) == nil {
			return empty, core.CodeUnsent, err
		}
		// The response may have arrived between the last read and withdrawal.
		if resp, readErr := readResponse(out, attempt); readErr == nil {
			return resp, "", nil
		}
		return empty, core.CodeOutcomeUnknown, err
	}
}

func publishFile(ctx context.Context, dir *os.Root, temporary, final string, data []byte) error {
	file, err := dir.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	closeErr := file.Close()
	err = errors.Join(writeErr, closeErr, ctx.Err())
	// A process can resume after its deadline before the context timer runs.
	if deadline, ok := ctx.Deadline(); err == nil && ok && !time.Now().Before(deadline) {
		err = context.DeadlineExceeded
	}
	if err == nil {
		err = dir.Rename(temporary, final)
	}
	if err != nil {
		dir.Remove(temporary)
	}
	return err
}

func readResponse(out *os.Root, attempt string) (wire.Response, error) {
	name := attempt + ".json"
	data, err := ident.ReadPrivateFile(out, name, 16<<20)
	response := wire.Response{Result: core.Result{Status: -1}}
	if err != nil {
		return response, err
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return response, err
	}
	if response.Version != wire.Version || response.Attempt != attempt ||
		response.Status < core.ExitOK || response.Status > core.ExitUnavailable || len(response.Data) == 0 {
		return response, fmt.Errorf("invalid response for attempt %s", attempt)
	}
	out.Remove(name)
	return response, nil
}

func (c Client) failure(req wire.Request, code core.Code, cause error) (Reply, error) {
	key := req.OpKey
	if strings.ContainsAny(key, " \t\r\n'\"\\;$`&|*?()<>[]~!#{}") {
		key = "'" + strings.ReplaceAll(key, "'", "'\\''") + "'"
	}
	hint := "retry with --key " + key
	// The client cannot distinguish every stale instance from a stalled one.
	// The conditional restart hint is also safe when a ready marker survives a crash.
	hint += ". If the dispatcher is unavailable, run crosshall owner start."
	data, _ := json.Marshal(struct {
		Key     string `json:"op_key"`
		Message string `json:"message"`
		Hint    string `json:"hint"`
	}{req.OpKey, string(code), hint})
	return Reply{Response: wire.Response{Version: wire.Version, Attempt: req.Attempt,
		Result: core.Result{Status: core.ExitUnavailable, Code: code, Data: data}}, OpKey: req.OpKey}, cause
}
