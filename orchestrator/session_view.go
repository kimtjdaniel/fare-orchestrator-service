package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"time"

	"fare-brain/formatting"
	"fare-brain/models"
)

func (b *Brain) ListGroupSnapshots(ctx context.Context, groupID string) ([]map[string]any, error) {
	snap, err := b.sessionSnapshot(ctx, groupID, "")
	if err != nil {
		if de, ok := err.(*DashboardError); ok && de.Status == 404 {
			return []map[string]any{}, nil
		}
		return nil, err
	}
	return []map[string]any{snap}, nil
}

func (b *Brain) GetSessionSnapshot(ctx context.Context, groupID, sessionID string) (map[string]any, error) {
	return b.sessionSnapshot(ctx, groupID, sessionID)
}

func (b *Brain) GroupEventsEnvelope(ctx context.Context, groupID string) (map[string]any, int64, error) {
	sessions, err := b.ListGroupSnapshots(ctx, groupID)
	if err != nil {
		return nil, 0, err
	}
	rev := int64(0)
	if len(sessions) > 0 {
		rev = int64(num(sessions[0]["revision"]))
	}
	return map[string]any{
		"version":  1,
		"type":     "group.snapshot",
		"groupId":  groupID,
		"revision": rev,
		"sessions": sessions,
	}, rev, nil
}

func (b *Brain) sessionSnapshot(ctx context.Context, groupID, sessionID string) (map[string]any, error) {
	trip, err := b.Store.GetTrip(ctx, groupID)
	if err != nil {
		return nil, err
	}
	if trip == nil {
		return nil, &DashboardError{Status: 404, Msg: "No trip yet. Sessions are created from WhatsApp only."}
	}
	sid := or(trip.ID, trip.GroupID)
	if sessionID != "" && sessionID != sid && sessionID != trip.GroupID {
		return nil, &DashboardError{Status: 404, Msg: "This session was not found in the group."}
	}
	msgs, err := b.Store.GetMessages(ctx, groupID, nil, 12, true)
	if err != nil {
		return nil, err
	}
	itin := trip.Itinerary
	if itin == nil {
		itin = map[string]any{}
	}
	flights := sessionFlights(itin)
	hotels := sessionHotels(itin, trip)
	hasFlights := len(flights) > 0
	hasHotels := len(hotels) > 0
	hasPlan := len(dashDays(asMapAny(itin["advisor"]))) > 0
	status, flight, hotel, planning := sessionSteps(trip, hasFlights, hasHotels, hasPlan)
	tasks := map[string]string{
		"flight-prices":  taskStatus(hasFlights, planning),
		"hotel-location": taskStatus(hasHotels, planning),
		"group-budget":   taskStatus(trip.BudgetNote != "" || trip.CostPerPerson != nil, planning),
		"daily-schedule": taskStatus(hasPlan, planning),
	}
	spend := formatting.ComputeSpend(trip)
	plan := sessionPlan(trip, itin, spend, hasPlan)
	activity := sessionActivity(msgs)
	rev := trip.UpdatedAt.UnixMilli()
	if rev <= 0 {
		rev = time.Now().UnixMilli()
	}
	dest := or(trip.Destination, "Planning your trip")
	origin := or(trip.Origin, "TBD")
	return map[string]any{
		"session": map[string]any{
			"id":                 sid,
			"groupId":            trip.GroupID,
			"destination":        dest,
			"origin":             origin,
			"originAirport":      trip.Origin,
			"destinationAirport": trip.DestinationAirport,
			"adults":             max(1, len(trip.Participants)),
			"startDate":          trip.EmbarkingDate,
			"endDate":            trip.ReturningDate,
			"status":             status,
			"message":            sessionMessage(trip, status),
			"createdAt":          trip.CreatedAt.UTC().Format(time.RFC3339),
		},
		"revision":       rev,
		"flight":         flight,
		"hotel":          hotel,
		"planning":       planning,
		"planningTasks":  tasks,
		"flightMessage":  flightMsg(trip, hasFlights, flight),
		"hotelMessage":   hotelMsg(trip, hasHotels, hotel),
		"flights":        flights,
		"hotels":         hotels,
		"plan":           plan,
		"activity":       activity,
		"error":          sessionError(trip),
		"previews":       map[string]any{"flight": map[string]any{}, "hotel": map[string]any{}},
	}, nil
}

func sessionSteps(trip *models.Trip, hasFlights, hasHotels, hasPlan bool) (status, flight, hotel, planning string) {
	switch trip.State {
	case models.Cancelled:
		return "failed", "failed", "failed", "failed"
	case models.Searching:
		flight, hotel = "running", "running"
		if hasFlights {
			flight = "completed"
		}
		if hasHotels {
			hotel = "completed"
		}
		return "searching", flight, hotel, "pending"
	case models.Collecting, models.AwaitingChoice:
		return "created", "pending", "pending", "pending"
	case models.Booked:
		return "completed", "completed", "completed", "completed"
	default:
		flight, hotel = "pending", "pending"
		if hasFlights {
			flight = "completed"
		}
		if hasHotels {
			hotel = "completed"
		}
		planning = "pending"
		if hasFlights && hasHotels {
			planning = "running"
		}
		if hasPlan || trip.State == models.Booked {
			planning = "completed"
		}
		status = "planning"
		if hasPlan || trip.State == models.AwaitingApproval || trip.State == models.BookingState {
			status = "planning"
		}
		if trip.State == models.Booked {
			status = "completed"
		}
		if !hasFlights && !hasHotels && trip.State == models.AwaitingApproval {
			status = "searching"
		}
		return status, flight, hotel, planning
	}
}

func taskStatus(done bool, planning string) string {
	if done {
		return "completed"
	}
	if planning == "running" {
		return "running"
	}
	if planning == "failed" {
		return "failed"
	}
	return "pending"
}

