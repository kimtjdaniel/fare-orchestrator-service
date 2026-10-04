package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf16"

	"fare-brain/llm"
	"fare-brain/models"
	"fare-brain/prompts"

	"github.com/google/uuid"
)

var activityReplacementRe = regexp.MustCompile(`(?i)\b(replace|swap)\b|\b(suggest|find)\b.*\b(alternative|replacement)\b|\b(too expensive|cheaper|quieter|less crowded)\b`)
var activityTargetRe = regexp.MustCompile(`(?i)\b(day\s*\d+|activity|activities|dinner|lunch|breakfast|tour|museum|walk|visit)\b`)
var activityReplacementReplyRe = regexp.MustCompile(`(?i)^(accept|reject) replacement ([a-f0-9]{8})[.!]?$`)
var activityReplacementReplyHintRe = regexp.MustCompile(`(?i)\b(yes|yeah|yep|sure|okay|ok|cool|no|nah|nope|use|keep|accept|reject|another|else|instead|cheaper|quieter)\b|sounds good|that works|love it|let'?s do`)

func looksLikeActivityReplacement(trip *models.Trip, text string) bool {
	if trip == nil || looksLikeNewTripRequest(text) || looksLikeCancelBooking(text) || !activityReplacementRe.MatchString(text) {
		return false
	}
	if activityTargetRe.MatchString(text) {
		return true
	}
	itinerary := models.ScheduleItinerary(trip, trip.Itinerary, false)
	for _, raw := range sliceAny(asMapAny(itinerary["advisor"])["days"]) {
		for _, item := range sliceAny(asMapAny(raw)["activities"]) {
			title := strings.ToLower(strAny(asMapAny(item)["title"]))
			if title != "" && strings.Contains(strings.ToLower(text), title) {
				return true
			}
		}
	}
	return false
}

func (b *Brain) activityReplacementAction(ctx context.Context, trip *models.Trip, body map[string]any) (*models.Trip, string, error) {
	if err := b.validateActivityRequest(ctx, trip, body); err != nil {
		return nil, "", err
	}
	itinerary := models.ScheduleItinerary(trip, trip.Itinerary, false)
	if strAny(body["action"]) == "propose_activity_replacement" {
		return b.proposeActivityReplacement(ctx, trip, itinerary, body)
	}
	proposal := asMapAny(itinerary["pending_activity_replacement"])
	if proposal == nil || strAny(body["proposal_id"]) == "" || strAny(body["proposal_id"]) != strAny(proposal["id"]) {
		return nil, "", &DashboardError{Status: 409, Msg: "that suggestion is no longer current; ask for a new replacement"}
	}
	if strAny(body["action"]) == "reject_activity_replacement" {
		delete(itinerary, "pending_activity_replacement")
		return b.saveActivityItinerary(ctx, trip, itinerary, "Kept the original activity and dismissed the suggestion.")
	}
	// Accept only the stored suggestion, never replacement text supplied by the client.
	patch := map[string]any{"action": "update_activity", "expected_revision": trip.ItineraryRevision, "activity_id": proposal["activityId"], "time": proposal["time"], "title": proposal["title"], "description": proposal["description"]}
	return b.activityAction(ctx, trip, patch)
}

func (b *Brain) proposeActivityReplacement(ctx context.Context, trip *models.Trip, itinerary map[string]any, body map[string]any) (*models.Trip, string, error) {
	request := strAny(body["request"])
	if request == "" || len(utf16.Encode([]rune(request))) > 500 {
		return nil, "", &DashboardError{Status: 400, Msg: "describe the replacement in 1–500 characters"}
	}
	targetID := strAny(body["activity_id"])
	if targetID != "" {
		if activity, _ := findScheduleActivity(itinerary, targetID); activity == nil {
			return nil, "", &DashboardError{Status: 404, Msg: "activity not found"}
		}
	}
	inputData := map[string]any{
		"request": request, "target_activity_id": targetID,
		"destination": tripDestination(trip), "budget_note": trip.BudgetNote,
		"preferences":          formatKnownPrefs(trip.Participants),
		"activity_preferences": trip.ActivityDescription, "culinary_preferences": trip.CulinaryDescription,
		"days": asMapAny(itinerary["advisor"])["days"],
	}
	if previous := asMapAny(itinerary["pending_activity_replacement"]); previous != nil && targetID == strAny(previous["activityId"]) {
		inputData["previous_suggestion"] = previous
	}
	if trip.Intake != nil {
		inputData["group_constraints"] = trip.Intake.Constraints
		inputData["group_vibe"] = trip.Intake.Vibe
	}
	input, err := json.Marshal(inputData)
	if err != nil {
		return nil, "", err
	}
	out, err := b.LLM.Structured(ctx, prompts.ActivityReplacementSystem, []llm.Message{{Role: "user", Content: string(input)}}, toSchema(prompts.ActivityReplacement))
	if err != nil {
		return nil, "", &DashboardError{Status: 400, Msg: "I couldn’t suggest a replacement. Try again with the day, activity and what you want instead."}
	}
	id := strAny(out["activity_id"])
	if id == "" {
		return nil, "", &DashboardError{Status: 400, Msg: orStr(strAny(out["clarification"]), "Which activity should I replace? Repeat the request with the day and activity.")}
	}
	activity, _ := findScheduleActivity(itinerary, id)
	if activity == nil || (targetID != "" && id != targetID) {
		return nil, "", &DashboardError{Status: 400, Msg: "The suggestion did not match that activity. Please try again."}
	}
	title, err := activityText("title", out["title"])
	if err != nil {
		return nil, "", err
	}
	description, err := activityText("description", out["description"])
	if err != nil {
		return nil, "", err
	}
	reason := strAny(out["reason"])
	if reason == "" || len(utf16.Encode([]rune(reason))) > 500 || (title == strAny(activity["title"]) && description == strAny(activity["description"])) {
		return nil, "", &DashboardError{Status: 400, Msg: "I couldn’t suggest a different activity. Try a more specific request."}
	}
	if previous := asMapAny(inputData["previous_suggestion"]); previous != nil && title == strAny(previous["title"]) && description == strAny(previous["description"]) {
		return nil, "", &DashboardError{Status: 400, Msg: "That would repeat the last suggestion. What would you like to be different?"}
	}
	proposalID := uuid.NewString()[:8]
	message := fmt.Sprintf("Suggested replacement for %s at %s:\n%s\n%s\nWhy: %s\nUnverified suggestion; prices and availability aren’t confirmed.\nWant to use this, keep the original, or try another option? You can reply naturally.", strAny(activity["title"]), strAny(activity["time"]), title, description, reason)
	itinerary["pending_activity_replacement"] = map[string]any{"id": proposalID, "activityId": id, "time": activity["time"], "title": title, "description": description, "reason": reason, "request": request, "message": message, "revision": trip.ItineraryRevision + 1}
	return b.saveActivityItinerary(ctx, trip, itinerary, message)
}

