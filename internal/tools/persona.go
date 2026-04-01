package tools

import (
	"context"
	"strings"
	"time"

	"naima/internal/persona"
)

const defaultPersonaToolTimeout = 8 * time.Second

type PersonaStore interface {
	ListFacts(ctx context.Context) ([]persona.Fact, error)
	GetFactsByKey(ctx context.Context, key string) ([]persona.Fact, error)
	SetFact(ctx context.Context, fact persona.Fact) (persona.Fact, error)
	DeleteFact(ctx context.Context, key string, value string) error
}

type PersonaTool struct {
	store PersonaStore
}

type personaParams struct {
	Operation  string  `json:"operation"`
	Key        string  `json:"key,omitempty"`
	Value      string  `json:"value,omitempty"`
	Source     string  `json:"source,omitempty"`
	Confidence float64 `json:"confidence,omitempty"`
	Reason     string  `json:"reason,omitempty"`
}

func NewPersonaTool(store PersonaStore) Tool {
	return &PersonaTool{store: store}
}

func (t *PersonaTool) GetName() string {
	return "persona"
}

func (t *PersonaTool) GetDescription() string {
	return "Stores and retrieves durable user persona facts such as email, interests, preferences, and other profile details."
}

func (t *PersonaTool) GetFunction() func(params string) string {
	return func(params string) string {
		if t.store == nil {
			return errorJSON("persona storage is not configured")
		}
		var in personaParams
		if err := jsonUnmarshal(params, &in); err != nil {
			return errorJSON("invalid params: " + err.Error())
		}
		op := strings.ToLower(strings.TrimSpace(in.Operation))
		if op == "" {
			op = "list"
		}
		ctx, cancel := context.WithTimeout(context.Background(), defaultPersonaToolTimeout)
		defer cancel()

		switch op {
		case "list":
			facts, err := t.store.ListFacts(ctx)
			if err != nil {
				return errorJSON("list persona facts failed: " + err.Error())
			}
			return mustJSON(map[string]any{"operation": "list", "count": len(facts), "facts": facts})
		case "get":
			if strings.TrimSpace(in.Key) == "" {
				return errorJSON("key is required")
			}
			facts, err := t.store.GetFactsByKey(ctx, in.Key)
			if err != nil {
				return errorJSON("get persona facts failed: " + err.Error())
			}
			return mustJSON(map[string]any{"operation": "get", "key": strings.TrimSpace(in.Key), "count": len(facts), "facts": facts})
		case "set", "save":
			if strings.TrimSpace(in.Key) == "" || strings.TrimSpace(in.Value) == "" {
				return errorJSON("key and value are required")
			}
			fact, err := t.store.SetFact(ctx, persona.Fact{
				Key:        strings.TrimSpace(in.Key),
				Value:      strings.TrimSpace(in.Value),
				Source:     strings.TrimSpace(in.Source),
				Confidence: in.Confidence,
				Reason:     strings.TrimSpace(in.Reason),
			})
			if err != nil {
				return errorJSON("set persona fact failed: " + err.Error())
			}
			return mustJSON(map[string]any{"operation": "set", "fact": fact})
		case "delete":
			if strings.TrimSpace(in.Key) == "" {
				return errorJSON("key is required")
			}
			if err := t.store.DeleteFact(ctx, in.Key, in.Value); err != nil {
				return errorJSON("delete persona fact failed: " + err.Error())
			}
			return mustJSON(map[string]any{"operation": "delete", "key": strings.TrimSpace(in.Key), "value": strings.TrimSpace(in.Value)})
		default:
			return errorJSON("unsupported operation: " + op)
		}
	}
}

func (t *PersonaTool) IsImmediate() bool {
	return false
}

func (t *PersonaTool) GetParameters() Parameters {
	return Parameters{
		Type: "object",
		Properties: map[string]any{
			"operation": map[string]any{
				"type":        "string",
				"description": "Operation: list, get, set/save, or delete.",
				"enum":        []string{"list", "get", "set", "save", "delete"},
			},
			"key": map[string]any{
				"type":        "string",
				"description": "Persona fact key such as email, news_interest, preference, location, or name.",
			},
			"value": map[string]any{
				"type":        "string",
				"description": "Persona fact value.",
			},
			"source": map[string]any{
				"type":        "string",
				"description": "Fact source: explicit or inferred.",
				"enum":        []string{"explicit", "inferred"},
			},
			"confidence": map[string]any{
				"type":        "number",
				"description": "Optional confidence score between 0 and 1.",
				"minimum":     0,
				"maximum":     1,
			},
			"reason": map[string]any{
				"type":        "string",
				"description": "Optional note about why the fact was stored.",
			},
		},
		Required: []string{"operation"},
	}
}
