package orchestrator

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"time"

	"fare-brain/formatting"
	"fare-brain/models"
	"fare-brain/tools"
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
			"group_id":    t.GroupID,
			"group_name":  t.GroupName,
			"destination": tripDestination(t),
			"origin":      t.Origin,
			"dates":       formatting.Dates(t.EmbarkingDate, t.ReturningDate),
			"state":       t.State,
			"updated_at":  t.UpdatedAt,
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
	if action == "chat" {
		text := strAny(body["text"])
		if text == "" {
			return nil, &DashboardError{Status: 400, Msg: "text is required"}
		}
		actor := strAny(body["actor"])
		if actor == "" {
			actor = "Someone"
		}
		b.Handle(ctx, models.IncomingMessage{
			GroupID: groupID, GroupName: trip.GroupName,
			SenderID: "dashboard:" + actor, SenderName: actor,
			Text: text, Tagged: true,
		})
		fresh, err := b.Store.GetTrip(ctx, groupID)
		if err != nil || fresh == nil {
			return b.dashboardPayload(ctx, trip)
		}
		return b.dashboardPayload(ctx, fresh)
	}

	itin := cloneMap(trip.Itinerary)
	if itin == nil {
		itin = map[string]any{}
	}
	fields := map[string]any{}

	switch action {
	case "pick_flight", "select_flight":
		offer := findItinOffer(itin["flight_options"], strAny(body["offer_id"]))
		if offer == nil {
			return nil, &DashboardError{Status: 404, Msg: "flight offer not found"}
		}
		itin["selected_flight"] = offer
		itin["selected_flight_id"] = strAny(offer["offer_id"])
		fields["itinerary"] = itin
	case "pick_hotel", "select_hotel":
		offer := findItinOffer(itin["hotel_options"], strAny(body["offer_id"]))
		if offer == nil {
			return nil, &DashboardError{Status: 404, Msg: "hotel offer not found"}
		}
		itin["selected_hotel"] = offer
		itin["selected_hotel_id"] = strAny(offer["offer_id"])
		itin["hotel"] = offer
		var hotel models.HotelOffer
		if err := decodeInto(offer, &hotel); err != nil {
			return nil, err
		}
		checkIn, _ := models.ParseDate(orStr(hotel.CheckIn, trip.EmbarkingDate))
		checkOut, _ := models.ParseDate(orStr(hotel.CheckOut, trip.ReturningDate))
		accommodations := append([]models.Accommodation(nil), trip.Accommodations...)
		if len(accommodations) == 0 {
			accommodations = []models.Accommodation{{}}
		}
		// Preserve existing booking state while carrying the selected offer's provenance.
		accommodation := &accommodations[0]
		if accommodation.BookingStatus == "" {
			accommodation.BookingStatus = models.StatusIncomplete
		}
		accommodation.CheckInDate, accommodation.CheckOutDate = checkIn, checkOut
		accommodation.Rating, accommodation.Costs, accommodation.BookingURL = hotel.Rating, &hotel.TotalPrice, hotel.CheckoutURL
		accommodation.Source, accommodation.PropertyType = hotel.Source, hotel.PropertyType
		accommodation.OriginalRating, accommodation.OriginalRatingScale = hotel.OriginalRating, hotel.OriginalRatingScale
		accommodation.PriceNote = hotel.PriceNote
		fields["accommodations"] = accommodations
		if existing := asMapAny(itin["dashboard_plan"]); len(existing) > 0 {
			plan := cloneMap(existing)
			guests := len(trip.Participants)
			if guests < 1 {
				guests = 1
			}
			plan["hotel"], plan["hotelOfferId"] = hotel.Name, hotel.OfferID
			plan["hotelPrice"] = hotel.TotalPrice / float64(guests)
			plan["hotelSource"], plan["hotelPropertyType"] = hotel.Source, hotel.PropertyType
			plan["hotelOriginalRating"], plan["hotelOriginalRatingScale"] = hotel.OriginalRating, hotel.OriginalRatingScale
			plan["hotelPriceNote"] = hotel.PriceNote
			itin["dashboard_plan"] = plan
		}
		fields["itinerary"] = itin
	case "set_budget":
		note := strAny(body["budget"])
		if note == "" {
			if n := numAny(body["amount_cad"]); n != nil {
				note = strconv.FormatFloat(*n, 'f', 0, 64)
			}
		}
		fields["budget_note"] = note
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
		name := orStr(strAny(body["name"]), strAny(body["whatsapp_name"]))
		pid := strAny(body["participant_id"])
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
	if action != "save_traveler" {
		_ = b.say(ctx, groupID, "Dashboard updated — I saved that on the trip.", nil)
	} else {
		_ = b.say(ctx, groupID, "Saved traveler details on the trip. I didn't put the passport in chat.", nil)
	}
	return b.dashboardPayload(ctx, trip)
}

