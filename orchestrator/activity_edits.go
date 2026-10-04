package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf16"

	"fare-brain/llm"
	"fare-brain/models"
	"fare-brain/prompts"
	"fare-brain/store"
)

var activityEditVerbRe = regexp.MustCompile(`(?i)\b(move|reschedule|rename|change|edit|update|make|push|shift|clear)\b`)
var activityEditRe = regexp.MustCompile(`(?i)\b(move|reschedule|rename|change|edit|update|make|push|shift|clear)\b.*\b(day\s*\d+|activity|activities|dinner|lunch|breakfast|tour|museum|walk|visit)\b`)
var activityUndoRe = regexp.MustCompile(`(?i)\bundo (?:the )?(?:last )?(?:itinerary|activity|schedule) edit\b`)

func looksLikeActivityEdit(text string) bool {
	return activityUndoRe.MatchString(text) || activityEditRe.MatchString(text)
}

func looksLikeTripActivityEdit(trip *models.Trip, text string) bool {
	if looksLikeNewTripRequest(text) || looksLikeCancelBooking(text) {
		return false
	}
	if looksLikeActivityEdit(text) {
		return true
	}
	if trip == nil || !activityEditVerbRe.MatchString(text) {
		return false
	}
	days := asMapAny(trip.Itinerary["advisor"])["days"]
	if len(sliceAny(days)) == 0 {
		days = asMapAny(trip.Itinerary["dashboard_plan"])["days"]
	}
	text = strings.ToLower(text)
	for _, raw := range sliceAny(days) {
		for _, item := range sliceAny(asMapAny(raw)["activities"]) {
			title := strings.ToLower(strings.TrimSpace(strAny(asMapAny(item)["title"])))
			if title != "" && strings.Contains(text, title) {
				return true
			}
		}
	}
	return false
}

