package mcp

import (
	"errors"
	"time"

	"github.com/pyranthus-hq/mora/internal/memory"
)

type DecisionBuilder func(createdAt, asOf, durability, flipConditions, reviewBy string) *memory.DecisionValidity

// MemoryFromArgs validates write_memory arguments and constructs the canonical pre-publish record.
func MemoryFromArgs(args map[string]any, now time.Time, decision DecisionBuilder) (memory.Memory, error) {
	m := memory.Memory{Scope: StringArg(args, "scope", "global"), Type: StringArg(args, "type", "insight"), Title: StringArg(args, "title", ""), Text: StringArg(args, "text", ""), Source: StringArg(args, "source", "mcp"), CreatedAt: now.Format(time.RFC3339)}
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
