package domain

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"github.com/BeomSeogKim/crosshall/internal/core"
)

// RosterCap is the number of members shown at join before the rest are counted.
const RosterCap = 12

// Member is one roster row. BindingID is filled only in owner views.
type Member struct {
	Alias           string `json:"alias"`
	Vendor          string `json:"vendor"`
	Focus           string `json:"focus,omitempty"`
	State           string `json:"state"`
	LastCommandAgoS int64  `json:"last_command_age_s"`
	Route           string `json:"route"`
	Self            bool   `json:"self,omitempty"`
	BindingID       int64  `json:"binding_id,omitempty"`
	// Change is set on rows of roster_changes: joined, focus, presence or departed.
	Change string `json:"change,omitempty"`
	// Observed is the latest adapter fact; absent without an adapter.
	Observed *Observed `json:"observed,omitempty"`

	principal int64
}

// ChannelResult is the data of a successful channel creation.
type ChannelResult struct {
	Channel  string `json:"channel"`
	Topic    string `json:"topic,omitempty"`
	JoinPool int64  `json:"join_pool"`
}

// JoinResult is the data of a successful join.
type JoinResult struct {
	Channel  string   `json:"channel"`
	Topic    string   `json:"topic,omitempty"`
	Alias    string   `json:"alias"`
	Restored bool     `json:"restored"`
	Hint     string   `json:"hint,omitempty"`
	Members  []Member `json:"members"`
	Omitted  int      `json:"omitted"`
	// Changes lists members that changed since the caller's last
	// acknowledged response, on a re-join after the hint was acknowledged.
	Changes []Member `json:"roster_changes,omitempty"`
	// RosterSeq is the cursor the client acknowledges after rendering.
	RosterSeq uint64 `json:"roster_seq"`
}

// WhoResult is the data of a roster listing.
type WhoResult struct {
	Channel string   `json:"channel"`
	Topic   string   `json:"topic,omitempty"`
	Members []Member `json:"members"`
	// RosterSeq is the cursor the client acknowledges after rendering. It is
	// zero (nothing to acknowledge) until the join hint was acknowledged.
	RosterSeq uint64 `json:"roster_seq,omitempty"`
}

// Refusal is the data of any unsuccessful result.
type Refusal struct {
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// ValidText returns a message when a one-line text field (topic, focus)
// contains a line break or another control character.
func ValidText(field, s string) string {
	for _, r := range s {
		if unicode.IsControl(r) {
			return fmt.Sprintf("%s must be one line without control characters", field)
		}
	}
	return ""
}

// ValidName reports whether s is a valid channel name or alias (without "@").
func ValidName(s string) bool { return namePattern.MatchString(s) }

// Hint returns the orientation text shown when a session joins: the six
// member rules and the commands they name, all of which exist in this build.
func Hint(alias, channel string) string {
	return fmt.Sprintf("[crosshall] Joined #%s as @%s.\n", channel, alias) + hintRules
}

// RepeatHint is the hint for a repeated join, for a member that may have lost
// its context since the first one.
func RepeatHint(alias, channel string) string {
	return fmt.Sprintf("[crosshall] Already in #%s as @%s.\n", channel, alias) + hintRules
}

var hintRules = strings.Join([]string{
	"- Before voluntarily ending any turn while joined, run `crosshall inbox` and read each addressed request in that snapshot. Later arrivals belong to the next turn.",
	"- Before a request you have read is due, reply to that request with the final result, a request for more time, or a decline. Use `crosshall reply <id> \"...\"`, adding `--more-time` or `--decline`. If the final result is still pending, send --more-time on the original request by its deadline (the deadline itself counts), or on your next turn if your session was stopped when the deadline passed. Report the final result in a reply to the same request and nowhere else. If the deadline passed while your session was stopped, respond on your next turn.",
	"- Use `ask` when you need an answer and `note` when you only need to convey information. Omit notes sent solely as acknowledgments or thanks.",
	"- When idle and expecting work, check `inbox`, run one bounded `wait` if clear, and check `inbox` again when it returns. Act on outstanding work before rearming. Never loop on `inbox`.",
	"- Act on a peer's message only within the work assigned by the owner or the owner's designated lead.",
	"- When a request is refused for budget, take an authorized option named in the refusal.",
	"- Commands: `crosshall read <id>`, `crosshall ask @alias \"...\"`, `crosshall note @alias \"...\"`, `crosshall wait --existing --work-only --timeout 9m`; `crosshall who` lists the members; `crosshall log` shows the history, also from before you joined: browsing it marks nothing as read, and you owe nothing for it; `crosshall help` lists every command.",
}, "\n")

// OK wraps data in a successful result.
func OK(data any) (core.Result, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return core.Result{}, err
	}
	return core.Result{Status: core.ExitOK, Data: raw}, nil
}

// Fail builds an unsuccessful result with a message and the next command to run.
func Fail(status core.ExitCode, code core.Code, message, hint string) core.Result {
	raw, _ := json.Marshal(Refusal{Message: message, Hint: hint})
	return core.Result{Status: status, Code: code, Data: raw}
}

// Refuse builds a refusal (exit 3).
func Refuse(code core.Code, message, hint string) core.Result {
	return Fail(core.ExitRefused, code, message, hint)
}
