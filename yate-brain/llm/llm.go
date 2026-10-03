// Package llm abstracts Claude behind two calls: Structured (JSON matching a schema) and Agent
// (a tool-use loop). ClaudeLLM is the real implementation; MockLLM drives the whole flow offline.
package llm

import "context"

// Handler runs one tool call and returns a JSON-able result.
type Handler func(ctx context.Context, input map[string]any) (any, error)

// Schema is one entry from prompts.go: {"name": ..., "schema": {...}}, passed to Structured.
type Schema struct {
	Name   string
	Schema map[string]any
}

// Message is one turn in the conversation Structured/Agent send to Claude.
type Message struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string or []map[string]any (tool_result blocks etc.)
}

// Tool is one entry from AgentTools in tools.go.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

type LLM interface {
	// Structured asks for JSON matching schema.Schema (structured outputs); returns it as a map.
	Structured(ctx context.Context, system string, messages []Message, schema Schema) (map[string]any, error)

	// Agent runs a tool-use loop: Claude asks for tools, we run them via handlers, repeat until
	// Claude answers in text.
	Agent(ctx context.Context, system string, messages []Message, tools []Tool, handlers map[string]Handler) (string, error)
}