func (b *Brain) dashboardPayload(ctx context.Context, trip *models.Trip) (map[string]any, error) {
	itin := trip.Itinerary
	if itin == nil {
		itin = map[string]any{}
	}
	spend := formatting.ComputeSpend(trip)
	advisor := asMapAny(itin["advisor"])
	msgs, _ := b.Store.GetMessages(ctx, trip.GroupID, nil, 80, true)
	people := dashPeople(trip)
	editable := trip.State != models.Booked && trip.State != models.Cancelled && trip.State != models.BookingState
	return map[string]any{
		"group_id":    trip.GroupID,
		"group_name":  trip.GroupName,
		"state":       trip.State,
		"editable":    editable,
		"destination": tripDestination(trip),
		"origin":      trip.Origin,
		"dates":       formatting.Dates(trip.EmbarkingDate, trip.ReturningDate),
		"nights":      trip.DurationNights,
		"budget_note": trip.BudgetNote,
		"payer_name":  trip.PayerName,
		"updated_at":  trip.UpdatedAt,
		"spend": map[string]any{
			"flight_each":   spend.FlightEach,
			"hotel_group":   spend.HotelGroup,
			"hotel_each":    spend.HotelEach,
			"travel_each":   spend.TravelEach,
			"travel_group":  spend.TravelGroup,
			"people":        spend.People,
			"includes_food": false,
			"food_note":     strAny(advisor["food_note"]),
			"food_per_day":  numAny(advisor["food_per_day_cad"]),
			"food_trip":     numAny(advisor["food_trip_cad"]),
		},
		"owes":     dashOwes(trip, spend),
		"people":   people,
		"days":     dashDays(itin),
		"places":   dashPlaces(trip, itin),
		"flights":  dashViewFlights(itin),
		"hotels":   dashViewHotels(itin),
		"messages": dashMessages(msgs),
	}, nil
}

func dashPeople(trip *models.Trip) []map[string]any {
	out := make([]map[string]any, 0, len(trip.Participants))
	for _, p := range trip.Participants {
		out = append(out, map[string]any{
			"name": p.WhatsAppName, "payer": p.Payer, "legal_name": p.LegalName,
			"date_of_birth": p.DateOfBirth, "has_passport": p.PassportNumber != "",
			"passport_last4": last4(p.PassportNumber), "origin": orStr(p.Origin, p.OriginCity),
		})
	}
	return out
}

func dashDays(itin map[string]any) []map[string]any {
	out := make([]map[string]any, 0)
	push := func(raw any) {
		for _, item := range sliceAny(raw) {
			d := asMapAny(item)
			if d == nil {
				continue
			}
			title := orStr(strAny(d["title"]), strAny(d["date"]))
			body := orStr(strAny(d["body"]), strAny(d["description"]))
			if title == "" && body == "" {
				continue
			}
			out = append(out, map[string]any{"title": title, "body": body, "food_cad": numAny(d["food_cad"])})
		}
	}
	if advisor := asMapAny(itin["advisor"]); advisor != nil {
		push(advisor["days"])
	}
	if len(out) == 0 {
		if plan := asMapAny(itin["dashboard_plan"]); plan != nil {
			push(plan["days"])
		}
	}
	return out
}

func dashPlaces(trip *models.Trip, itin map[string]any) []map[string]any {
	city := tripDestination(trip)
	out := make([]map[string]any, 0)
	rest := asMapAny(itin["restaurants"])
	if rest == nil {
		return out
	}
	for _, item := range sliceAny(rest["places"]) {
		p := asMapAny(item)
		if p == nil {
			continue
		}
		name := strAny(p["name"])
		if name == "" {
			continue
		}
		q := name
		if city != "" {
			q += " " + city
		}
		out = append(out, map[string]any{
			"name": name, "neighborhood": strAny(p["neighborhood"]), "why": strAny(p["why"]),
			"dish": strAny(p["dish"]), "est_cad": numAny(p["est_cad"]),
			"map": "https://www.google.com/maps/search/?api=1&query=" + url.QueryEscape(q),
		})
	}
	return out
}

