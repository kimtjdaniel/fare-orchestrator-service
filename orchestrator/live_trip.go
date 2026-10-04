package orchestrator

import (
	"context"
	"strconv"
	"strings"
	"time"

	"fare-brain/formatting"
	"fare-brain/models"
)

type DashboardError struct {
	Status int
	Msg    string
}

func (e *DashboardError) Error() string { return e.Msg }

func (b *Brain) ListDashboardTrips(ctx context.Context) ([]map[string]any, error) {
	trips, err := b.Store.ListTrips(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(trips))
	for _, t := range trips {
		if t == nil {
			continue
		}
		out = append(out, map[string]any{
			"id": t.ID, "group_id": t.GroupID, "state": t.State,
			"destination": tripDestination(t),
			"start_date":  t.EmbarkingDate, "end_date": t.ReturningDate,
			"updated_at": t.UpdatedAt,
		})
	}
	return out, nil
}

func (b *Brain) DashboardView(ctx context.Context, groupID string) (map[string]any, error) {
	trip, err := b.Store.GetTrip(ctx, groupID)
	if err != nil || trip == nil {
		return nil, &DashboardError{Status: 404, Msg: "trip not found"}
	}
	return b.dashboardPayload(ctx, trip)
}

func (b *Brain) DashboardAct(ctx context.Context, groupID string, body map[string]any) (map[string]any, error) {
	trip, err := b.Store.GetTrip(ctx, groupID)
	if err != nil || trip == nil {
		return nil, &DashboardError{Status: 404, Msg: "trip not found"}
	}
	if body == nil {
		body = map[string]any{}
	}
	action := strings.ToLower(strings.TrimSpace(strAny(body["action"])))
	itin := cloneMap(trip.Itinerary)
	if itin == nil {
		itin = map[string]any{}
	}
	fields := map[string]any{}

	switch action {
	case "pick_flight":
		offer := findItinOffer(itin["flight_options"], strAny(body["offer_id"]))
		if offer == nil {
			return nil, &DashboardError{Status: 404, Msg: "flight offer not found"}
		}
		itin["selected_flight"] = offer
		itin["selected_flight_id"] = strAny(offer["offer_id"])
		fields["itinerary"] = itin
	case "pick_hotel":
		offer := findItinOffer(itin["hotel_options"], strAny(body["offer_id"]))
		if offer == nil {
			return nil, &DashboardError{Status: 404, Msg: "hotel offer not found"}
		}
		itin["selected_hotel"] = offer
		itin["selected_hotel_id"] = strAny(offer["offer_id"])
		itin["hotel"] = offer
		fields["itinerary"] = itin
	case "set_budget":
		if n := numAny(body["amount_cad"]); n != nil {
			itin["budget_cad"] = *n
			fields["itinerary"] = itin
			fields["cost_per_person"] = *n
		}
	case "set_payer":
		name := strAny(body["name"])
		if name == "" {
			return nil, &DashboardError{Status: 400, Msg: "name is required"}
		}
		found := false
		people := append([]models.Participant(nil), trip.Participants...)
		for i := range people {
			match := strings.EqualFold(people[i].WhatsAppName, name) || strings.EqualFold(people[i].LegalName, name)
			people[i].Payer = match
			if match {
				found = true
			}
		}
		if !found {
			people = append(people, models.Participant{WhatsAppName: name, Payer: true})
		}
		fields["participants"] = people
		fields["payer_name"] = name
	case "save_traveler":
		pid := strAny(body["participant_id"])
		name := strAny(body["whatsapp_name"])
		people := append([]models.Participant(nil), trip.Participants...)
		idx := -1
		for i, p := range people {
			if (pid != "" && p.PID == pid) || (name != "" && strings.EqualFold(p.WhatsAppName, name)) {
				idx = i
				break
			}
		}
		if idx < 0 {
			return nil, &DashboardError{Status: 404, Msg: "traveler not found"}
		}
		p := &people[idx]
		if v := strAny(body["legal_name"]); v != "" {
			p.LegalName = v
		}
		if v := strAny(body["date_of_birth"]); v != "" {
			p.DateOfBirth = v
		}
		if v := strAny(body["passport_number"]); v != "" {
			p.PassportNumber = v
			p.UploadedPassportInfo = true
		}
		if v := strAny(body["origin"]); v != "" {
			p.Origin = v
			p.OriginCity = v
		}
		fields["participants"] = people
	default:
		return nil, &DashboardError{Status: 400, Msg: "unknown action"}
	}

	trip, err = b.Store.UpdateTrip(ctx, trip.ID, fields)
	if err != nil {
		return nil, err
	}
	_ = b.say(ctx, groupID, "Dashboard updated — I saved that on the trip.", nil)
	return b.dashboardPayload(ctx, trip)
}

