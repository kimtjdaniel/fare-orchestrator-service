package llm

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var (
	approveRe      = regexp.MustCompile(`(?i)✅|👍|\b(yes|yep|approve|book it|do it|go)\b`)
	rejectRe       = regexp.MustCompile(`(?i)❌|👎|\b(no|nope|reject)\b`)
	cancelRe       = regexp.MustCompile(`(?i)\b(cancel|start over|never ?mind)\b`)
	reviseRe       = regexp.MustCompile(`(?i)\b(cheaper|instead|swap|change|different)\b`)
	numberRe       = regexp.MustCompile(`\b([1-3])\b`)
	startTripRe    = regexp.MustCompile(`(?i)\bplan (a|the) trip\b|\bwhere should we go\b`)
	dateRangeRe    = regexp.MustCompile(`(\d{4}-\d{2}-\d{2})\s*(?:to|\.\.)\s*(\d{4}-\d{2}-\d{2})`)
	nightsRe       = regexp.MustCompile(`(\d+)\s*nights?`)
	includesRe     = regexp.MustCompile(`(?i)flights?(?:\s*\+|\s+and\s+|,)\s*(?:stay|hotel)`)
	noneConstrRe   = regexp.MustCompile(`(?i)^\s*none\s*\.?\s*$`)
	routeNewTripRe = regexp.MustCompile(`(?i)\b(?:(?:plan|organize)\s+(?:a|an|another|new|separate)\s+(?:[\p{L}\p{N}-]+\s+){0,6}(?:trip|vacation|holiday)|(?:start|create|plan)\s+(?:a\s+)?(?:new|another|separate)\s+(?:trip|vacation|holiday|(?:planning\s+)?session)|(?:start|create)\s+(?:a|an)\s+(?:trip|vacation|holiday|(?:planning\s+)?session))\b`)
)

// MockLLM is a deterministic stand-in for Gemini. It classifies replies and basic trip routing
// via regex offline; record_preferences/propose_options have no canned data
// and need a real GeminiLLM (MOCK_LLM=false).
type MockLLM struct {
	Calls []string
}

func (m *MockLLM) Structured(ctx context.Context, system string, messages []Message, schema Schema) (map[string]any, error) {
	m.Calls = append(m.Calls, schema.Name)
	switch schema.Name {
	case "route_trip":
		var content string
		if len(messages) > 0 {
			content, _ = messages[len(messages)-1].Content.(string)
		}
		text := content
		if index := strings.LastIndex(text, "Latest WhatsApp message from "); index >= 0 {
			text = text[index:]
			if index = strings.Index(text, "\n"); index >= 0 {
				text = text[index+1:]
			}
		}
		low := strings.ToLower(strings.TrimSpace(text))
		pending := strings.Contains(content, `"pending_trip_request":{`)
		if pending && (low == "new trip" || low == "a separate one" || low == "another session") {
			return map[string]any{"action": "new_trip", "use_pending_request": true, "question": ""}, nil
		}
		if pending && (low == "continue this trip" || low == "change this one" || low == "replace the current destination") {
			return map[string]any{"action": "continue", "use_pending_request": true, "question": ""}, nil
		}
		if routeNewTripRe.MatchString(text) && !strings.Contains(low, "instead") && !strings.Contains(low, "don't") {
			return map[string]any{"action": "new_trip", "use_pending_request": false, "question": ""}, nil
		}
		if strings.HasPrefix(low, "what about ") {
			return map[string]any{"action": "clarify", "use_pending_request": false, "question": "Should I change the current trip or start a separate trip? Reply 'continue this trip' or 'new trip'."}, nil
		}
		return map[string]any{"action": "continue", "use_pending_request": false, "question": ""}, nil
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

	case "intake_extract":
		var text string
		if len(messages) > 0 {
			if s, ok := messages[len(messages)-1].Content.(string); ok {
				text = s
			}
		}
		intent := "none"
		switch {
		case cancelRe.MatchString(text):
			intent = "cancel"
		case startTripRe.MatchString(text):
			intent = "start"
		}
		approval := "none"
		switch {
		case approveRe.MatchString(text):
			approval = "yes"
		case rejectRe.MatchString(text):
			approval = "no"
		}
		var updates []map[string]any
		if dm := dateRangeRe.FindStringSubmatch(text); dm != nil {
			updates = append(updates, map[string]any{"scope": "trip", "field": "date_window", "value_text": dm[1] + ".." + dm[2], "confidence": "confirmed"})
		}
		if nm := nightsRe.FindStringSubmatch(text); nm != nil {
			updates = append(updates, map[string]any{"scope": "trip", "field": "nights", "value_text": nm[1], "confidence": "confirmed"})
		}
		if includesRe.MatchString(text) {
			updates = append(updates, map[string]any{"scope": "trip", "field": "budget_includes", "value_text": "flights,stay", "confidence": "confirmed"})
		}
		if noneConstrRe.MatchString(text) {
			updates = append(updates, map[string]any{"scope": "trip", "field": "constraints", "value_text": "none", "confidence": "confirmed"})
		}
		return map[string]any{
			"trip_intent": intent, "updates": updates,
			"answers_pending_question": len(updates) > 0 || approval != "none",
			"approval":                 approval,
		}, nil

	case "intake_writer":
		var slot string
		if len(messages) > 0 {
			if s, ok := messages[len(messages)-1].Content.(string); ok {
				slot = s
			}
		}
		return map[string]any{"text": "(mock) " + slot}, nil

	case "destination_suggestions":
		return map[string]any{"cities": []string{"Lisbon", "Mexico City", "Costa Rica"}}, nil

	default:
		return nil, fmt.Errorf("MockLLM has no canned answer for %s", schema.Name)
	}
}

func (m *MockLLM) Agent(ctx context.Context, system string, messages []Message, tools []Tool, handlers map[string]Handler) (string, error) {
	m.Calls = append(m.Calls, "agent")
	return "(mock) Good question! I'll have a real answer once MOCK_LLM=false.", nil
}
