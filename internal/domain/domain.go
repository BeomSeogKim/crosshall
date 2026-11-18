// Package domain defines the command handler boundary.
package domain

import (
	"context"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

// Request carries typed command arguments and the operation key.
type Request struct {
	OpKey   string
	Command Command
}

// Command is a decoded domain command.
type Command interface {
	command()
}

// CreateChannel creates a new channel.
type CreateChannel struct {
	Name  string
	Topic string
}

func (CreateChannel) command() {}

// Join binds the caller to a channel.
type Join struct {
	Channel string
	Alias   string
	Focus   string
}

func (Join) command() {}

// Who lists members in the caller's channel.
type Who struct{}

func (Who) command() {}

// Handler executes a command within the dispatcher-owned transaction.
type Handler interface {
	Handle(ctx context.Context, tx store.Tx, caller core.Caller, request Request) (core.Result, error)
}
