package mcp

import (
	"errors"
	"time"

	"github.com/pyranthus-hq/mora/internal/disposition"
	"github.com/pyranthus-hq/mora/internal/memory"
)

type DecisionBuilder func(createdAt, asOf, durability, flipConditions, reviewBy string) *memory.DecisionValidity

// MemoryFromArgs validates write_memory arguments and constructs the canonical pre-publish record.
func MemoryFromArgs(args map[string]any, now time.Time, decision DecisionBuilder) (memory.Memory, error) {
	for _, key := range []string{"target", "disposition"} {
		if raw, exists := args[key]; exists {
			if _, ok := raw.(string); !ok {
				return memory.Memory{}, errors.New(key + " must be a string")
			}
		}
	}
	_, targetSupplied := args["target"]
	_, dispositionSupplied := args["disposition"]
	target := StringArg(args, "target", "")
	value := StringArg(args, "disposition", "")
	if (targetSupplied && target == "") || (dispositionSupplied && value == "") {
		return memory.Memory{}, errors.New("target and disposition cannot be empty when supplied")
	}
	if err := disposition.ValidateFields(target, value); err != nil {
		return memory.Memory{}, err
	}
	typ := StringArg(args, "type", "insight")
	_, typeSupplied := args["type"]
	if target != "" || value != "" {
		if (typeSupplied && typ != "correction") || (!typeSupplied && typ != "insight") {
			return memory.Memory{}, errors.New("target/disposition require type=correction")
		}
		typ = "correction"
	}
	m := memory.Memory{Scope: StringArg(args, "scope", "global"), Type: typ, Title: StringArg(args, "title", ""), Text: StringArg(args, "text", ""), Source: StringArg(args, "source", "mcp"), CreatedAt: now.Format(time.RFC3339)}
	if target != "" {
		m.Meta = map[string]any{"target": target}
		if value != "" {
			m.Meta["disposition"] = value
		}
	}
	if m.Title == "" || m.Text == "" {
		return memory.Memory{}, errors.New("title and text required")
	}
	asOf := StringArg(args, "as_of", "")
	durability := StringArg(args, "durability", "")
	flip := StringArg(args, "flip_conditions", "")
	reviewBy := StringArg(args, "review_by", "")
	if m.Type == "decision" {
		m.Decision = decision(m.CreatedAt, asOf, durability, flip, reviewBy)
	} else if asOf != "" || durability != "" || flip != "" || reviewBy != "" {
		return memory.Memory{}, errors.New("decision validity fields require type=decision")
	}
	return m, nil
}
