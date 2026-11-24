package hook

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/wire"
)

func hookFileRoot(t *testing.T) *os.Root {
	t.Helper()
	path := t.TempDir()
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	return root
}

func hookFile(t *testing.T, root *os.Root, name, content string) {
	t.Helper()
	if err := root.WriteFile(name, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestReadHookPolicy(t *testing.T) {
	rv := hookFileRoot(t)
	hookFile(t, rv, "policy", "claude 1 0 4\ncodex 1 1 7\n")
	got, err := readPolicy(rv, "codex")
	if err != nil || !got.Enabled || !got.Reminder || got.Epoch != 7 {
		t.Fatalf("codex policy = %+v, %v", got, err)
	}
	for _, contents := range []string{"claude 1 0 4\n", "codex 0 0 8\n", "codex 1 0 -1\n",
		"codex yes 1 4\n", "codex 1 1 7", "codex 1 1 7\ncodex 0 0 8\n"} {
		hookFile(t, rv, "policy", contents)
		got, _ := readPolicy(rv, "codex")
		if got.Enabled {
			t.Errorf("policy %q unexpectedly enabled", contents)
		}
	}
	if err := rv.Remove("policy"); err != nil {
		t.Fatal(err)
	}
	if p, _ := readPolicy(rv, "codex"); p.Enabled {
		t.Fatal("policy missing but adapter enabled")
	}
}

func TestHookStampScopeAndAcknowledgements(t *testing.T) {
	rv, leaf := hookFileRoot(t), hookFileRoot(t)
	hookFile(t, rv, "ready", `{"v":1,"instance":"instance","pid":1}`)
	hookFile(t, leaf, "gen", "gen\n")
	hookFile(t, leaf, "stamp", "instance gen 3 8\n")
	for _, effect := range []string{"notice", "reminder"} {
		if stampClean(rv, leaf, "gen", effect, "turn1") {
			t.Fatalf("clean without %s acknowledgement", effect)
		}
	}
	n := wire.HookAck{Kind: "notice", Instance: "instance", Generation: "gen", Seq: 3}
	if err := saveSeen(rv, leaf, "gen", "notice", "", n); err != nil {
		t.Fatal(err)
	}
	if !stampClean(rv, leaf, "gen", "notice", "") || stampClean(rv, leaf, "gen", "reminder", "turn1") {
		t.Fatal("notice ack incorrectly scoped")
	}
	r := wire.HookAck{Kind: "reminder", Instance: "instance", Generation: "gen", Seq: 8, TurnRef: "turn1"}
	if err := saveSeen(rv, leaf, "gen", "notice", "", r); err == nil || stampClean(rv, leaf, "gen", "reminder", "turn1") {
		t.Fatal("accepted wrong-effect ack")
	}
	if err := saveSeen(rv, leaf, "gen", "reminder", "turn1", r); err != nil {
		t.Fatal(err)
	}
	if !stampClean(rv, leaf, "gen", "reminder", "turn1") || stampClean(rv, leaf, "gen", "reminder", "turn2") {
		t.Fatal("reminder ack incorrectly scoped")
	}
	hookFile(t, leaf, "stamp", "instance gen 4 8\n")
	if stampClean(rv, leaf, "gen", "notice", "") || !stampClean(rv, leaf, "gen", "reminder", "turn1") {
		t.Fatal("new mail must dirty only the notice check")
	}
	if err := saveSeen(rv, leaf, "gen", "notice", "", n); err == nil {
		t.Fatal("trusted a stale sequence")
	}
	n.Seq = 4
	if err := saveSeen(rv, leaf, "gen", "notice", "", n); err != nil {
		t.Fatal(err)
	}
	n.Seq = 3
	if err := saveSeen(rv, leaf, "gen", "notice", "", n); err == nil || !stampClean(rv, leaf, "gen", "notice", "") {
		t.Fatal("older response replaced current acknowledgement")
	}
	hookFile(t, rv, "ready", `{"v":1,"instance":"new","pid":1}`)
	if stampClean(rv, leaf, "gen", "notice", "") {
		t.Fatal("accepted an old dispatcher instance")
	}
	hookFile(t, rv, "ready", `{"v":1,"instance":"instance","pid":1}`)
	hookFile(t, leaf, "gen", "new-gen")
	if stampClean(rv, leaf, "gen", "notice", "") {
		t.Fatal("ignored a changed leaf generation")
	}
}

func TestHookRepositoryIndex(t *testing.T) {
	rv := hookFileRoot(t)
	present, absent := "/work/one", "/work/two"
	hookFile(t, rv, "repo-defaults", fmt.Sprintf("%x\n", sha256.Sum256([]byte(present))))
	for _, tc := range []struct {
		path string
		want bool
	}{{present, true}, {absent, false}} {
		got, err := hasDefault(context.Background(), rv, tc.path)
		if err != nil || got != tc.want {
			t.Errorf("hasDefault(%q) = %t, %v", tc.path, got, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if ok, err := hasDefault(ctx, rv, present); err == nil || ok {
		t.Error("approved lookup after deadline")
	}
}

func TestHookIncompleteStampAndBusyLeafStayDirty(t *testing.T) {
	rv, leaf := hookFileRoot(t), hookFileRoot(t)
	hookFile(t, rv, "ready", `{"v":1,"instance":"dispatcher"}`)
	hookFile(t, leaf, "gen", "generation\n")
	hookFile(t, leaf, seenReceiptNotice, "dispatcher generation 9\n")
	for _, stamp := range []string{"dispatcher generation 9 3", "dispatcher generation 9\n3\n"} {
		hookFile(t, leaf, "stamp", stamp)
		if stampClean(rv, leaf, "generation", "notice", "") {
			t.Errorf("trusted malformed stamp %q", stamp)
		}
	}
	hookFile(t, leaf, "stamp", "dispatcher generation 10 3\n")
	lock, err := leaf.Open(".")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	n := wire.HookAck{Kind: "notice", Instance: "dispatcher", Generation: "generation", Seq: 10}
	if err := saveSeen(rv, leaf, "generation", "notice", "", n); err == nil || stampClean(rv, leaf, "generation", "notice", "") {
		t.Error("busy leaf must leave the newer notice dirty")
	}
}

func countHook(rv *os.Root, adapter, event, outcome string, entry time.Time) {
	counter := openHookCounter(rv, adapter, event, entry)
	if counter != nil {
		defer counter.file.Close()
		counter.append(outcome, entry)
	}
}

func TestHookCountersAreBoundedAndPrivate(t *testing.T) {
	rv := hookFileRoot(t)
	entry := time.Now().UTC()
	countHook(rv, "claude", "Stop", "clean", entry)
	countHook(rv, "codex", "UserPromptSubmit", "rpc", entry)
	countHook(rv, "private-adapter", "private-event", "private-outcome", entry)
	data, err := rv.ReadFile(filepath.Join("hooks", entry.Format("20060102")))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "claude Stop clean ") || !strings.Contains(string(data), "codex UserPromptSubmit rpc ") || strings.Contains(string(data), "private") {
		t.Fatalf("unexpected daily records %q", data)
	}
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if len(line)+1 > 64 || len(strings.Fields(line)) != 4 {
			t.Fatalf("malformed counter entry %q", line)
		}
	}
	if fi, err := rv.Stat(filepath.Join("hooks", entry.Format("20060102"))); err != nil || fi.Mode().Perm() != 0600 {
		t.Fatal("counter file must be private")
	}
}

func TestHookConcurrentCountersKeepEveryRecord(t *testing.T) {
	rv := hookFileRoot(t)
	entry := time.Now().UTC()
	const writers = 20
	var wg sync.WaitGroup
	for range writers {
		wg.Go(func() { countHook(rv, "claude", "Stop", "clean", entry) })
	}
	wg.Wait()
	data, err := rv.ReadFile(filepath.Join("hooks", entry.Format("20060102")))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != writers {
		t.Fatalf("got %d records, want %d", len(lines), writers)
	}
	for _, line := range lines {
		if !strings.HasPrefix(line, "claude Stop clean ") || len(strings.Fields(line)) != 4 {
			t.Fatalf("interleaved counter record %q", line)
		}
	}
}
