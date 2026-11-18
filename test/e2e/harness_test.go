//go:build e2e

package e2e

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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
	build := exec.Command("go", "build", "-o", binary, "../../cmd/crosshall")
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "build failed:", err)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// world is one isolated installation: a home, a state root that does not
// exist yet, and the rendezvous root the dispatcher derives from it.
type world struct {
	t     *testing.T
	home  string
	state string
}

func newWorld(t *testing.T) *world {
	t.Helper()
	home := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(home); err == nil {
		home = resolved
	}
	w := &world{t: t, home: home, state: filepath.Join(home, "state")}
	t.Cleanup(w.stop)
	return w
}

// baseEnv is built from scratch: nothing is inherited from the agent session
// that runs the tests.
func (w *world) baseEnv() []string {
	return []string{
		"HOME=" + w.home,
		"PATH=/usr/bin:/bin:/usr/sbin:/sbin",
		"TMPDIR=" + filepath.Join(w.home, "tmp"),
		"CROSSHALL_STATE_DIR=" + w.state,
	}
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
		dec := json.NewDecoder(strings.NewReader(o.stdout))
		if err := dec.Decode(&o.obj); err != nil || dec.More() {
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
		p.w.t.Fatalf("crosshall %v: exit %d\nstdout %s\nstderr %s", args, o.exit, o.stdout, o.stderr)
	}
	return o
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

// dispatcherPIDs lists the processes holding the dispatcher lock.
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
	for _, pid := range pids {
		if proc, err := os.FindProcess(pid); err == nil {
			_ = proc.Kill()
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(w.dispatcherPIDs()) > 0 {
		if time.Now().After(deadline) {
			w.t.Fatal("dispatcher did not exit after SIGKILL")
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
