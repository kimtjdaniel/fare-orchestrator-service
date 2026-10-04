package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"fare-brain/llm"
	"fare-brain/models"
	"fare-brain/prompts"
	"fare-brain/store"
)

const chatTurnSystem = `You are Fare, a travel-planning assistant in a WhatsApp group.

Your job is to understand the latest message using the current trip context and return a structured decision.

Rules:
- Extract new or changed trip information from the latest message.
- Never invent trip details.
- Preserve existing information unless the user changes it.
- Track who is traveling. Do not assume every group member is going.
- Determine party size from known travelers or an explicit number. If it is unknown and needed, ask how many people are going.
- "I'm going" means the person is joining; it does not mean they are available for every proposed date.
- If required information is missing, ask one short, natural question.
- If enough information exists, choose the appropriate action.
- Keep replies short and conversational.
- Don't repeat answered questions. @mention travelers whose answers are missing before accepting an explicit decision to proceed without their answers.
- "No budget" means no spending cap. Optional preferences should not delay searches or sharing an available trip link.

Important trip information:
- destination
- dates
- origin
- travelers / party size
- budget and preferences when provided

Possible actions:
- reply
- new_trip
- clarify_trip
- plan
- choose
- search
- cancel
- dashboard
- itinerary
- restaurants
- hotel_map
- retry_flights
- skip_flights
- undo_activity
- edit_activity
- replace_activity
- replacement_reply
- ignore

Return only the structured response matching the provided schema.

The server validates and executes your decision. Do not claim an action succeeded unless the context says it did.`

func chatPlanningSchema() map[string]any {
	schema := make(map[string]any)
	for key, value := range prompts.PlanTrip["schema"].(map[string]any) {
		schema[key] = value
	}
	schema["description"] = "Complete current planning inputs for plan/new_trip, or search/dashboard while collecting. For a new trip, use the new request's facts."
	return schema
}

type chatDecision struct {
	Action            string         `json:"action"`
	Reply             string         `json:"reply"`
	OptionNumber      int            `json:"option_number"`
	UsePendingRequest bool           `json:"use_pending_request"`
	DeferSearch       bool           `json:"defer_search"`
	Planning          map[string]any `json:"planning"`
}

func (b *Brain) conversationFacts(ctx context.Context, trip *models.Trip) map[string]any {
	facts := tripFacts(trip)
	facts["today"] = b.today().Format("2006-01-02")
	facts["timezone"] = b.Config.Timezone
	facts["session_id"] = trip.SessionID
	facts["options"] = trip.Options
	facts["has_saved_stays"] = len(pendingHotels(trip)) > 0
	facts["chosen_option_position"] = trip.ChosenOptionPosition
	facts["preferences"] = formatKnownPrefs(trip.Participants)
	facts["roster"] = trip.Roster
	facts["intake"] = trip.Intake
	facts["planning_readiness"] = trip.PlanningReadiness
	facts["pending_question"] = trip.PendingQuestion
	facts["pending_trip_request"] = trip.PendingTripRequest
	facts["pending_change"] = trip.PendingChange
	facts["last_poll"] = trip.LastPoll
	facts["dashboard_url"] = ""
	if trip.State == models.Searching || trip.State == models.AwaitingApproval || trip.State == models.Booked || trip.State == models.BookingState {
		facts["dashboard_url"] = b.tripPageURL(ctx, trip, trip.SessionID)
	}
	if trip.Itinerary != nil {
		facts["advisor"] = trip.Itinerary["advisor"]
		facts["pending_activity_replacement"] = trip.Itinerary["pending_activity_replacement"]
	}
	return facts
}

