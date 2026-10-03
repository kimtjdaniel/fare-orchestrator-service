package llm

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var (
	approveRe = regexp.MustCompile(`(?i)✅|👍|\b(yes|yep|approve|book it|do it|go)\b`)
	rejectRe  = regexp.MustCompile(`(?i)❌|👎|\b(no|nope|reject)\b`)
	cancelRe  = regexp.MustCompile(`(?i)\b(cancel|start over|never ?mind)\b`)
	reviseRe  = regexp.MustCompile(`(?i)\b(cheaper|instead|swap|change|different)\b`)
	numberRe  = regexp.MustCompile(`\b([1-3])\b`)
)

// MockLLM is a deterministic stand-in for Gemini. It only knows how to classify replies
// (interpret_reply, via regex) offline; record_preferences/propose_options have no canned data
// and need a real GeminiLLM (MOCK_LLM=false).
type MockLLM struct {
	Calls []string
}

func (m *MockLLM) Structured(ctx context.Context, system string, messages []Message, schema Schema) (map[string]any, error) {
	m.Calls = append(m.Calls, schema.Name)
	switch schema.Name {
	case "interpret_reply":
		var text string
		if len(messages) > 0 {
			if s, ok := messages[len(messages)-1].Content.(string); ok {
				text = s
			}
		}
		switch {
		case cancelRe.MatchString(text):
			return map[string]any{"intent": "cancel"}, nil
		case numberRe.MatchString(text):
			n, _ := strconv.Atoi(numberRe.FindStringSubmatch(text)[1])
			return map[string]any{"intent": "choose", "option_number": n}, nil
		case reviseRe.MatchString(text):
			return map[string]any{"intent": "revise", "revision_request": text}, nil
		case approveRe.MatchString(text):
			return map[string]any{"intent": "approve"}, nil
		case rejectRe.MatchString(text):
			return map[string]any{"intent": "reject"}, nil
		default:
			intent := "other"
			if strings.Contains(text, "?") {
				intent = "question"
			}
			return map[string]any{"intent": intent}, nil
		}
	default:
		return nil, fmt.Errorf("MockLLM has no canned answer for %s", schema.Name)
	}
}

func (m *MockLLM) Agent(ctx context.Context, system string, messages []Message, tools []Tool, handlers map[string]Handler) (string, error) {
	m.Calls = append(m.Calls, "agent")
	return "(mock) Good question! I'll have a real answer once MOCK_LLM=false.", nil
}