func (b *Brain) handleActivityReplacementReply(ctx context.Context, trip *models.Trip, m models.IncomingMessage, text string) (bool, error) {
	itinerary := models.ScheduleItinerary(trip, trip.Itinerary, false)
	proposal := asMapAny(itinerary["pending_activity_replacement"])
	quoted := quotedText(m)
	if proposal == nil {
		if strings.HasPrefix(quoted, "Suggested replacement for ") {
			return true, b.say(ctx, trip.GroupID, "That suggestion is no longer current. Ask me for a new replacement if you still want to change the activity.", nil)
		}
		return false, nil
	}
	shortReply := len(utf16.Encode([]rune(text))) <= 200 && activityReplacementReplyHintRe.MatchString(text)
	if !m.Tagged && !looksLikeDirectQuestion(text) && !(m.Quoted != nil && m.Quoted.FromMe) && !shortReply {
		return false, nil
	}
	if strings.HasPrefix(quoted, "Suggested replacement for ") && quoted != strAny(proposal["message"]) {
		return true, b.say(ctx, trip.GroupID, "That was an older suggestion. Do you want to use the current suggestion, keep the original, or try another option?", nil)
	}
	history, err := b.Store.GetMessages(ctx, trip.GroupID, trip.HistoryStart, 8, true)
	if err != nil {
		return true, err
	}
	contextData, err := json.Marshal(map[string]any{"suggestion": proposal, "days": asMapAny(itinerary["advisor"])["days"], "quoted_message": quoted, "recent_chat": compactChat(history, 8), "latest_reply": text})
	if err != nil {
		return true, err
	}
	out, err := b.LLM.Structured(ctx, prompts.ActivityReplacementReplySystem, []llm.Message{{Role: "user", Content: string(contextData)}}, toSchema(prompts.ActivityReplacementReply))
	if err != nil {
		return true, b.say(ctx, trip.GroupID, "Did you want to use the suggested activity, keep the original, or try another option?", nil)
	}
	action := strAny(out["action"])
	if action == "unrelated" {
		return false, nil
	}
	if action != "accept" && action != "reject" && action != "revise" {
		return true, b.say(ctx, trip.GroupID, orStr(strAny(out["clarification"]), "Did you want to use the suggested activity, keep the original, or try another option?"), nil)
	}
	body := map[string]any{"expected_revision": trip.ItineraryRevision, "proposal_id": proposal["id"], "action": action + "_activity_replacement"}
	if action == "revise" {
		body["action"] = "propose_activity_replacement"
		body["activity_id"] = proposal["activityId"]
		body["request"] = orStr(strAny(out["request"]), text)
	}
	_, message, err := b.activityReplacementAction(ctx, trip, body)
	if e, ok := err.(*DashboardError); ok {
		return true, b.say(ctx, trip.GroupID, e.Msg, nil)
	}
	if err != nil {
		return true, err
	}
	_ = b.say(ctx, trip.GroupID, message, nil)
	return true, nil
}

func (b *Brain) activityReplacementMessage(ctx context.Context, trip *models.Trip, m models.IncomingMessage, text string) error {
	body := map[string]any{"action": "propose_activity_replacement", "expected_revision": trip.ItineraryRevision, "request": text}
	if reply := activityReplacementReplyRe.FindStringSubmatch(text); reply != nil {
		body["action"] = strings.ToLower(reply[1]) + "_activity_replacement"
		body["proposal_id"] = strings.ToLower(reply[2])
	}
	_, message, err := b.activityReplacementAction(ctx, trip, body)
	if e, ok := err.(*DashboardError); ok {
		return b.say(ctx, trip.GroupID, e.Msg, nil)
	}
	if err != nil {
		return err
	}
	// The proposal/edit is saved even if its WhatsApp notification fails.
	_ = b.say(ctx, trip.GroupID, message, nil)
	return nil
}
