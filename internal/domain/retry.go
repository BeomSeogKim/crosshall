package domain

import "github.com/BeomSeogKim/crosshall/internal/core"

// VerbRetry sends a linked device's command again as a new request, after
// its outcome was reported unknown (#400). A linked device's dispatcher
// answers it from its outbox; the home device has no such commands.
const VerbRetry = "retry"

// Retry names the operation key of the command to send again.
type Retry struct {
	Key string `json:"key"`
}

func (Retry) command() {}

func (Retry) Verb() string { return VerbRetry }

func retryAtHome() core.Result {
	return Refuse(CodeNotOnADevice, "crosshall retry sends again a command of a linked device; this session is on the hall's home device",
		"send the command again with --key <key> to replay its stored result")
}