func (b *Brain) handleChatTurn(ctx context.Context, trip *models.Trip, m models.IncomingMessage, sentAt time.Time) error {
	input, err := json.Marshal(map[string]any{"current_trip": b.conversationFacts(ctx, trip), "latest_message": m})
	if err != nil {
		return err
	}
	out, err := b.structured(ctx, trip, chatTurnSystem, []llm.Message{{Role: "user", Content: string(input)}}, llm.Schema{
		Name: "chat_turn", Schema: map[string]any{
			"type": "object", "properties": map[string]any{
				"action":              map[string]any{"type": "string", "enum": []string{"reply", "ignore", "new_trip", "clarify_trip", "plan", "choose", "search", "cancel", "dashboard", "itinerary", "restaurants", "hotel_map", "retry_flights", "skip_flights", "undo_activity", "edit_activity", "replace_activity", "replacement_reply"}},
				"reply":               map[string]any{"type": "string", "description": "Response for reply or clarify_trip; otherwise empty."},
				"option_number":       map[string]any{"type": "integer", "description": "Selected existing option number, or 0."},
				"use_pending_request": map[string]any{"type": "boolean", "description": "True when resolving the stored pending trip request."},
				"defer_search":        map[string]any{"type": "boolean", "description": "True only when the user asks to wait or wants a draft without searching."},
				"planning":            chatPlanningSchema(),
			}, "required": []string{"action", "reply", "option_number", "use_pending_request", "defer_search"}, "additionalProperties": false,
		},
	})
	if err != nil {
		return err
	}
	var decision chatDecision
	if err := decodeInto(out, &decision); err != nil {
		return err
	}
	ctx = context.WithValue(ctx, deferSearchKey{}, decision.DeferSearch)
	if decision.Action == "reply" || decision.Action == "clarify_trip" {
		if strings.TrimSpace(decision.Reply) == "" {
			return fmt.Errorf("conversation decision returned an empty reply")
		}
	}
	if decision.Action == "clarify_trip" {
		pending := &models.PendingTripRequest{Text: m.Text, SenderName: m.SenderName, SentAt: sentAt, Question: decision.Reply, MessageIDs: incomingMessageIDs(ctx)}
		if trip.PendingTripRequest != nil {
			copy := *trip.PendingTripRequest
			copy.Question = decision.Reply
			copy.MessageIDs = append(append([]string(nil), copy.MessageIDs...), incomingMessageIDs(ctx)...)
			pending = &copy
		}
		if _, err := b.Store.UpdateTrip(ctx, trip.ID, map[string]any{"pending_trip_request": pending}); err != nil {
			return err
		}
		return b.sayReply(ctx, trip.GroupID, decision.Reply)
	}
	if pending := trip.PendingTripRequest; pending != nil && decision.UsePendingRequest {
		if decision.Action != "new_trip" && decision.Action != "plan" {
			return b.say(ctx, trip.GroupID, "Session routing is unresolved. Ask which trip the original request applies to; no action has been taken.", nil)
		}
		ids := append(append([]string(nil), pending.MessageIDs...), incomingMessageIDs(ctx)...)
		ctx = context.WithValue(ctx, incomingMessageIDsKey{}, ids)
		m.Text = fmt.Sprintf("Original request from %s: %s\nClarification from %s: %s", pending.SenderName, pending.Text, m.SenderName, m.Text)
		sentAt = pending.SentAt
		trip, err = b.Store.UpdateTrip(ctx, trip.ID, map[string]any{"pending_trip_request": nil})
		if err != nil {
			return err
		}
	} else if pending != nil && (decision.Action == "search" || decision.Action == "choose" || decision.Action == "cancel") {
		return b.say(ctx, trip.GroupID, "Resolve the pending trip question before changing the current plan: "+pending.Question, nil)
	}
	switch decision.Action {
	case "ignore":
		return nil
	case "reply":
		return b.sayReply(ctx, trip.GroupID, decision.Reply)
	case "new_trip":
		// Session creation does not require the model to supply a complete plan.
		// If it omitted planning, collect known facts in the new session instead.
		// The first mention must retain the background chat just adopted into
		// this empty session instead of resetting it out of the model's context.
		if trip.State == models.Collecting && len(trip.Participants) == 0 && len(trip.Options) == 0 && trip.Destination == "" {
			if decision.Planning == nil {
				return b.plan(ctx, trip, m.Text, m)
			}
			return b.applyChatPlan(ctx, trip, decision.Planning)
		}
		return b.startNewTripWithPlan(ctx, trip, m, sentAt, decision.Planning)
	case "plan":
		if trip.State == models.Searching || trip.State == models.BookingState {
			return b.sayExact(ctx, trip.GroupID, "Your current session is searching. This change hasn't been applied. Follow the session here:\n"+b.tripPageURL(ctx, trip, trip.SessionID), nil)
		}
		if trip.State == models.Booked || trip.State == models.Cancelled {
			trip, err = store.SetState(ctx, b.Store, trip.ID, models.Collecting, nil)

		}
		if err != nil {
			return err
		}
		return b.applyChatPlan(ctx, trip, decision.Planning)
	case "choose":
		if trip.State != models.AwaitingChoice || !hasDestinationOption(trip, decision.OptionNumber) {
			return b.say(ctx, trip.GroupID, "The requested destination option is not selectable in the current state. Ask for a current option without changing anything.", nil)
		}
		return b.selectOption(ctx, trip, decision.OptionNumber)
	case "search":
		if trip.State == models.Collecting {
			return b.applyChatPlan(ctx, trip, decision.Planning)
		}
		return b.startReadySearch(ctx, trip, decision.OptionNumber)
	case "cancel":
		if trip.State == models.Booked || trip.State == models.BookingState {
			return b.say(ctx, trip.GroupID, "This application cannot cancel external bookings. No booking was changed.", nil)
		}
		if trip.State != models.Cancelled {
			if _, err := store.SetState(ctx, b.Store, trip.ID, models.Cancelled, nil); err != nil {
				return err
			}
		}
		return b.say(ctx, trip.GroupID, "Trip planning was stopped. No external reservation was cancelled.", nil)
	case "dashboard":
		if trip.State == models.AwaitingChoice {
			return b.startReadySearch(ctx, trip, decision.OptionNumber)
		}
		if trip.State == models.Collecting {
			return b.applyChatPlan(ctx, trip, decision.Planning)
		}
		if trip.Itinerary != nil && trip.Itinerary["dashboard_plan"] != nil {
			return b.shareItinerary(ctx, trip)
		}
		return b.shareLiveSearch(ctx, trip, trip.SessionID)
	case "itinerary":
		return b.writeAdvisorItinerary(ctx, trip, m)
	case "restaurants":
		return b.writeRestaurantPlan(ctx, trip, m)
	case "hotel_map":
		return b.sendHotelMap(ctx, trip)
	case "retry_flights":
		option := trip.ChosenOption()
		origin := originAirportCode(trip.Origin, trip.Participants)
		if trip.State != models.Searching || len(pendingHotels(trip)) == 0 || option == nil ||
			!validIATA(origin) || !validIATA(option.DestinationAirport) || !isoDateOK(option.EmbarkingDate) || !isoDateOK(option.ReturningDate) {
			return b.say(ctx, trip.GroupID, "No flight retry started. A retry requires an unfinished search with saved stays and valid airports/dates. Explain the missing prerequisite from the current facts.", nil)
		}
		if err := b.say(ctx, trip.GroupID, "The requested flight retry is starting with the same dates and airports. Saved stays will be reused.", nil); err != nil {
			return err
		}
		if err := b.flushReply(ctx, trip.GroupID); err != nil {
			return err
		}
		b.runFlightSearch(ctx, trip, origin, option.DestinationAirport, option.EmbarkingDate, option.ReturningDate)
		return b.say(ctx, trip.GroupID, "The flight retry has returned. Report only the currently saved fare result; if no fares were saved, explain that the stay is still available and no flight is confirmed.", nil)
	case "skip_flights":
		if len(pendingHotels(trip)) == 0 || trip.ChosenOption() == nil || (trip.State != models.Searching && trip.State != models.AwaitingChoice) {
			return b.say(ctx, trip.GroupID, "The request to continue without flights was not applied: there is no unfinished plan with a saved stay to continue from.", nil)
		}
		b.continueWithoutFlights(ctx, trip.GroupID)
		return nil
	case "undo_activity":
		_, note, err := b.activityAction(ctx, trip, map[string]any{"action": "undo_activity_edit", "expected_revision": trip.ItineraryRevision})
		if err != nil {
			if e, ok := err.(*DashboardError); ok {
				return b.say(ctx, trip.GroupID, e.Msg, nil)
			}
			return err
		}
		return b.say(ctx, trip.GroupID, note, nil)
	case "edit_activity":
		return b.editActivityMessage(ctx, trip, m)
	case "replace_activity":
		return b.activityReplacementMessage(ctx, trip, m, m.Text)
	case "replacement_reply":
		m.Tagged = true // semantic routing already identified a reply to this suggestion
		if handled, err := b.handleActivityReplacementReply(ctx, trip, m, m.Text); err != nil || handled {
			return err
		}
		return b.say(ctx, trip.GroupID, "No current activity suggestion could be matched to this reply. Ask which activity they mean.", nil)
	default:
		return fmt.Errorf("unsupported conversation action %q", decision.Action)
	}
}

