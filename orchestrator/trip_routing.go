package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"fare-brain/llm"
	"fare-brain/models"
	"fare-brain/prompts"
)

const tripRoutingQuestion = "Should I change the current trip or start a separate trip? Reply 'continue this trip' or 'new trip'."

// These qualifiers make a planning command refer to the active conversation,
// so Gemini should interpret it rather than starting a session automatically.
var tripContinuationRe = regexp.MustCompile(`(?i)\b(?:instead|itinerary|day[- ]by[- ]day|don't|do not|rather than|not (?:a |another |a new )?trip|(?:this|that|the current|the existing|our current|the same)\s+(?:trip|plan|session))\b`)

func isStandaloneNewTripRequest(text string) bool {
	text = strings.TrimSpace(text)
	if strings.HasPrefix(strings.ToLower(text), "please ") {
		text = strings.TrimSpace(text[len("please "):])
	}
	match := newTripAskRe.FindStringIndex(text)
	return match != nil && match[0] == 0 && !tripContinuationRe.MatchString(text)
}

// Routing happens before preference extraction or booking. Gemini chooses the
// conversation's intent; code still owns session creation and all state changes.
func (b *Brain) routeTripMessage(ctx context.Context, trip *models.Trip, m models.IncomingMessage, sentAt time.Time) (*models.Trip, models.IncomingMessage, bool, error) {
	pending := trip.PendingTripRequest
	if !m.Tagged && !looksLikeDirectQuestion(m.Text) && pending == nil {
		return trip, m, false, nil
	}
	text := strings.TrimSpace(b.mentionRe.ReplaceAllString(m.Text, ""))

	if pending == nil && looksLikeTripActivityEdit(trip, text) {
		return trip, m, false, nil
	}
	obviousReply := choiceOnlyRe.MatchString(text) || approveOnlyRe.MatchString(text) || rejectOnlyRe.MatchString(text)
	if pending == nil && quotedText(m) == "" && (obviousReply || looksLikeOnlyGreeting(text) || looksLikeIntroAsk(text) || looksLikeDashboardAsk(text)) {
		return trip, m, false, nil
	}
	// A yes/no or numbered answer cannot choose between two routing alternatives.
	// In particular it must not accidentally approve booking the old trip.
	if pending != nil && obviousReply {
		return trip, m, true, b.say(ctx, m.GroupID, pending.Question, nil)
	}
	// A standalone planning command is an explicit new request even when its
	// destination and dates match the active trip. Do not let a model mistake it
	// for a search or approval of the previous plan.
	if m.Tagged && quotedText(m) == "" && isStandaloneNewTripRequest(text) {
		return trip, m, true, b.startNewTrip(ctx, trip, m, sentAt)
	}
	if looksLikePlanRecap(text) || (looksLikeAdvisorAsk(text) && !looksLikeNewTripRequest(text) && !looksLikeCancelBooking(text) && !looksLikeReplan(text)) {
		return trip, m, false, nil
	}

	history, err := b.Store.GetMessages(ctx, m.GroupID, trip.HistoryStart, 20, true)
	if err != nil {
		return trip, m, true, err
	}
	facts, err := json.Marshal(map[string]any{
		"state": trip.State, "origin": trip.Origin, "destination": trip.Destination,
		"start_date": trip.EmbarkingDate, "end_date": trip.ReturningDate,
		"options": trip.Options, "budget_note": trip.BudgetNote,
		"preferences": formatKnownPrefs(trip.Participants), "last_poll": trip.LastPoll,
		"pending_change": trip.PendingChange, "pending_trip_request": pending,
	})
	if err != nil {
		return trip, m, true, err
	}
	user := fmt.Sprintf("Current trip: %s\nRecent chat (background only):\n%s\nQuoted message: %s\nLatest WhatsApp message from %s:\n%s",
		facts, compactChat(history, 20), quotedText(m), m.SenderName, m.Text)
	out, classifyErr := b.LLM.Structured(ctx, prompts.RouteTripSystem,
		[]llm.Message{{Role: "user", Content: user}}, toSchema(prompts.RouteTrip))
	// If classification fails or returns an unknown action, ask instead of resetting
	// a trip or passing a potential new request into the existing booking flow.
	action, _ := out["action"].(string)
	usePending, _ := out["use_pending_request"].(bool)
	question, _ := out["question"].(string)
	if classifyErr != nil || (action != "new_trip" && action != "continue" && action != "clarify") {
		action = "clarify"
		question = tripRoutingQuestion
	}
	if action == "clarify" {
		if strings.TrimSpace(question) == "" {
			question = tripRoutingQuestion
		}
		request := &models.PendingTripRequest{Text: m.Text, SenderName: m.SenderName, SentAt: sentAt, Question: question}
		if pending != nil {
			copy := *pending
			copy.Question = question
			request = &copy
		}
		trip, err = b.Store.UpdateTrip(ctx, trip.ID, map[string]any{"pending_trip_request": request})
		if err != nil {
			return trip, m, true, err
		}
		return trip, m, true, b.say(ctx, m.GroupID, question, nil)
	}
	if pending != nil && usePending {
		m.Text = fmt.Sprintf("Original request from %s: %s\nClarification from %s: %s", pending.SenderName, pending.Text, m.SenderName, m.Text)
		m.Tagged = true
		sentAt = pending.SentAt
		trip, err = b.Store.UpdateTrip(ctx, trip.ID, map[string]any{"pending_trip_request": nil})
		if err != nil {
			return trip, m, true, err
		}
	}
	if action == "new_trip" {
		if looksLikePlanRecap(text) || (looksLikeAdvisorAsk(text) && !looksLikeNewTripRequest(text)) {
			return trip, m, false, nil
		}
		return trip, m, true, b.startNewTrip(ctx, trip, m, sentAt)
	}
	return trip, m, false, nil
}
