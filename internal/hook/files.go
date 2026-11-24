package hook

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

type adapterPolicy struct {
	Enabled  bool
	Reminder bool
	Epoch    uint64
}

func readPolicy(rv *os.Root, adapter string) (adapterPolicy, error) {
	return readPolicyFile(rv, "policy", adapter)
}

func readPolicyFile(dir *os.Root, name, adapter string) (adapterPolicy, error) {
	data, err := ident.ReadPrivateFile(dir, name, 4096)
	if err != nil {
		return adapterPolicy{}, err
	}
	var policy adapterPolicy
	found := false
	rows, complete := strings.CutSuffix(string(data), "\n")
	if !complete && len(data) != 0 {
		return adapterPolicy{}, errors.New("incomplete hook policy")
	}
	for _, line := range strings.Split(rows, "\n") {
		f := strings.Split(line, " ")
		if f[0] != adapter {
			continue
		}
		if found || len(f) != 4 || (f[1] != "0" && f[1] != "1") || (f[2] != "0" && f[2] != "1") {
			return adapterPolicy{}, errors.New("invalid hook policy")
		}
		epoch, err := strconv.ParseUint(f[3], 10, 64)
		if err != nil {
			return adapterPolicy{}, errors.New("invalid policy epoch")
		}
		policy = adapterPolicy{Enabled: f[1] == "1", Reminder: f[2] == "1", Epoch: epoch}
		found = true
	}
	return policy, nil
}

// turnEndListed is a wait hint. The dispatcher applies the version override
// before deciding whether the Stop may continue the turn.
func turnEndListed(rv *os.Root, adapter, platform, mode, manifestRev string) bool {
	data, err := ident.ReadPrivateFile(rv, "turn_end", 64<<10)
	if err != nil {
		return false
	}
	rows, complete := strings.CutSuffix(string(data), "\n")
	if !complete {
		return false
	}
	for _, line := range strings.Split(rows, "\n") {
		fields := strings.Split(line, " ")
		if len(fields) == 5 && fields[1] != "" && fields[0] == adapter && fields[2] == platform &&
			fields[3] == mode && fields[4] == manifestRev {
			return true
		}
	}
	return false
}

// hasDefault is only a hint for a first join. The dispatcher rechecks it in the enrollment transaction.
func hasDefault(ctx context.Context, rv *os.Root, repo string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if !filepath.IsAbs(repo) || filepath.Clean(repo) != repo {
		return false, nil
	}
	sum := sha256.Sum256([]byte(repo))
	return repoSetHas(ctx, rv, hex.EncodeToString(sum[:]))
}

// AutoJoinLine is the line of the published repository set that says the
// owner's automatic joining is on: any Git repository may then be joined.
const AutoJoinLine = "auto"

func autoJoin(ctx context.Context, rv *os.Root) bool {
	on, _ := repoSetHas(ctx, rv, AutoJoinLine)
	return on
}

func repoSetHas(ctx context.Context, rv *os.Root, want string) (bool, error) {
	f, err := spool.OpenServerFile(rv, "repo-defaults", os.O_RDONLY)
	if err != nil {
		return false, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 4096)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if scanner.Text() == want {
			return true, nil
		}
	}
	return false, scanner.Err()
}