// The chat decision already extracted the requirements; never ask Gemini to
// repeat that work before handing the session to the search services.
func (b *Brain) applyChatPlan(ctx context.Context, trip *models.Trip, planned map[string]any) error {
	if planned == nil {
		return fmt.Errorf("planning action returned no trip inputs")
	}
	return b.applyPlan(ctx, trip, planned)
}

func hasDestinationOption(trip *models.Trip, number int) bool {
	for _, option := range trip.Options {
		if option.Position == number {
			return true
		}
	}
	return false
}

// This flag belongs to the addressed request, never to background chat.
type deferSearchKey struct{}

func searchDeferred(ctx context.Context) bool {
	deferred, _ := ctx.Value(deferSearchKey{}).(bool)
	return deferred
}

func (b *Brain) startReadySearch(ctx context.Context, trip *models.Trip, number int) error {
	if searchDeferred(ctx) {
		return b.say(ctx, trip.GroupID, "Searching is deferred at the user's request. Describe the saved draft without claiming a live search exists.", nil)
	}
	if trip.State != models.AwaitingChoice {
		return b.say(ctx, trip.GroupID, "No new search started. Describe the current search or completed itinerary and share its existing session link if available.", nil)
	}
	if number == 0 && trip.ChosenOption() == nil && len(trip.Options) == 1 {
		number = trip.Options[0].Position
	}
	if number > 0 {
		if !hasDestinationOption(trip, number) {
			return b.say(ctx, trip.GroupID, "No search started because that option is not in the current plan. Ask which current destination option to use.", nil)
		}
		var err error
		trip, err = b.lockChosenOption(ctx, trip, number)
		if err != nil {
			return err
		}
	}
	if trip.ChosenOption() == nil {
		return b.say(ctx, trip.GroupID, "The destinations are still alternatives. Ask which option to search before sharing a live session link.", nil)
	}
	return b.startTravelSearch(ctx, trip)
}

// Prefer the model's focused follow-up over restarting the group questionnaire.
func planningReadinessQuestion(trip *models.Trip, questions ...string) string {
	if len(trip.Participants) > 0 && trip.PlanningReadiness.AttendanceConfirmed && trip.PlanningReadiness.AvailabilityConfirmed {
		return ""
	}
	for _, question := range questions {
		if strings.TrimSpace(question) != "" {
			return question
		}
	}
	if len(trip.Participants) == 0 || !trip.PlanningReadiness.AttendanceConfirmed {
		return "Who's joining us?"
	}
	var waiting []string
	for _, person := range trip.Participants {
		if len(person.GeneralPreferences.Availability) == 0 && person.WhatsAppName != "" {
			waiting = append(waiting, "@"+person.WhatsAppName)
		}
	}
	if len(waiting) > 0 {
		return strings.Join(waiting, ", ") + ", what dates work for you?"
	}
	return "Which dates can we agree on for this trip?"
}
