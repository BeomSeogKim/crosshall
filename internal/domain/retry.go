package domain

import (
	"encoding/json"

	"github.com/BeomSeogKim/crosshall/internal/core"
)

// VerbRetry sends a linked device's command again as a new request, after
// its outcome was reported unknown (#400). A linked device's dispatcher
// answers it from its outbox; the home device has no such commands.
const VerbRetry = "retry"

// Retry names the operation key of the command to send again.
type Retry struct {
	Key string `json:"key"`
}

func (Retry) command() {}

// RetryResult is a successful retry's answer: the command it concerned and
// that command's own result, so the CLI shows it as the command's.
type RetryResult struct {
	Command string          `json:"command"`
	Result  json.RawMessage `json:"result"`
}

func (Retry) Verb() string { return VerbRetry }

func retryAtHome() core.Result {
	return Refuse(CodeNotOnADevice, "crosshall retry sends again a command of a linked device; this session is on the hall's home device",
		"send the command again with --key <key> to replay its stored result")
}
