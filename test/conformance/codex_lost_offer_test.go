//go:build conformance

package conformance

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/hook"
)

// Codex sends prior turns to the model again. Only items after the previous
// assistant reply can establish that this prompt added (or omitted) a line.
func newCodexPendingLines(t *testing.T, raw []byte, resumed bool) []string {
	t.Helper()
	var request struct {
		Input []struct {
			Role    string
			Content []struct{ Text string }
		}
	}
	codexMust(t, json.Unmarshal(raw, &request))
	start := 0
	for i, item := range request.Input {
		if item.Role == "assistant" {
			start = i + 1
		}
	}
	if (start > 0) != resumed {
		t.Fatalf("unexpected prior assistant boundary at item %d", start)
	}
	pattern := regexp.MustCompile(`\[crosshall\] pending: [0-9]+(?: [0-9]+)*`)
	var pending []string
	user := false
	for _, item := range request.Input[start:] {
		for _, part := range item.Content {
			if item.Role == "user" && part.Text == "Run this fixture turn." {
				user = true
			}
			for _, line := range pattern.FindAllString(part.Text, -1) {
				pending = append(pending, item.Role+" "+line)
			}
		}
	}
	if !user {
		t.Fatal("current model input omitted the fixture prompt")
	}
	return pending
}

func TestCodexLostOffer(t *testing.T) {
	if _, err := exec.LookPath("codex"); errors.Is(err, exec.ErrNotFound) {
		t.Skip("no codex binary on PATH")
	}
	var spec hook.InstallSpec
	for _, candidate := range hook.InstallSpecs() {
		if candidate.Adapter == "codex" {
			spec = candidate
		}
	}
	requests := make(chan []byte, 4)
	w := makeMeasurementWorld(t, spec, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			var raw bytes.Buffer
			body := r.Body
			defer body.Close()
			r.Body = io.NopCloser(io.TeeReader(body, &raw))
			next.ServeHTTP(rw, r)
			requests <- raw.Bytes()
		})
	})
	fifo := filepath.Join(w.root, "prompt-stdout")
	w.configure(t, "base")
	configPath := filepath.Join(w.root, "codex", spec.File)
	raw, err := os.ReadFile(configPath)
	codexMust(t, err)
	var config struct {
		Hooks map[string][]codexGroup `json:"hooks"`
	}
	codexMust(t, json.Unmarshal(raw, &config))
	prompt := &config.Hooks["UserPromptSubmit"][0].Hooks[0]
	prompt.Command = "if test -p " + codexQuote(fifo) + "; then exec >" + codexQuote(fifo) + "; fi; exec " + prompt.Command
	raw, err = json.Marshal(config)
	codexMust(t, err)
	codexPut(t, configPath, raw)
	s := startLatencySession(t, w)
	turn := func(resumed bool, want ...string) {
		t.Helper()
		s.turn(t, 0, false, true)
		if got := newCodexPendingLines(t, <-requests, resumed); !slices.Equal(got, want) {
			t.Fatalf("pending lines in newly appended model input: got %q, want %q", got, want)
		}
	}
	turn(false)
	db := openCodexLedger(t, filepath.Join(w.root, "state"))
	var alias string
	codexMust(t, db.QueryRow("SELECT alias FROM binding JOIN principal ON principal.id=binding.principal_id WHERE vendor='codex' AND state='live'").Scan(&alias))
	peer := append(slices.Clone(w.env), "CROSSHALL_ROUTE=0123456789abcdef0123456789abcdef")
	codexMust(t, codexCLI(w.exe, peer, "join", "latency", "--as", "gate-peer"))
	for _, body := range []string{"first fixture request", "second fixture request"} {
		codexMust(t, codexCLI(w.exe, peer, "ask", "@"+alias, body, "--new"))
	}

	// The real hook can publish its request and receive the committed batch.
	// Its stdout then blocks until the callback deadline; nobody drains the FIFO.
	codexMust(t, syscall.Mkfifo(fifo, 0600))
	fd, err := syscall.Open(fifo, syscall.O_RDWR|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	codexMust(t, err)
	defer syscall.Close(fd)
	for fill := make([]byte, 4096); len(fill) != 0; {
		_, err := syscall.Write(fd, fill)
		switch err {
		case nil, syscall.EINTR:
		case syscall.EAGAIN:
			fill = fill[:len(fill)/2]
		default:
			t.Fatal(err)
		}
	}
	turn(true)
	var offered int
	codexMust(t, db.QueryRow(`SELECT count(DISTINCT i.message_id) FROM offer o JOIN offer_item i ON i.offer_id=o.id
		WHERE o.reason='prompt' AND i.message_id IN (1,2)`).Scan(&offered))
	if offered != 2 {
		t.Fatalf("lost callback committed %d of the two IDs", offered)
	}
	waiting := func() int {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), w.exe, "--json", "waiting")
		cmd.Env = peer
		raw, err := cmd.Output()
		codexMust(t, err)
		var envelope struct {
			OK bool
			domain.WaitingResult
		}
		codexMust(t, json.Unmarshal(raw, &envelope))
		rows := envelope.Requests
		if !envelope.OK || len(rows) != 2 {
			t.Fatalf("waiting after offer: %s", raw)
		}
		shown := 0
		for _, row := range rows {
			if len(row.Recipients) != 1 || row.Recipients[0].PickedUpAtMS != nil {
				t.Fatalf("expected one unread recipient for request %d: %+v", row.ID, row.Recipients)
			}
			if row.Recipients[0].ShownAtMS != nil {
				shown++
			}
		}
		return shown
	}
	if got := waiting(); got != 0 {
		t.Errorf("waiting reported %d requests shown after the failed write", got)
	}
	codexMust(t, os.Remove(fifo))
	turn(true, "developer [crosshall] pending: 1 2")
	deadline := time.Now().Add(5 * time.Second)
	for waiting() != 2 {
		if time.Now().After(deadline) {
			t.Fatal("waiting did not confirm the two retried IDs")
		}
		time.Sleep(10 * time.Millisecond)
	}
	turn(true)
}