func sessionMessage(trip *models.Trip, status string) string {
	switch status {
	case "created":
		return "Tag Fare in WhatsApp. A destination choice starts the search agents."
	case "searching":
		return "Looking up flights and stays for the dates the group picked."
	case "planning":
		return "Putting the itinerary, fares, and stay together."
	case "completed":
		return "The plan is ready for the group. Nothing is booked until you approve in chat."
	case "failed":
		return "This trip was cancelled. Start again from WhatsApp."
	default:
		return trip.GroupName
	}
}

func sessionError(trip *models.Trip) any {
	if trip.State == models.Cancelled {
		return "This trip was cancelled in WhatsApp."
	}
	return nil
}

func flightMsg(trip *models.Trip, has bool, status string) string {
	if status == "failed" {
		return "Flight search did not finish. Pick another destination in WhatsApp."
	}
	if has {
		return "Saved fares from the group search. Choosing one updates WhatsApp."
	}
	if trip.State == models.Collecting || trip.State == models.AwaitingChoice {
		return "Flights appear after the group picks a destination in chat."
	}
	return "Waiting for flight results from the chat workflow."
}

func hotelMsg(trip *models.Trip, has bool, status string) string {
	if status == "failed" {
		return "Stay search did not finish. Pick another destination in WhatsApp."
	}
	if has {
		return "Saved stays from the group search. Choosing one updates WhatsApp."
	}
	if trip.State == models.Collecting || trip.State == models.AwaitingChoice {
		return "Stays appear after the group picks a destination in chat."
	}
	return "Waiting for hotel results from the chat workflow."
}

func sessionFlights(itin map[string]any) []map[string]any {
	out := make([]map[string]any, 0)
	for _, item := range dashFlights(itin) {
		origin := str(item["origin"])
		dest := str(item["destination"])
		summary := str(item["summary"])
		duration, stops := "—", "—"
		if parts := strings.Split(summary, "·"); len(parts) >= 2 {
			duration = strings.TrimSpace(parts[0])
			stops = strings.TrimSpace(parts[1])
		}
		out = append(out, map[string]any{
			"id":       or(str(item["offer_id"]), fmt.Sprintf("flight_%d", len(out)+1)),
			"airline":  or(str(item["airline"]), "Flight"),
			"route":    strings.TrimSpace(origin + " → " + dest),
			"duration": duration,
			"stops":    stops,
			"price":    num(item["price"]),
			"currency": "CAD",
		})
	}
	return out
}

func sessionHotels(itin map[string]any, trip *models.Trip) []map[string]any {
	out := make([]map[string]any, 0)
	nights := trip.DurationNights
	adults := len(trip.Participants)
	if adults < 1 {
		adults = 2
	}
	for _, item := range dashHotels(itin) {
		total := num(item["total"])
		out = append(out, map[string]any{
			"id":           or(str(item["offer_id"]), fmt.Sprintf("hotel_%d", len(out)+1)),
			"name":         or(str(item["name"]), "Stay"),
			"neighborhood": or(str(item["city"]), trip.Destination),
			"rating":       item["rating"],
			"price":        total,
			"nights":       nights,
			"adults":       adults,
			"currency":     "CAD",
		})
	}
	return out
}

func sessionPlan(trip *models.Trip, itin map[string]any, spend formatting.Spend, hasPlan bool) any {
	if !hasPlan && str(asMapAny(itin["hotel"])["name"]) == "" && num(asMapAny(itin["flights"])["round_trip_each"]) == 0 {
		return nil
	}
	advisor := asMapAny(itin["advisor"])
	hotel := asMapAny(itin["hotel"])
	flights := asMapAny(itin["flights"])
	embark := asMapAny(flights["embarking"])
	days := make([]map[string]any, 0)
	for i, d := range dashDays(advisor) {
		date := trip.EmbarkingDate
		if t, err := time.Parse("2006-01-02", trip.EmbarkingDate); err == nil {
			date = t.AddDate(0, 0, i).Format("2006-01-02")
		}
		title := str(d["title"])
		body := str(d["body"])
		days = append(days, map[string]any{
			"date":        date,
			"title":       title,
			"description": body,
			"activities": []map[string]any{{
				"time":        "09:00",
				"title":       title,
				"description": body,
			}},
		})
	}
	if len(days) == 0 && !hasPlan {
		return nil
	}
	route := strings.TrimSpace(str(embark["origin"]) + " → " + str(embark["destination"]))
	if route == "→" {
		route = strings.TrimSpace(trip.Origin + " → " + trip.Destination)
	}
	return map[string]any{
		"isSampleSchedule": false,
		"flight":           or(str(embark["airline"]), "Group flight"),
		"route":            route,
		"flightPrice":      spend.FlightEach,
		"hotel":            or(str(hotel["name"]), "Group stay"),
		"nights":           trip.DurationNights,
		"hotelPrice":       spend.HotelEach,
		"explanation":      or(str(advisor["why"]), "Built from the WhatsApp plan and the fares the group locked in."),
		"days":             days,
	}
}

func sessionActivity(msgs []models.Message) []map[string]any {
	out := make([]map[string]any, 0, len(msgs))
	for i, msg := range msgs {
		text := strings.TrimSpace(msg.Text)
		if text == "" {
			continue
		}
		if len(text) > 140 {
			text = text[:137] + "…"
		}
		who := or(msg.SenderName, "Fare")
		if msg.IsBot {
			who = "Fare"
		}
		out = append(out, map[string]any{
			"id":        i + 1,
			"timestamp": msg.SentAt.UTC().Format(time.RFC3339),
			"message":   who + ": " + text,
		})
	}
	return out
}
