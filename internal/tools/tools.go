package tools

type Tool interface {
	GetName() string
	GetDescription() string
	GetFunction() func(params string) string
	IsImmediate() bool
	GetParameters() Parameters
}

type Parameters struct {
	Type       string   `json:"type"`
	Properties any      `json:"properties"`
	Required   []string `json:"required"`
}