func dashViewFlights(itin map[string]any) []map[string]any {
	selID := strAny(itin["selected_flight_id"])
	if selID == "" {
		selID = nestStr(asMapAny(itin["selected_flight"]), "offer_id")
	}
	out := make([]map[string]any, 0)
	seen := map[string]bool{}
	push := func(m map[string]any) {
		if m == nil {
			return
		}
		id := orStr(strAny(m["offer_id"]), strAny(m["id"]))
		if id != "" && seen[id] {
			return
		}
		if id != "" {
			seen[id] = true
		}
		out = append(out, map[string]any{
			"offer_id": id, "airline": strAny(m["airline"]),
			"origin": strAny(m["origin"]), "destination": strAny(m["destination"]),
			"summary": strAny(m["summary"]), "price": floatOr(m["price"]),
			"selected": id != "" && id == selID,
		})
	}
	push(asMapAny(itin["selected_flight"]))
	push(mapVal(asMapAny(itin["flights"]), "embarking"))
	for _, item := range sliceAny(itin["flight_options"]) {
		push(asMapAny(item))
	}
	return out
}

func dashViewHotels(itin map[string]any) []map[string]any {
	selID := strAny(itin["selected_hotel_id"])
	hotel := asMapAny(itin["hotel"])
	if selID == "" {
		selID = nestStr(hotel, "offer_id")
	}
	out := make([]map[string]any, 0)
	seen := map[string]bool{}
	push := func(m map[string]any) {
		if m == nil {
			return
		}
		id := orStr(strAny(m["offer_id"]), strAny(m["id"]))
		if id != "" && seen[id] {
			return
		}
		if id != "" {
			seen[id] = true
		}
		out = append(out, map[string]any{
			"offer_id": id, "name": strAny(m["name"]), "city": strAny(m["city"]),
			"nightly": floatOr(m["price_per_night"]), "total": floatOr(m["total_price"]),
			"rating": numAny(m["rating"]), "image": strAny(m["image_url"]),
			"source": m["source"], "property_type": m["property_type"],
			"original_rating": m["original_rating"], "original_rating_scale": m["original_rating_scale"],
			"price_note": m["price_note"], "checkout_url": m["checkout_url"],
			"selected": id != "" && id == selID,
		})
	}
	push(asMapAny(itin["selected_hotel"]))
	push(hotel)
	for _, item := range sliceAny(itin["hotel_options"]) {
		push(asMapAny(item))
	}
	return out
}

func dashOwes(trip *models.Trip, spend formatting.Spend) []map[string]any {
	if split := mapVal(trip.Itinerary, "split"); split != nil {
		rows := make([]map[string]any, 0)
		for _, item := range sliceAny(split["owes"]) {
			o := asMapAny(item)
			if o == nil {
				continue
			}
			rows = append(rows, map[string]any{"from": strAny(o["from"]), "to": strAny(o["to"]), "amount": floatOr(o["amount"])})
		}
		if len(rows) > 0 {
			return rows
		}
	}
	if !spend.Ok() || trip.PayerName == "" {
		return []map[string]any{}
	}
	names := make([]string, 0, len(trip.Participants))
	for _, p := range trip.Participants {
		if strings.TrimSpace(p.WhatsAppName) != "" {
			names = append(names, p.WhatsAppName)
		}
	}
	split := tools.ComputeSplit(names, spend.FlightEach, spend.HotelGroup, trip.PayerName)
	rows := make([]map[string]any, 0, len(split.Owes))
	for _, o := range split.Owes {
		rows = append(rows, map[string]any{"from": o.From, "to": o.To, "amount": o.Amount})
	}
	return rows
}

func dashMessages(msgs []models.Message) []map[string]any {
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, map[string]any{
			"id": m.ExternalID, "sender": m.SenderName, "text": m.Text,
			"bot": m.IsBot || m.SenderID == "fare-bot", "at": m.SentAt.UTC().Format(time.RFC3339),
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

func floatOr(v any) float64 {
	if n := numAny(v); n != nil {
		return *n
	}
	return 0
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

func nestStr(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	return strAny(m[key])
}

func mapVal(m map[string]any, key string) map[string]any {
	if m == nil {
		return nil
	}
	return asMapAny(m[key])
}