func (b *Brain) dashboardPayload(ctx context.Context, trip *models.Trip) (map[string]any, error) {
	itin := trip.Itinerary
	if itin == nil {
		itin = map[string]any{}
	}
	spend := formatting.ComputeSpend(trip)
	msgs, _ := b.Store.GetMessages(ctx, trip.GroupID, nil, 80, true)
	return map[string]any{
		"id": trip.ID, "group_id": trip.GroupID, "state": trip.State,
		"destination":      tripDestination(trip),
		"start_date":       trip.EmbarkingDate, "end_date": trip.ReturningDate,
		"nights":           trip.DurationNights,
		"budget_cad":       numAny(itin["budget_cad"]),
		"payer_name":       trip.PayerName,
		"locked_spend_cad": spend.TravelEach, "food_estimate_cad": 0,
		"people":           dashPeople(trip),
		"places":           dashPlaces(trip),
		"flights":          dashItinOffers(itin, "flight_options", "selected_flight_id"),
		"hotels":           dashItinOffers(itin, "hotel_options", "selected_hotel_id"),
		"messages":         dashMessages(msgs),
		"itinerary":        itin,
		"config":           map[string]any{"currency": "CAD", "mock_travel": b.Config.MockTravel},
	}, nil
}

func dashPeople(trip *models.Trip) []map[string]any {
	out := make([]map[string]any, 0, len(trip.Participants))
	for _, p := range trip.Participants {
		out = append(out, map[string]any{
			"id": p.PID, "whatsapp_name": p.WhatsAppName, "legal_name": p.LegalName,
			"date_of_birth": p.DateOfBirth, "passport_last4": last4(p.PassportNumber),
			"has_passport": p.PassportNumber != "", "origin": orStr(p.Origin, p.OriginCity),
			"payer": p.Payer, "activity": p.GeneralPreferences.Activity, "culinary": p.GeneralPreferences.Culinary,
		})
	}
	return out
}

func dashPlaces(trip *models.Trip) []string {
	seen := map[string]bool{}
	var out []string
	add := func(v string) {
		v = strings.TrimSpace(v)
		if v == "" || seen[strings.ToLower(v)] {
			return
		}
		seen[strings.ToLower(v)] = true
		out = append(out, v)
	}
	add(trip.Destination)
	if trip.Itinerary != nil {
		add(strAny(trip.Itinerary["destination"]))
	}
	for _, o := range trip.Options {
		add(o.Destination)
	}
	return out
}

func dashItinOffers(itin map[string]any, listKey, selKey string) []map[string]any {
	selID := strAny(itin[selKey])
	raw := sliceAny(itin[listKey])
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		m := asMapAny(item)
		if m == nil {
			continue
		}
		row := cloneMap(m)
		id := orStr(strAny(m["offer_id"]), strAny(m["id"]))
		row["picked"] = id != "" && id == selID
		out = append(out, row)
	}
	return out
}

func dashMessages(msgs []models.Message) []map[string]any {
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, map[string]any{
			"id": m.ExternalID, "from": m.SenderName, "text": m.Text,
			"at": m.SentAt.UTC().Format(time.RFC3339), "from_agent": m.IsBot,
		})
	}
	return out
}

func findItinOffer(raw any, id string) map[string]any {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil
	}
	for _, item := range sliceAny(raw) {
		m := asMapAny(item)
		if m == nil {
			continue
		}
		if strAny(m["offer_id"]) == id || strAny(m["id"]) == id {
			return cloneMap(m)
		}
	}
	return nil
}

func sliceAny(v any) []any {
	switch t := v.(type) {
	case []any:
		return t
	case []map[string]any:
		out := make([]any, len(t))
		for i, m := range t {
			out[i] = m
		}
		return out
	default:
		return nil
	}
}

func cloneMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func asMapAny(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}

func strAny(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

func numAny(v any) *float64 {
	switch t := v.(type) {
	case float64:
		return &t
	case float32:
		f := float64(t)
		return &f
	case int:
		f := float64(t)
		return &f
	case int64:
		f := float64(t)
		return &f
	case string:
		if strings.TrimSpace(t) == "" {
			return nil
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		if err != nil {
			return nil
		}
		return &f
	}
	return nil
}

func orStr(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

func last4(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= 4 {
		return s
	}
	return s[len(s)-4:]
}
