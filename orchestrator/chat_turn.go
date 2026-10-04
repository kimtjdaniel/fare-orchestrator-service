package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"fare-brain/llm"
	"fare-brain/models"
	"fare-brain/store"
)

const chatTurnSystem = `You are Fare, a conversational group travel planner. Decide the next action from the latest message, actual trip facts, recent turns and earlier memory. Return one structured decision. The server validates and executes it; you cannot claim an action already happened.

Conversation rules:
- Understand intent, negation, corrections and pronouns. A word like "book", "hotel", "yes" or a number by itself is not enough to choose an action. Resolve short replies against the most recent relevant question or quoted message. If two interpretations remain, ask one specific question.
- Retain each person's preferences and constraints. Do not ask for known information again. Do not launch into an introduction or a questionnaire when someone asked a question.
- Only mentions and direct replies to the bot reach this decision step. Untagged group chat is background context: use its preferences and corrections when answering the current addressed request, but never treat an older untagged message as a fresh command or approval. Tagged greetings deserve a brief natural reply.
- An initial planning request can use the empty current session. Start a new session only for an explicit separate/new trip or a standalone request to plan a trip; amendments, hypotheticals and follow-ups stay in the same session. Do not reset a trip just because another city was mentioned.
- A pending_trip_request concerns whether to start another session. Resolve it with use_pending_request only when the latest message unambiguously answers it. A bare yes cannot choose between two alternatives. Use clarify_trip for unresolved session ambiguity; use reply for other clarification.
- Prices and availability come only from saved search results. Options are proposals, not actual fares. Planning and searching do not make a reservation. This application does not purchase travel or cancel external bookings.

Actions:
reply: answer a question, greeting or clarification in reply, grounded in supplied facts. Ask at most one missing question. Do not promise a write or search with this action.
ignore: no response or changes.
new_trip: explicitly start a separate planning session.
clarify_trip: ask in reply whether the original request changes this trip or starts a separate one.
plan: record preferences or revise the plan, including destination/date/origin/budget corrections and answers to planning questions. Do not use for a question about the existing plan.
choose: select a currently offered destination by its exact option_number. A short number can select only a destination option that was actually just offered, not a day or poll of another kind.
search: the user explicitly requests flight/stay search or unambiguously agrees to the current search/finalize question. Never for "yes" to a map, suggestion, budget question, or hypothetical booking question. Set option_number if selecting an existing option too. If still COLLECTING, use plan to extract the known preferences and produce a complete option first. A complete single-option plan or a destination choice starts searches automatically. Use search for an existing ready plan in AWAITING_CHOICE.
cancel: explicitly stop planning the current trip. Never claim an external reservation was cancelled.
dashboard: share an existing active/completed session; if the plan is ready but not started, launch its flight/stay searches before sharing. If multiple destinations remain, ask for the choice first.
itinerary: create or update the saved day-by-day itinerary at the user's request.
restaurants: suggest dining for this trip.
hotel_map: send the selected hotel's map only when that is requested.
retry_flights: explicitly retry failed/missing flights when the stay search is saved.
skip_flights: explicitly continue with the saved stays without searching flights.
undo_activity: undo the most recent saved activity edit.
edit_activity: apply a requested specific edit to the saved itinerary.
replace_activity: propose an alternative to an activity, without accepting it.
replacement_reply: respond to the current pending activity suggestion; this is separate from search approval.

Set defer_search=true only when the current request explicitly asks for a draft, options only, or to wait/not search. Otherwise set it false: complete single-option planning requests and destination choices start flight/stay searches without a second confirmation. Never use reply to announce that you are starting a session/search; choose the executable action.

For actions other than reply or clarify_trip leave reply empty. Do not create canned responses. Match the user's language and tone, be concise, and never repeat a question already answered. option_number is 0 when irrelevant. use_pending_request is false unless resolving the stored request.`

type chatDecision struct {
	Action            string `json:"action"`
	Reply             string `json:"reply"`
	OptionNumber      int    `json:"option_number"`
	UsePendingRequest bool   `json:"use_pending_request"`
	DeferSearch       bool   `json:"defer_search"`
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
				"action": map[string]any{"type": "string", "enum": []string{"reply", "ignore", "new_trip", "clarify_trip", "plan", "choose", "search", "cancel", "dashboard", "itinerary", "restaurants", "hotel_map", "retry_flights", "skip_flights", "undo_activity", "edit_activity", "replace_activity", "replacement_reply"}},
				"reply":  map[string]any{"type": "string"}, "option_number": map[string]any{"type": "integer"},
				"use_pending_request": map[string]any{"type": "boolean"},
				"defer_search":        map[string]any{"type": "boolean"},
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
		// The first mention must retain the background chat just adopted into
		// this empty session instead of resetting it out of the model's context.
		if trip.State == models.Collecting && len(trip.Participants) == 0 && len(trip.Options) == 0 && trip.Destination == "" {
			return b.plan(ctx, trip, m.Text, m)
		}
		return b.startNewTrip(ctx, trip, m, sentAt)
	case "plan":
		if trip.State == models.Searching || trip.State == models.BookingState {
			return b.say(ctx, trip.GroupID, "A search is in progress. The requested change has not been applied. Explain the current state and ask them to wait for this search or explicitly start a separate trip.", nil)
		}
		if trip.State == models.Booked || trip.State == models.Cancelled {
			trip, err = store.SetState(ctx, b.Store, trip.ID, models.Collecting, nil)

		}
		if err != nil {
			return err
		}
		return b.plan(ctx, trip, m.Text, m)
	case "choose":
		if trip.State != models.AwaitingChoice || !hasDestinationOption(trip, decision.OptionNumber) {
			return b.say(ctx, trip.GroupID, "The requested destination option is not selectable in the current state. Ask for a current option without changing anything.", nil)
		}
		return b.selectOption(ctx, trip, decision.OptionNumber)
	case "search":
		if trip.State == models.Collecting {
			return b.plan(ctx, trip, m.Text, m)
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
			return b.plan(ctx, trip, m.Text, m)
		}
		return b.say(ctx, trip.GroupID, "Share the existing active or completed session link and accurately describe its current state. Do not start another search.", nil)
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
