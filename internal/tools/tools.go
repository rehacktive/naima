package tools

import "context"

type Tool interface {
	GetName() string
	GetDescription() string
	GetFunction() func(params string) string
	IsImmediate() bool
	GetParameters() Parameters
}

type MaxToolRoundsProvider interface {
	MaxToolRounds() int
}

type ImmediateToolProvider interface {
	IsImmediateForParams(params string) bool
}

type ContextToolProvider interface {
	Execute(ctx context.Context, params string) string
}

type Parameters struct {
	Type       string   `json:"type"`
	Properties any      `json:"properties"`
	Required   []string `json:"required"`
}
