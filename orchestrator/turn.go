package orchestrator

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"fare-brain/formatting"
	"fare-brain/llm"
	"fare-brain/models"
	"fare-brain/prompts"
	"fare-brain/store"
)

// dispatchTurn asks Gemini what this message is, then runs that action.
// Regex routing runs only when this call fails or returns no action.
func (b *Brain) dispatchTurn(ctx context.Context, trip *models.Trip, m models.IncomingMessage, sentAt time.Time) (bool, error) {
	if trip == nil || !shouldClassify(trip, m) {
		return false, nil
	}
	pending := ""
	if trip.PendingQuestion != nil {
		pending = trip.PendingQuestion.Field
	}
	history, _ := b.Store.GetMessages(ctx, trip.GroupID, trip.HistoryStart, 12, true)
	facts, _ := json.Marshal(map[string]any{
		"state": trip.State, "origin": trip.Origin, "destination": tripDestination(trip),
		"start": trip.EmbarkingDate, "end": trip.ReturningDate, "pending": pending,
	})
	user := "Trip:\n" + string(facts) + "\n\nRecent chat:\n" + compactChat(history, 8) +
		"\n\nLatest message from " + m.SenderName + ":\n" + m.Text
	out, err := b.LLM.Structured(ctx, prompts.TurnSystem(string(trip.State), pending, tripDestination(trip)),
		[]llm.Message{{Role: "user", Content: user}}, toSchema(prompts.TurnRoute))
	if err != nil || out == nil {
		slog.Warn("turn route failed, using fallback", "group_id", trip.GroupID, "err", err)
		return false, nil
	}
	action, _ := out["action"].(string)
	action = strings.TrimSpace(action)
	if action == "" || action == "ignore" {
		if m.Tagged || trip.PendingQuestion != nil {
			return false, nil
		}
		return action == "ignore", nil
	}
	slog.Info("turn route", "group_id", trip.GroupID, "action", action, "text", clipLog(m.Text, 80))
	switch action {
	case "intro":
		return true, b.say(ctx, trip.GroupID, formatting.IntroMessage(b.Config.BotName), nil)
	case "recap":
		return true, b.replayPlan(ctx, trip, m)
	case "itinerary":
		return true, b.writeAdvisorItinerary(ctx, trip, m)
	case "restaurants":
		return true, b.writeRestaurantPlan(ctx, trip, m)
	case "search", "book":
		return true, b.handleBookAsk(ctx, trip, m)
	case "cancel":
		if !looksLikeCancelBooking(m.Text) {
			return true, b.answerQuestion(ctx, trip, m)
		}
		if trip.State == models.Booked {
			return true, b.reopenAndPlan(ctx, trip, m)
		}
		if _, err := store.SetState(ctx, b.Store, trip.ID, models.Cancelled, nil); err != nil {
			return true, err
		}
		return true, b.say(ctx, trip.GroupID, "Okay, dropping this trip. Ping me if you want to start over.", nil)
	case "change":
		dest, _ := out["destination"].(string)
		dest = strings.TrimSpace(dest)
		if dest == "" || samePlace(dest, tripDestination(trip)) || trip.State == models.Collecting {
			return true, b.runIntakeTurn(ctx, trip, m)
		}
		old := tripDestination(trip)
		if old == "" {
			old = "the current trip"
		}
		return true, b.startChangePoll(ctx, trip, m, &models.PendingChange{
			Kind:        "reopen",
			Summary:     "Cancel " + old + " and plan " + dest + " instead?",
			Destination: dest,
		})
	case "new_trip":
		return true, b.startNewTrip(ctx, trip, m, sentAt)
	case "intake":
		return true, b.runIntakeTurn(ctx, trip, m)
	case "answer":
		return true, b.answerQuestion(ctx, trip, m)
	default:
		return false, nil
	}
}

func shouldClassify(trip *models.Trip, m models.IncomingMessage) bool {
	if trip == nil {
		return false
	}
	if m.Tagged || trip.PendingQuestion != nil || looksLikeDirectQuestion(m.Text) {
		return true
	}
	return false
}
