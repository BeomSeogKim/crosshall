//go:build conformance

package conformance

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/hook"
)

func TestCodexAppServerSessionMovePromptIsolation(t *testing.T) {
	if _, err := exec.LookPath("codex"); errors.Is(err, exec.ErrNotFound) {
		t.Skip("no codex binary on PATH")
	} else {
		codexMust(t, err)
	}
	var spec hook.InstallSpec
	for _, candidate := range hook.InstallSpecs() {
		if candidate.Adapter == "codex" {
			spec = candidate
			break
		}
	}

	var mu sync.Mutex
	var requests [][]byte
	var nextTool string
	world := makeMeasurementWorld(t, spec, func(http.Handler) http.Handler {
		return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
			if request.Method != http.MethodPost || request.URL.Path != "/v1/responses" {
				http.NotFound(response, request)
				return
			}
			body, err := io.ReadAll(http.MaxBytesReader(response, request.Body, 8<<20))
			if err != nil || !json.Valid(body) {
				http.Error(response, "invalid fixture model request", http.StatusBadRequest)
				return
			}
			mu.Lock()
			requests = append(requests, body)
			number, tool := len(requests), nextTool
			nextTool = ""
			mu.Unlock()
			response.Header().Set("Content-Type", "text/event-stream")
			if tool != "" {
				codexFixtureShell(response, fmt.Sprintf("call_move_%d", number), tool)
				return
			}
			fmt.Fprintf(response, codexResponseStream, number)
		})
	})
	world.configure(t, "base")

	codexMust(t, codexCLI(world.exe, world.env, "owner", "channel", "create", "inventory"))
	sourcePeer := append(slices.Clone(world.env), "CROSSHALL_ROUTE="+strings.Repeat("a", 32))
	destinationPeer := append(slices.Clone(world.env), "CROSSHALL_ROUTE="+strings.Repeat("b", 32))
	codexMust(t, codexCLI(world.exe, sourcePeer, "join", "latency", "--as", "source-peer"))
	codexMust(t, codexCLI(world.exe, destinationPeer, "join", "inventory", "--as", "destination-peer"))

	session := startLatencySession(t, world)
	runTurn := func(prompt string) {
		t.Helper()
		request := session.send(t, "turn/start", map[string]any{
			"threadId": session.thread,
			"input":    []map[string]any{{"type": "text", "text": prompt, "text_elements": []string{}}},
		})
		turnID, answered := "", false
		for {
			event := session.read(t)
			if event.ID == request {
				turnID = event.Result.Turn.ID
			}
			if event.Params.ThreadID != session.thread {
				continue
			}
			switch event.Method {
			case "item/completed":
				answered = answered || (event.Params.Item.Type == "agentMessage" && event.Params.Item.Text == "OK")
			case "turn/completed":
				if turnID == "" || event.Params.Turn.ID != turnID || event.Params.Turn.Status != "completed" || !answered {
					t.Fatalf("Codex did not finish the requested turn: %+v", event)
				}
				return
			}
		}
	}
	runTurn("Start the native Codex fixture.")

	db := openCodexLedger(t, filepath.Join(world.root, "state"))
	var principal, sourceBinding, sourceGeneration int64
	codexMust(t, db.QueryRowContext(t.Context(), "SELECT k.principal_id, b.id, s.generation "+
		"FROM principal_key k JOIN member_selection s ON s.principal_id = k.principal_id "+
		"JOIN binding b ON b.id = s.binding_id JOIN channel ch ON ch.id = b.channel_id "+
		"WHERE k.vendor = 'codex' AND k.key = ? AND k.state = 'live' AND b.state = 'live' AND ch.name = 'latency'",
		session.thread).Scan(&principal, &sourceBinding, &sourceGeneration))

	askFile := filepath.Join(world.root, "source-ask.json")
	moveFile := filepath.Join(world.root, "move-result.json")
	exe := codexQuote(world.exe)
	mu.Lock()
	nextTool = exe + " --json ask @source-peer 'Please reply after the move' > " + codexQuote(askFile) +
		" && " + exe + " --json join inventory --move --as moved > " + codexQuote(moveFile)
	mu.Unlock()
	runTurn("Run the channel move fixture tool.")

	var asked struct {
		OK bool
		ID int64
	}
	var moved struct {
		OK      bool
		Channel string
		Alias   string
		Move    *struct{ Channel string }
	}
	ask, err := os.ReadFile(askFile)
	codexMust(t, err)
	codexMust(t, json.Unmarshal(ask, &asked))
	move, err := os.ReadFile(moveFile)
	codexMust(t, err)
	codexMust(t, json.Unmarshal(move, &moved))
	if !asked.OK || asked.ID == 0 || !moved.OK || moved.Channel != "inventory" ||
		moved.Alias != "moved" || moved.Move == nil || moved.Move.Channel != "latency" {
		t.Fatalf("unexpected native tool results:\nask: %s\nmove: %s", ask, move)
	}

	const lateText, promptText = "Late reply for latency", "Inventory prompt marker"
	codexMust(t, codexCLI(world.exe, sourcePeer, "reply", strconv.FormatInt(asked.ID, 10), lateText))
	codexMust(t, codexCLI(world.exe, destinationPeer, "note", "@moved", promptText))
	var lateReply, note int64
	codexMust(t, db.QueryRowContext(t.Context(), "SELECT id FROM message WHERE kind = 'reply' AND in_reply_to = ? AND body = ?", asked.ID, lateText).Scan(&lateReply))
	codexMust(t, db.QueryRowContext(t.Context(), "SELECT id FROM message WHERE kind = 'note' AND body = ?", promptText).Scan(&note))

	mu.Lock()
	before := len(requests)
	mu.Unlock()
	runTurn("Read the inventory prompt context.")
	mu.Lock()
	destinationRequests := slices.Clone(requests[before:])
	mu.Unlock()
	if len(destinationRequests) != 1 {
		t.Fatalf("inventory generated %d model requests, want one prompt", len(destinationRequests))
	}
	counts, err := codexCounts(destinationRequests[0], map[string]string{
		"inventory": fmt.Sprintf("[crosshall] pending: %d", note),
		"latency":   fmt.Sprintf("[crosshall] pending: %d", lateReply),
	})
	codexMust(t, err)
	if counts["developer"]["inventory"] != 1 || counts["developer"]["latency"] != 0 {
		t.Fatalf("wrong native prompt context: %v", counts)
	}

	var destinationBinding, generation int64
	codexMust(t, db.QueryRowContext(t.Context(), "SELECT b.id, s.generation FROM principal_key k "+
		"JOIN member_selection s ON s.principal_id = k.principal_id "+
		"JOIN binding b ON b.id = s.binding_id JOIN channel ch ON ch.id = b.channel_id "+
		"WHERE k.vendor = 'codex' AND k.key = ? AND k.state = 'live' AND k.principal_id = ? "+
		"AND b.state = 'live' AND ch.name = 'inventory'", session.thread, principal).Scan(&destinationBinding, &generation))
	if sourceBinding == destinationBinding || generation <= sourceGeneration {
		t.Fatalf("the same native Codex thread did not change its selection: binding %d → %d generation %d → %d",
			sourceBinding, destinationBinding, sourceGeneration, generation)
	}
	var notesOffered, repliesRetained int
	codexMust(t, db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM offer_item i JOIN offer o ON o.id = i.offer_id "+
		"WHERE i.message_id = ? AND o.binding_id = ? AND o.reason = 'prompt'", note, destinationBinding).Scan(&notesOffered))
	codexMust(t, db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM recipient r JOIN message m ON m.id = r.message_id "+
		"JOIN binding b ON b.channel_id = m.channel_id AND b.principal_id = r.principal_id "+
		"WHERE r.message_id = ? AND b.id = ? AND b.state = 'departed' AND r.picked_up_at_ms IS NULL "+
		"AND NOT EXISTS (SELECT 1 FROM offer_item i WHERE i.message_id = r.message_id AND i.principal_id = r.principal_id)",
		lateReply, sourceBinding).Scan(&repliesRetained))
	if notesOffered != 1 || repliesRetained != 1 {
		t.Fatalf("inventory prompt offers %d, unread latency replies retained %d", notesOffered, repliesRetained)
	}
}
