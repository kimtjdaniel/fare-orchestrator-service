package models

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// ScheduleItinerary copies the schedule and derives both views from the same activities.
// Legacy IDs are deterministic until the first edit persists them.
func ScheduleItinerary(trip *Trip, itinerary map[string]any, fresh bool) map[string]any {
	raw, _ := json.Marshal(itinerary)
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	if out == nil {
		out = map[string]any{}
	}
	advisor, _ := out["advisor"].(map[string]any)
	plan, _ := out["dashboard_plan"].(map[string]any)
	if advisor == nil {
		advisor = map[string]any{}
		out["advisor"] = advisor
	}
	if plan == nil {
		plan = map[string]any{}
	}
	days, _ := advisor["days"].([]any)
	if len(days) == 0 {
		days, _ = plan["days"].([]any)
	}
	start, err := ParseDate(trip.EmbarkingDate)
	dashboardDays := []any{}
	for di, rawDay := range days {
		day, ok := rawDay.(map[string]any)
		if !ok {
			continue
		}
		if err == nil {
			day["date"] = start.AddDate(0, 0, di).Format("2006-01-02")
		}
		activities, _ := day["activities"].([]any)
		if activities == nil {
			activities = []any{}
		}
		body := []string{}
		for ai, rawActivity := range activities {
			activity, ok := rawActivity.(map[string]any)
			if !ok {
				continue
			}
			if id, _ := activity["id"].(string); id == "" {
				if fresh {
					activity["id"] = uuid.NewString()
				} else {
					activity["id"] = uuid.NewSHA1(uuid.NameSpaceURL, []byte(fmt.Sprintf("%s:%v:%d:%d", trip.ID, trip.HistoryStart, di, ai))).String()
				}
			}
			title, _ := activity["title"].(string)
			clock, _ := activity["time"].(string)
			description, _ := activity["description"].(string)
			body = append(body, strings.TrimSpace(clock+" "+title+". "+description))
		}
		if len(body) > 0 {
			day["body"] = strings.Join(body, " ")
		}
		day["activities"] = activities
		description := day["body"]
		if description == nil {
			description = day["description"]
		}
		dashboardDays = append(dashboardDays, map[string]any{"date": day["date"], "title": day["title"], "description": description, "activities": activities})
	}
	advisor["days"] = days
	plan["days"] = dashboardDays
	plan["itineraryRevision"] = trip.ItineraryRevision
	plan["itineraryEditable"] = ScheduleEditable(trip) && len(days) > 0
	plan["canUndoActivityEdit"] = out["activity_edit_undo"] != nil
	plan["flightReason"] = SelectedTravelReason(out, "flight")
	plan["hotelReason"] = SelectedTravelReason(out, "hotel")
	plan["selectionReason"] = out["selection_reason"]
	return out
}

func ScheduleEditable(trip *Trip) bool {
	return trip.State != Booked && trip.State != Cancelled && trip.State != BookingState && trip.State != Searching
}

// SelectedTravelReason is valid only for the exact offer it was generated for.
func SelectedTravelReason(itinerary map[string]any, kind string) string {
	selectedID, _ := itinerary["selected_"+kind+"_id"].(string)
	reasonID, _ := itinerary[kind+"_reason_offer_id"].(string)
	if selectedID == "" || reasonID != selectedID {
		return ""
	}
	reason, _ := itinerary[kind+"_reason"].(string)
	return strings.TrimSpace(reason)
}