func (b *Brain) activityAction(ctx context.Context, trip *models.Trip, body map[string]any) (*models.Trip, string, error) {
	if !models.ScheduleEditable(trip) {
		return nil, "", &DashboardError{Status: 409, Msg: "this trip's itinerary cannot be edited right now"}
	}
	if sid := strAny(body["session_id"]); sid != "" {
		if b.Dashboard == nil {
			return nil, "", &DashboardError{Status: 409, Msg: "active session unavailable"}
		}
		current, err := b.Dashboard.CurrentID(ctx, trip.GroupID)
		if err != nil {
			return nil, "", err
		}
		if current != sid {
			return nil, "", &DashboardError{Status: 409, Msg: "only the active trip can be edited"}
		}
	}
	revision := numAny(body["expected_revision"])
	if revision == nil || *revision != float64(trip.ItineraryRevision) {
		return nil, "", &DashboardError{Status: 409, Msg: store.ErrItineraryConflict.Error()}
	}
	itinerary := models.ScheduleItinerary(trip, trip.Itinerary, false)
	advisor := asMapAny(itinerary["advisor"])
	message := "Undid the last itinerary edit."
	if strAny(body["action"]) == "undo_activity_edit" {
		previous := asMapAny(itinerary["activity_edit_undo"])
		if previous == nil {
			return nil, "", &DashboardError{Status: 400, Msg: "there's no activity edit to undo"}
		}
		itinerary["advisor"] = previous
		delete(itinerary, "activity_edit_undo")
	} else {
		id := strAny(body["activity_id"])
		var activity map[string]any
		var dayActivities []any
		for _, raw := range sliceAny(advisor["days"]) {
			day := asMapAny(raw)
			for _, item := range sliceAny(day["activities"]) {
				candidate := asMapAny(item)
				if strAny(candidate["id"]) == id && id != "" {
					activity = candidate
					dayActivities = sliceAny(day["activities"])
				}
			}
		}
		if activity == nil {
			return nil, "", &DashboardError{Status: 404, Msg: "activity not found"}
		}
		previous := models.ScheduleItinerary(trip, itinerary, false)["advisor"]
		changed := false
		oldTime := strAny(activity["time"])
		for _, key := range []string{"time", "title", "description"} {
			raw, exists := body[key]
			if !exists {
				continue
			}
			value, ok := raw.(string)
			if !ok {
				return nil, "", &DashboardError{Status: 400, Msg: key + " must be text"}
			}
			value = strings.TrimSpace(value)
			limit := 2000
			if key == "title" {
				limit = 200
			}
			// Match the browser's maxLength, which counts UTF-16 code units.
			if (value == "" && key != "description") || len(utf16.Encode([]rune(value))) > limit {
				return nil, "", &DashboardError{Status: 400, Msg: key + " is empty or too long"}
			}
			if key == "time" {
				parsed, err := time.Parse("15:04", value)
				if err != nil || parsed.Format("15:04") != value {
					return nil, "", &DashboardError{Status: 400, Msg: "time must use HH:MM (24-hour local time)"}
				}
			}
			if strAny(activity[key]) != value {
				activity[key] = value
				changed = true
			}
		}
		if !changed {
			return nil, "", &DashboardError{Status: 400, Msg: "no activity changes provided"}
		}
		itinerary["activity_edit_undo"] = previous
		sort.SliceStable(dayActivities, func(i, j int) bool {
			return strAny(asMapAny(dayActivities[i])["time"]) < strAny(asMapAny(dayActivities[j])["time"])
		})
		message = "Updated " + strAny(activity["title"]) + ". Say “undo itinerary edit” to revert."
		if oldTime != strAny(activity["time"]) {
			message = fmt.Sprintf("Moved %s from %s to %s. Say “undo itinerary edit” to revert.", strAny(activity["title"]), oldTime, strAny(activity["time"]))
		}
	}
	itinerary = models.ScheduleItinerary(trip, itinerary, false)
	plan := asMapAny(itinerary["dashboard_plan"])
	if plan != nil {
		plan["itineraryRevision"] = trip.ItineraryRevision + 1
	}
	updated, err := b.Store.UpdateItinerary(ctx, trip, itinerary)
	if errors.Is(err, store.ErrItineraryConflict) {
		return nil, "", &DashboardError{Status: 409, Msg: err.Error()}
	}
	if err != nil {
		return nil, "", err
	}
	// The edit is committed; a missed notification must not turn it into a failed save.
	// Dashboard polling can recover the persisted plan.
	_ = b.dashboardEvent(ctx, updated, "itinerary.updated", map[string]any{"plan": plan, "message": message})
	return updated, message, nil
}

func (b *Brain) editActivityMessage(ctx context.Context, trip *models.Trip, m models.IncomingMessage) error {
	body := map[string]any{"action": "undo_activity_edit", "expected_revision": trip.ItineraryRevision}
	if !activityUndoRe.MatchString(m.Text) {
		itinerary := models.ScheduleItinerary(trip, trip.Itinerary, false)
		days := asMapAny(itinerary["advisor"])["days"]
		raw, err := json.Marshal(days)
		if err != nil {
			return err
		}
		user := fmt.Sprintf("Request: %s\nCurrent activities (day 1 is the first listed day): %s", m.Text, raw)
		out, err := b.LLM.Structured(ctx, prompts.ActivityEditSystem, []llm.Message{{Role: "user", Content: user}}, toSchema(prompts.ActivityEdit))
		if err != nil {
			return b.say(ctx, trip.GroupID, "I couldn’t interpret that edit. Please try again with the activity and day.", nil)
		}
		if strAny(out["activity_id"]) == "" {
			return b.say(ctx, trip.GroupID, orStr(strAny(out["clarification"]), "Please repeat the edit with the specific day and activity."), nil)
		}
		body["action"], body["activity_id"] = "update_activity", out["activity_id"]
		for _, raw := range sliceAny(out["changed_fields"]) {
			key := strAny(raw)
			if key == "time" || key == "title" || key == "description" {
				body[key] = out[key]
			}
		}
	}
	_, message, err := b.activityAction(ctx, trip, body)
	if e, ok := err.(*DashboardError); ok {
		return b.say(ctx, trip.GroupID, e.Msg, nil)
	}
	if err != nil {
		return err
	}
	// Delivery failure does not invalidate the already saved itinerary.
	_ = b.say(ctx, trip.GroupID, message, nil)
	return nil
}
