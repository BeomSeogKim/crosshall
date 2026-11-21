//go:build e2e

package e2e

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/wire"

	_ "modernc.org/sqlite"
)

// binary is the crosshall executable built once for the whole package.
var binary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "crosshall-e2e-bin-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	binary = filepath.Join(dir, "crosshall")
	build := exec.Command("go", "build", "-tags=testvendorhome", "-o", binary, "../../cmd/crosshall")
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "build failed:", err)
		os.Exit(1)
	}
	// The first launch of a fresh build pays a one-time start cost (#95);
	// pay it here so timed hook tests measure only the per-launch cost.
	warm := exec.Command(binary, "version")
	warm.Env = []string{}
	if out, err := warm.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "warm-up failed: %v: %s\n", err, out)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// world is one isolated installation: a home, a state root that does not
// exist yet, and the rendezvous root the dispatcher derives from it.
type world struct {
	t       *testing.T
	home    string
	state   string
	lease   *os.File
	workers *os.File
}

func newWorld(t *testing.T) *world {
	t.Helper()
	home := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(home); err == nil {
		home = resolved
	}
	w := &world{t: t, home: home, state: filepath.Join(home, "state")}
	leaseDir, err := os.MkdirTemp(t.TempDir(), "dispatcher-lease-")
	if err != nil {
		t.Fatal(err)
	}
	w.workers, err = os.OpenFile(filepath.Join(leaseDir, "workers.lock"), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.workers.Close() })
	w.lease, err = os.OpenFile(filepath.Join(leaseDir, "runner.lock"), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.cleanup)
	if err := syscall.Flock(int(w.lease.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	return w
}

// baseEnv is built from scratch. CI also passes its test-only read sentinel.
func (w *world) baseEnv() []string {
	env := []string{
		"HOME=" + w.home,
		"PATH=/usr/bin:/bin:/usr/sbin:/sbin",
		"TMPDIR=" + filepath.Join(w.home, "tmp"),
		"CROSSHALL_STATE_DIR=" + w.state,
		w.dispatcherLeaseEnv(),
	}
	if sentinel := os.Getenv("CROSSHALL_TEST_VENDOR_SENTINEL"); sentinel != "" {
		env = append(env, "CROSSHALL_TEST_VENDOR_SENTINEL="+sentinel)
	}
	return env
}

func (w *world) dispatcherLeaseEnv() string {
	return "CROSSHALL_TEST_DISPATCHER_LEASE_DIR=" + filepath.Dir(w.lease.Name())
}

type participant struct {
	w     *world
	route string // empty for the owner
	extra []string
}

func (w *world) owner() *participant { return &participant{w: w} }

func (w *world) member() *participant { return &participant{w: w, route: randomHex(16)} }

type outcome struct {
	stdout, stderr string
	exit           int
	obj            map[string]any
}

func (p *participant) env() []string {
	env := p.w.baseEnv()
	if p.route != "" {
		env = append(env, "CROSSHALL_ROUTE="+p.route)
	}
	return append(env, p.extra...)
}

// run executes crosshall with --json and decodes its single output object.
func (p *participant) run(args ...string) outcome {
	p.w.t.Helper()
	cmd := exec.Command(binary, append([]string{"--json"}, args...)...)
	cmd.Env = p.env()
	cmd.Dir = p.w.home
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	o := outcome{stdout: out.String(), stderr: errOut.String()}
	if exitErr, ok := err.(*exec.ExitError); ok {
		o.exit = exitErr.ExitCode()
	} else if err != nil {
		p.w.t.Fatalf("crosshall %v: %v", args, err)
	}
	if strings.TrimSpace(o.stdout) != "" {
		if err := json.Unmarshal(out.Bytes(), &o.obj); err != nil || o.obj == nil {
			p.w.t.Fatalf("crosshall %v: stdout is not exactly one JSON object: %q", args, o.stdout)
		}
	}
	return o
}

// must runs a command and fails the test unless it exits 0.
func (p *participant) must(args ...string) outcome {
	p.w.t.Helper()
	o := p.run(args...)
	if o.exit != 0 {
		if len(args) == 2 && args[0] == "owner" && args[1] == "start" {
			p.w.logDispatcher()
		}
		p.w.t.Fatalf("crosshall %v: exit %d\nstdout %s\nstderr %s", args, o.exit, o.stdout, o.stderr)
	}
	return o
}

func (w *world) logDispatcher() {
	w.t.Helper()
	file, err := os.Open(filepath.Join(w.state, "dispatcher.log"))
	if err != nil {
		w.t.Logf("dispatcher.log unavailable: %v", err)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		w.t.Logf("read dispatcher.log: %v", err)
		return
	}
	const tail = 4 << 10
	data, err := io.ReadAll(io.NewSectionReader(file, max(0, info.Size()-tail), tail))
	w.t.Logf("dispatcher.log tail: %q (read error: %v)", data, err)
}

// mustFixtureWrite permits a single same-key retry when a fixture write may
// have committed before its reply deadline. The commands under test still use
// must or run directly.
func (p *participant) mustFixtureWrite(args ...string) {
	p.w.t.Helper()
	key := randomHex(16)
	keyed := append([]string{"--key", key}, args...)
	o := p.run(keyed...)
	if o.obj["code"] == "outcome_unknown" && o.obj["op_key"] == key {
		o = p.run(keyed...)
	}
	if o.exit != 0 {
		p.w.t.Fatalf("crosshall fixture %v: exit %d\nstdout %s\nstderr %s", args, o.exit, o.stdout, o.stderr)
	}
}

func (p *participant) mustPlain(args ...string) string {
	p.w.t.Helper()
	cmd := exec.Command(binary, args...)
	cmd.Dir, cmd.Env = p.w.home, p.env()
	out, err := cmd.CombinedOutput()
	if err != nil {
		p.w.t.Fatalf("crosshall %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// aliases returns the member aliases of a who or join result.
func (o outcome) aliases() []string {
	var out []string
	members, _ := o.obj["members"].([]any)
	for _, m := range members {
		if row, ok := m.(map[string]any); ok {
			out = append(out, fmt.Sprint(row["alias"]))
		}
	}
	return out
}

// rendezvous is the per-user root under /tmp for this world's state root.
func (w *world) rendezvous() string {
	sum := sha256.Sum256([]byte(w.state))
	return filepath.Join("/tmp", fmt.Sprintf("crosshall-%d-%s", os.Getuid(), hex.EncodeToString(sum[:])[:8]))
}

// leaf is the spool directory of a route-keyed member.
func (p *participant) leaf() string {
	sum := sha256.Sum256([]byte("route\x00" + p.route))
	return filepath.Join(p.w.rendezvous(), "m", hex.EncodeToString(sum[:])[:16])
}

// keyDigest is the full digest a member's envelope carries.
func (p *participant) keyDigest() string {
	sum := sha256.Sum256([]byte("route\x00" + p.route))
	return hex.EncodeToString(sum[:])
}

// dispatcherPIDs lists the processes with the dispatcher lock file open.
func (w *world) dispatcherPIDs() []int {
	out, err := exec.Command("lsof", "-t", filepath.Join(w.state, "dispatcher.lock")).Output()
	if err != nil {
		return nil
	}
	var pids []int
	for _, f := range strings.Fields(string(out)) {
		if pid, err := strconv.Atoi(f); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids
}

// kill stops the dispatcher with SIGKILL and waits until the lock is free.
func (w *world) kill() {
	w.t.Helper()
	pids := w.dispatcherPIDs()
	if len(pids) == 0 {
		w.t.Fatal("no dispatcher holds the lock")
	}
	lock, err := os.Open(filepath.Join(w.state, "dispatcher.lock"))
	if err != nil {
		w.t.Fatal(err)
	}
	defer lock.Close()
	for _, pid := range pids {
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			w.t.Fatalf("signal dispatcher %d: %v", pid, err)
		}
	}
	// Process enumeration can lose sight of an exiting process before the
	// kernel releases its flock.
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := syscall.Flock(int(lock.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
		if err == nil {
			return
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			w.t.Fatalf("probe dispatcher lock: %v", err)
		}
		if time.Now().After(deadline) {
			w.t.Fatal("dispatcher lock remained held after SIGKILL")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (w *world) stop() {
	for _, pid := range w.dispatcherPIDs() {
		if proc, err := os.FindProcess(pid); err == nil {
			_ = proc.Kill()
		}
	}
	_ = os.RemoveAll(w.rendezvous())
}

func (w *world) cleanup() {
	if err := w.lease.Close(); err != nil {
		w.t.Errorf("release fixture dispatcher lease: %v", err)
	}
	if err := lockFixtureFileUntil(w.workers, time.Now().Add(5*time.Second)); err != nil {
		w.t.Errorf("wait for fixture dispatcher cleanup: %v", err)
		return
	}
	if err := os.RemoveAll(w.rendezvous()); err != nil {
		w.t.Errorf("remove fixture rendezvous: %v", err)
	}
}

func lockFixtureFileUntil(file *os.File, deadline time.Time) error {
	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if !errors.Is(err, syscall.EWOULDBLOCK) || !time.Now().Before(deadline) {
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// db opens the world's store read-only, beside the running dispatcher.
func (w *world) db() *sql.DB {
	w.t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(w.state, "store.sqlite3")+"?mode=ro")
	if err != nil {
		w.t.Fatal(err)
	}
	w.t.Cleanup(func() { db.Close() })
	return db
}

// The three-shell scenario fixture should leave only its own state in HOME.
// With no vendor integration, no command can produce notification evidence.
func (w *world) checkShellDelivery() {
	w.t.Helper()
	entries, err := os.ReadDir(w.home)
	if err != nil {
		w.t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != filepath.Base(w.state) && entry.Name() != "tmp" {
			w.t.Errorf("shell scenario wrote an unexpected home entry: %s", entry.Name())
		}
	}
	var otherVendors, notifications int
	err = w.db().QueryRow(`SELECT
		(SELECT COUNT(*) FROM principal WHERE kind = 'session' AND vendor <> 'route'),
		(SELECT COUNT(*) FROM receipt WHERE stage = 'notified')`).Scan(&otherVendors, &notifications)
	if err != nil || otherVendors != 0 || notifications != 0 {
		w.t.Fatalf("shell scenario has %d non-route participants and %d notification receipts: %v", otherVendors, notifications, err)
	}
}

// counts is a coarse fingerprint of everything a member command can change.
type counts struct {
	principals, keys, bindings, rosterChanges, ops, events int
	focus                                                  string
}

func snapshot(t *testing.T, db *sql.DB) counts {
	t.Helper()
	var c counts
	for q, dst := range map[string]*int{
		`SELECT COUNT(*) FROM principal`:     &c.principals,
		`SELECT COUNT(*) FROM principal_key`: &c.keys,
		`SELECT COUNT(*) FROM binding`:       &c.bindings,
		`SELECT COUNT(*) FROM roster_change`: &c.rosterChanges,
		`SELECT COUNT(*) FROM op`:            &c.ops,
		`SELECT COUNT(*) FROM event`:         &c.events,
	} {
		if err := db.QueryRow(q).Scan(dst); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.QueryRow(`SELECT group_concat(alias || '=' || focus, ',') FROM (SELECT alias, focus FROM binding ORDER BY id)`).Scan(&c.focus); err != nil {
		t.Fatal(err)
	}
	return c
}

func memberRequest(key, gen, digest, args string) wire.Request {
	now := time.Now()
	return wire.Request{
		Version: wire.Version, Attempt: randomHex(16), OpKey: key,
		Generation: strings.TrimSpace(gen), KeyDigest: digest,
		IssuedAtMS: now.UnixMilli(), ExpiresAtMS: now.Add(5 * time.Second).UnixMilli(),
		Verb: "join", Args: json.RawMessage(args),
	}
}

func ownerRequest(key, args string) wire.Request {
	now := time.Now()
	return wire.Request{
		Version: wire.Version, Attempt: randomHex(16), OpKey: key,
		IssuedAtMS: now.UnixMilli(), ExpiresAtMS: now.Add(5 * time.Second).UnixMilli(),
		Verb: "owner channel create", Args: json.RawMessage(args),
	}
}

// plant publishes a request straight into a spool directory, waits for the
// dispatcher's response and returns it. With lose set, the response file is
// deleted afterwards, as if the client had died before reading it.
func plant(t *testing.T, dir string, req wire.Request, lose bool) (wire.Response, string) {
	t.Helper()
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	part := filepath.Join(dir, "in", req.Attempt+".part")
	if err := os.WriteFile(part, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(part, filepath.Join(dir, "in", req.Attempt+".json")); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out", req.Attempt+".json")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(out)
		var resp wire.Response
		if err == nil && json.Unmarshal(raw, &resp) == nil {
			if lose {
				if err := os.Remove(out); err != nil {
					t.Fatal(err)
				}
			}
			return resp, string(resp.Data)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no response to planted request in %s", dir)
	return wire.Response{}, ""
}