func currentAck(rv, leaf *os.Root, generation, effect, turn string) (wire.HookAck, error) {
	var ack wire.HookAck
	if effect != "notice" && effect != "reminder" {
		return ack, errors.New("unknown stamp effect")
	}
	if effect == "reminder" && (turn == "" || strings.ContainsAny(turn, " \n\r\t")) {
		return ack, errors.New("invalid stamp turn")
	}
	ready, err := ident.ReadPrivateFile(rv, "ready", 4096)
	if err != nil {
		return ack, err
	}
	var marker struct {
		Instance string `json:"instance"`
		Version  int    `json:"v"`
	}
	if err := json.Unmarshal(ready, &marker); err != nil || marker.Instance == "" || marker.Version != wire.Version {
		return ack, errors.New("invalid dispatcher marker")
	}
	gen, err := ident.ReadPrivateFile(leaf, "gen", 1024)
	if err != nil {
		return ack, err
	}
	if generation == "" || strings.TrimSpace(string(gen)) != generation {
		return ack, errors.New("member generation changed")
	}
	stamp, err := ident.ReadPrivateFile(leaf, "stamp", 4096)
	if err != nil {
		return ack, err
	}
	line, complete := strings.CutSuffix(string(stamp), "\n")
	fields := strings.Split(line, " ")
	if !complete || len(fields) != 4 || fields[0] != marker.Instance || fields[1] != generation {
		return ack, errors.New("stale member stamp")
	}
	notice, e := strconv.ParseUint(fields[2], 10, 64)
	reminder, err := strconv.ParseUint(fields[3], 10, 64)
	if e != nil || err != nil {
		return ack, errors.New("invalid stamp sequence")
	}
	ack = wire.HookAck{Kind: effect, Instance: marker.Instance, Generation: generation, Seq: notice}
	if effect == "reminder" {
		ack.Seq, ack.TurnRef = reminder, turn
	}
	return ack, nil
}

// Older hooks use seen-notice and do not check historical offers.
const seenReceiptNotice = "seen-notice-receipts"

func seenFile(effect string) string {
	if effect == "notice" {
		return seenReceiptNotice
	}
	return "seen-" + effect
}

func seenText(ack wire.HookAck) string {
	text := fmt.Sprintf("%s %s %d", ack.Instance, ack.Generation, ack.Seq)
	if ack.Kind == "reminder" {
		text += " " + ack.TurnRef
	}
	return text + "\n"
}

func stampClean(rv, leaf *os.Root, generation, effect, turn string) bool {
	ack, err := currentAck(rv, leaf, generation, effect, turn)
	if err != nil {
		return false
	}
	seen, err := ident.ReadPrivateFile(leaf, seenFile(effect), 4096)
	return err == nil && string(seen) == seenText(ack)
}

func hasSeenNotice(leaf *os.Root, generation string) bool {
	for _, name := range []string{seenReceiptNotice, "seen-notice"} {
		seen, err := ident.ReadPrivateFile(leaf, name, 4096)
		if fields := strings.Fields(string(seen)); err == nil && len(fields) == 3 && fields[1] == generation {
			return true
		}
	}
	return false
}

// TurnEndAskFile is the dispatcher's record, per member leaf, of whether a
// turn-end notice may have an ask to announce: "<instance> <generation> 0|1".
const TurnEndAskFile = "turn-end-ask"

// noTurnEndAsk reports whether the current dispatcher recorded, for this
// generation, that no ask waits for a turn-end notice. Anything else waits.
func noTurnEndAsk(rv, leaf *os.Root, generation string) bool {
	ready, err := ident.ReadPrivateFile(rv, "ready", 4096)
	if err != nil {
		return false
	}
	var marker struct {
		Instance string `json:"instance"`
	}
	if json.Unmarshal(ready, &marker) != nil || marker.Instance == "" || generation == "" {
		return false
	}
	record, err := ident.ReadPrivateFile(leaf, TurnEndAskFile, 4096)
	return err == nil && string(record) == marker.Instance+" "+generation+" 0\n"
}

const seenJoinHint = "seen-join-hint"

const JoinHintOfferFile = "join-hint-offer"

func ReadJoinHintOffer(leaf *os.Root, generation string) (wire.HookJoinHintAck, bool) {
	return readJoinHintRecord(leaf, JoinHintOfferFile, generation)
}

func CurrentJoinHintOffer(leaf *os.Root, ack wire.HookJoinHintAck) bool {
	offer, ok := ReadJoinHintOffer(leaf, ack.Generation)
	return ok && offer.SameOffer(ack)
}

func ReadJoinHintAck(leaf *os.Root, generation string) (wire.HookJoinHintAck, bool) {
	return readJoinHintRecord(leaf, seenJoinHint, generation)
}

func readJoinHintRecord(leaf *os.Root, name, generation string) (wire.HookJoinHintAck, bool) {
	var ack wire.HookJoinHintAck
	data, err := ident.ReadPrivateFile(leaf, name, 512)
	if err != nil || json.Unmarshal(data, &ack) != nil {
		return ack, false
	}
	return ack, ack.ValidFor(generation)
}

const seenReach = "seen-reach"

const ReachOfferFile = "reach-offer"

func ReadReachOffer(leaf *os.Root, generation string) (wire.HookReachAck, bool) {
	return readReachRecord(leaf, ReachOfferFile, generation)
}

func CurrentReachOffer(leaf *os.Root, ack wire.HookReachAck) bool {
	offer, ok := ReadReachOffer(leaf, ack.Generation)
	return ok && offer.SameOffer(ack)
}

// ReadReachAck is the reach line the hook last printed.
func ReadReachAck(leaf *os.Root, generation string) (wire.HookReachAck, bool) {
	return readReachRecord(leaf, seenReach, generation)
}

func readReachRecord(leaf *os.Root, name, generation string) (wire.HookReachAck, bool) {
	var ack wire.HookReachAck
	data, err := ident.ReadPrivateFile(leaf, name, 512)
	if err != nil || json.Unmarshal(data, &ack) != nil {
		return ack, false
	}
	return ack, ack.ValidFor(generation)
}

func saveSeen(rv, leaf *os.Root, generation, effect, turn string, ack wire.HookAck) error {
	// Hold the same stable inode across the comparison and atomic publication.
	// Losing a lock costs a future check; no hook waits on another hook's disk I/O.
	lock, err := leaf.Open(".")
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return err
	}
	current, err := currentAck(rv, leaf, generation, effect, turn)
	if err != nil {
		return err
	}
	if current != ack {
		return errors.New("hook acknowledgement no longer matches")
	}
	return writeRecord(leaf, seenFile(effect), seenText(ack))
}

func writeRecord(leaf *os.Root, name, record string) error {
	part := wire.NewID() + ".part"
	f, err := leaf.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer leaf.Remove(part)
	_, err = f.WriteString(record)
	if err = errors.Join(err, f.Close()); err != nil {
		return err
	}
	return leaf.Rename(part, name)
}

type hookCounter struct {
	file   io.WriteCloser
	prefix string
}

func openHookCounter(rv *os.Root, adapter, event string, entry time.Time) *hookCounter {
	// Names can originate in argv. Only names in our manifest reach the log.
	m, ok := loadManifest(adapter)
	if !ok {
		adapter = "unknown"
	}
	if _, ok := m.event(event); !ok {
		event = "unknown"
	}
	if !time.Now().Before(entry.Add(Budget)) {
		return nil
	}
	dir, err := spool.OpenServerChild(rv, "hooks", true)
	if err != nil {
		return nil
	}
	defer dir.Close()
	if !time.Now().Before(entry.Add(Budget)) {
		return nil
	}
	f, err := spool.OpenServerFile(dir, entry.UTC().Format("20060102"), os.O_WRONLY|os.O_CREATE|os.O_APPEND)
	if err != nil {
		return nil
	}
	return &hookCounter{file: f, prefix: adapter + " " + event + " "}
}

func (c *hookCounter) append(outcome string, entry time.Time) {
	switch outcome {
	case "off", "unmatched", "identity", "clean", "rpc", "deadline", "refused", "stale_epoch", "no_dispatcher":
	default:
		outcome = "refused"
	}
	record := fmt.Sprintf("%s%s %d\n", c.prefix, outcome, time.Since(entry).Milliseconds())
	if len(record) <= 64 {
		_, _ = io.WriteString(c.file, record)
	}
}

// launchMarks names, by PID, the processes in which a member's session
// started, so that a /clear start of a new session there is asked about.
const launchMarks = "launches"

func markLaunch(rv *os.Root, pid string) {
	dir, err := spool.OpenServerChild(rv, launchMarks, true)
	if err != nil {
		return
	}
	defer dir.Close()
	if f, err := spool.OpenServerFile(dir, pid, os.O_WRONLY|os.O_CREATE); err == nil {
		_ = f.Close()
	}
}

func launchMarked(rv *os.Root, pid string) bool {
	dir, err := spool.OpenServerChild(rv, launchMarks, false)
	if err != nil {
		return false
	}
	defer dir.Close()
	_, err = dir.Lstat(pid)
	return err == nil
}
