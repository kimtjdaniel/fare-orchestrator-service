package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"fare-brain/formatting"
	"fare-brain/models"
	"fare-brain/tools"
)

// Dashboard trips exist only after WhatsApp creates them. The dashboard reads that
// document and writes choices back into it, then announces the change in the group.

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
	for _, trip := range trips {
		if trip == nil || strings.TrimSpace(trip.GroupID) == "" {
			continue
		}
		out = append(out, map[string]any{
			"group_id":    trip.GroupID,
			"group_name":  or(trip.GroupName, "WhatsApp group"),
			"destination": trip.Destination,
			"origin":      trip.Origin,
			"dates":       formatting.Dates(trip.EmbarkingDate, trip.ReturningDate),
			"state":       trip.State,
			"updated_at":  trip.UpdatedAt,
		})
	}
	return out, nil
}

func (b *Brain) DashboardView(ctx context.Context, groupID string) (map[string]any, error) {
	trip, err := b.Store.GetTrip(ctx, groupID)
	if err != nil {
		return nil, err
	}
	if trip == nil {
		return nil, &DashboardError{Status: 404, Msg: "No trip yet. Add Fare to the WhatsApp group and tag it to start."}
	}
	msgs, err := b.Store.GetMessages(ctx, groupID, nil, 80, true)
	if err != nil {
		return nil, err
	}
	return dashboardPayload(trip, msgs), nil
}

func (b *Brain) DashboardAct(ctx context.Context, groupID string, body map[string]any) (map[string]any, error) {
	trip, err := b.Store.GetTrip(ctx, groupID)
	if err != nil {
		return nil, err
	}
	if trip == nil {
		return nil, &DashboardError{Status: 404, Msg: "No trip yet. Sessions are created from WhatsApp only."}
	}
	actor := strings.TrimSpace(str(body["actor"]))
	if actor == "" {
		actor = "Someone"
	}
	action := strings.TrimSpace(str(body["action"]))
	switch action {
	case "chat":
		text := strings.TrimSpace(str(body["text"]))
		if text == "" {
			return nil, &DashboardError{Status: 400, Msg: "Message is empty."}
		}
		if err := b.say(ctx, groupID, fmt.Sprintf("%s on the dashboard: %s", actor, text), nil); err != nil {
			return nil, err
		}
		b.Handle(ctx, models.IncomingMessage{
			GroupID: groupID, GroupName: trip.GroupName,
			SenderID: "dashboard:" + actor, SenderName: actor,
			Text: text, Tagged: true,
			MessageID: fmt.Sprintf("dash_%d", time.Now().UnixNano()),
			Timestamp: time.Now().Unix(),
		})
	case "load_options":
		if err := b.loadTravelOptions(ctx, trip, actor); err != nil {
			return nil, err
		}
	case "select_flight":
		if err := b.selectDashboardFlight(ctx, trip, actor, body); err != nil {
			return nil, err
		}
	case "select_hotel":
		if err := b.selectDashboardHotel(ctx, trip, actor, body); err != nil {
			return nil, err
		}
	case "set_budget":
		if err := b.setDashboardBudget(ctx, trip, actor, str(body["budget"])); err != nil {
			return nil, err
		}
	case "set_payer":
		if err := b.setDashboardPayer(ctx, trip, actor, str(body["name"])); err != nil {
			return nil, err
		}
	case "save_traveler":
		if err := b.saveDashboardTraveler(ctx, trip, actor, body); err != nil {
			return nil, err
		}
	default:
		return nil, &DashboardError{Status: 400, Msg: "Unknown dashboard action."}
	}
	return b.DashboardView(ctx, groupID)
}

func dashboardPayload(trip *models.Trip, msgs []models.Message) map[string]any {
	spend := formatting.ComputeSpend(trip)
	itin := trip.Itinerary
	if itin == nil {
		itin = map[string]any{}
	}
	advisor := asMapAny(itin["advisor"])
	foodPerDay := numPtr(advisor["food_per_day_cad"])
	foodTrip := numPtr(advisor["food_trip_cad"])
	people := len(trip.Participants)
	if people < 1 {
		people = spend.People
	}
	payer := strings.TrimSpace(trip.PayerName)
	for _, p := range trip.Participants {
		if p.Payer && payer == "" {
			payer = p.WhatsAppName
		}
	}
	var owes []map[string]any
	if payer != "" && spend.Ok() {
		for _, p := range trip.Participants {
			if strings.EqualFold(p.WhatsAppName, payer) {
				continue
			}
			owes = append(owes, map[string]any{
				"from": p.WhatsAppName, "to": payer, "amount": spend.TravelEach,
			})
		}
	}
	return map[string]any{
		"group_id": trip.GroupID,
		"group_name": or(trip.GroupName, "WhatsApp group"),
		"state": trip.State,
		"editable": trip.State != models.BookingState && trip.State != models.Booked,
		"destination": trip.Destination,
		"origin": trip.Origin,
		"embarking_date": trip.EmbarkingDate,
		"returning_date": trip.ReturningDate,
		"dates": formatting.Dates(trip.EmbarkingDate, trip.ReturningDate),
		"nights": trip.DurationNights,
		"budget_note": trip.BudgetNote,
		"payer_name": payer,
		"updated_at": trip.UpdatedAt,
		"spend": map[string]any{
			"flight_each": spend.FlightEach,
			"hotel_group": spend.HotelGroup,
			"hotel_each": spend.HotelEach,
			"travel_each": spend.TravelEach,
			"travel_group": spend.TravelGroup,
			"people": people,
			"includes_food": false,
			"food_note": str(advisor["food_note"]),
			"food_per_day": foodPerDay,
			"food_trip": foodTrip,
		},
		"owes": owes,
		"people": dashPeople(trip),
		"days": dashDays(advisor),
		"places": dashPlaces(asMapAny(itin["restaurants"]), trip.Destination),
		"flights": dashFlights(itin),
		"hotels": dashHotels(itin),
		"messages": dashMessages(msgs),
	}
}

func (b *Brain) ensureEditable(trip *models.Trip) error {
	if trip.State == models.BookingState || trip.State == models.Booked {
		return &DashboardError{Status: 409, Msg: "This trip is already booked. Choices are locked."}
	}
	return nil
}

func (b *Brain) loadTravelOptions(ctx context.Context, trip *models.Trip, actor string) error {
	if b.Config == nil || !b.Config.MockTravel {
		return &DashboardError{Status: 400, Msg: "Live searches start from the WhatsApp group after a destination is chosen. The dashboard only observes and can pick among saved offers."}
	}
	if err := b.ensureEditable(trip); err != nil {
		return err
	}
	dest := tripDestination(trip)
	if dest == "" || trip.EmbarkingDate == "" || trip.ReturningDate == "" {
		return &DashboardError{Status: 400, Msg: "Need a destination and dates from the WhatsApp chat before I can list flights and hotels."}
	}
	origin := trip.Origin
	if len(trip.Participants) > 0 && trip.Participants[0].OriginAirport != "" {
		origin = trip.Participants[0].OriginAirport
	}
	airport := trip.DestinationAirport
	if airport == "" {
		if o := trip.ChosenOption(); o != nil {
			airport = o.DestinationAirport
		}
	}
	var flights []models.FlightOffer
	for _, ap := range destAirportCandidates(airport, dest) {
		got, err := tools.SearchFlights(ctx, b.Config, origin, ap, trip.EmbarkingDate, trip.ReturningDate)
		if err != nil {
			continue
		}
		flights = append(flights, got...)
	}
	guests := len(trip.Participants)
	if guests < 1 {
		guests = 2
	}
	hotels, err := tools.SearchHotels(ctx, b.Config, dest, trip.EmbarkingDate, trip.ReturningDate, guests, nil)
	if err != nil {
		return err
	}
	if len(flights) == 0 && len(hotels) == 0 {
		return &DashboardError{Status: 404, Msg: "No flights or hotels came back for those dates."}
	}
	itin := cloneMap(trip.Itinerary)
	if len(flights) > 0 {
		itin["flight_options"] = flights
	}
	if len(hotels) > 0 {
		itin["hotel_options"] = hotels
	}
	mapped, err := structToMap(itin)
	if err != nil {
		return err
	}
	if _, err := b.Store.UpdateTrip(ctx, trip.ID, map[string]any{"itinerary": mapped}); err != nil {
		return err
	}
	return b.say(ctx, trip.GroupID, fmt.Sprintf("%s opened flight and hotel options on the dashboard. Nothing is booked until the group picks.", actor), nil)
}

func (b *Brain) selectDashboardFlight(ctx context.Context, trip *models.Trip, actor string, body map[string]any) error {
	if err := b.ensureEditable(trip); err != nil {
		return err
	}
	offerID := str(body["offer_id"])
	offer, ok := findOffer(trip.Itinerary, "flight_options", offerID)
	if !ok {
		offer = map[string]any{
			"offer_id": offerID, "airline": str(body["airline"]), "origin": str(body["origin"]),
			"destination": str(body["destination"]), "summary": str(body["summary"]), "price": num(body["price"]),
		}
		if str(offer["airline"]) == "" && num(offer["price"]) == 0 {
			return &DashboardError{Status: 404, Msg: "That flight isn't in the current list."}
		}
	}
	price := num(offer["price"])
	embark := cloneMap(offer)
	ret := cloneMap(offer)
	ret["origin"], ret["destination"] = offer["destination"], offer["origin"]
	itin := cloneMap(trip.Itinerary)
	flights := asMapAny(itin["flights"])
	flights["round_trip_each"] = price
	flights["embarking"] = embark
	flights["returning"] = ret
	itin["flights"] = flights
	itin["selected_flight_id"] = or(offerID, str(offer["offer_id"]))
	return b.persistChoice(ctx, trip, itin, fmt.Sprintf("%s picked %s %s → %s on the dashboard. Round trip %s each. Food stays out of that.",
		actor, or(str(offer["airline"]), "the flight"), str(offer["origin"]), str(offer["destination"]), money(price)))
}

func (b *Brain) selectDashboardHotel(ctx context.Context, trip *models.Trip, actor string, body map[string]any) error {
	if err := b.ensureEditable(trip); err != nil {
		return err
	}
	offerID := str(body["offer_id"])
	offer, ok := findOffer(trip.Itinerary, "hotel_options", offerID)
	if !ok {
		total := num(body["total"])
		if total == 0 {
			total = num(body["price"])
		}
		offer = map[string]any{
			"offer_id": offerID, "name": str(body["name"]), "city": str(body["city"]),
			"price_per_night": num(body["nightly"]), "total_price": total, "rating": numPtr(body["rating"]),
		}
		if str(offer["name"]) == "" {
			return &DashboardError{Status: 404, Msg: "That stay isn't in the current list."}
		}
	}
	itin := cloneMap(trip.Itinerary)
	itin["hotel"] = map[string]any{
		"offer_id": or(str(offer["offer_id"]), offerID), "name": offer["name"], "city": offer["city"],
		"price_per_night": offer["price_per_night"], "total_price": offer["total_price"],
		"rating": offer["rating"], "image_url": offer["image_url"],
	}
	itin["selected_hotel_id"] = or(offerID, str(offer["offer_id"]))
	return b.persistChoice(ctx, trip, itin, fmt.Sprintf("%s picked %s on the dashboard. %s for the group. The WhatsApp plan now uses that stay.",
		actor, str(offer["name"]), money(num(offer["total_price"]))))
}

func (b *Brain) persistChoice(ctx context.Context, trip *models.Trip, itin map[string]any, announce string) error {
	mapped, err := structToMap(itin)
	if err != nil {
		return err
	}
	people := trip.Participants
	spend := formatting.ComputeSpend(&models.Trip{Itinerary: mapped, Participants: people})
	fields := map[string]any{"itinerary": mapped, "flights_locked": true}
	if spend.Ok() {
		fields["cost_per_person"] = spend.TravelEach
		mapped["group_total"] = spend.TravelGroup
	}
	if _, err := b.Store.UpdateTrip(ctx, trip.ID, fields); err != nil {
		return err
	}
	return b.say(ctx, trip.GroupID, announce, nil)
}

func (b *Brain) setDashboardBudget(ctx context.Context, trip *models.Trip, actor, budget string) error {
	budget = strings.TrimSpace(budget)
	if budget == "" {
		return &DashboardError{Status: 400, Msg: "Budget is empty."}
	}
	if _, err := b.Store.UpdateTrip(ctx, trip.ID, map[string]any{"budget_note": budget}); err != nil {
		return err
	}
	return b.say(ctx, trip.GroupID, fmt.Sprintf("%s set the planning budget to %s CAD each on the dashboard. That's a guide — the locked number is still flights plus hotel, and food is extra.", actor, budget), nil)
}

func (b *Brain) setDashboardPayer(ctx context.Context, trip *models.Trip, actor, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return &DashboardError{Status: 400, Msg: "Pick who is paying."}
	}
	people := append([]models.Participant(nil), trip.Participants...)
	found := false
	for i := range people {
		people[i].Payer = strings.EqualFold(people[i].WhatsAppName, name)
		if people[i].Payer {
			found = true
			name = people[i].WhatsAppName
		}
	}
	fields := map[string]any{"payer_name": name, "asked_payer": true}
	if found {
		fields["participants"] = people
	}
	if _, err := b.Store.UpdateTrip(ctx, trip.ID, fields); err != nil {
		return err
	}
	return b.say(ctx, trip.GroupID, fmt.Sprintf("%s set %s as the card on the dashboard. Everyone else owes them their flights-and-hotel share.", actor, name), nil)
}

func (b *Brain) saveDashboardTraveler(ctx context.Context, trip *models.Trip, actor string, body map[string]any) error {
	name := strings.TrimSpace(str(body["name"]))
	legal := strings.TrimSpace(str(body["legal_name"]))
	dob := strings.TrimSpace(str(body["date_of_birth"]))
	passport := strings.TrimSpace(str(body["passport_number"]))
	if name == "" || legal == "" || dob == "" || passport == "" {
		return &DashboardError{Status: 400, Msg: "Legal name, date of birth, and passport number are required."}
	}
	if _, err := time.Parse("2006-01-02", dob); err != nil {
		return &DashboardError{Status: 400, Msg: "Date of birth should be YYYY-MM-DD."}
	}
	people := append([]models.Participant(nil), trip.Participants...)
	hit := -1
	for i := range people {
		if strings.EqualFold(people[i].WhatsAppName, name) {
			hit = i
			break
		}
	}
	if hit < 0 {
		people = append(people, models.Participant{WhatsAppName: name, PID: fmt.Sprintf("p_dash_%d", time.Now().UnixNano())})
		hit = len(people) - 1
	}
	people[hit].LegalName = legal
	people[hit].DateOfBirth = dob
	people[hit].PassportNumber = passport
	people[hit].UploadedPassportInfo = true
	if _, err := b.Store.UpdateTrip(ctx, trip.ID, map[string]any{"participants": people}); err != nil {
		return err
	}
	return b.say(ctx, trip.GroupID, fmt.Sprintf("%s saved travel documents for %s on the dashboard. The passport number stays off this chat.", actor, people[hit].WhatsAppName), nil)
}

func dashPeople(trip *models.Trip) []map[string]any {
	out := make([]map[string]any, 0, len(trip.Participants))
	for _, p := range trip.Participants {
		out = append(out, map[string]any{
			"name": p.WhatsAppName,
			"payer": p.Payer || strings.EqualFold(p.WhatsAppName, trip.PayerName),
			"legal_name": p.LegalName,
			"date_of_birth": p.DateOfBirth,
			"has_passport": p.PassportNumber != "",
			"passport_last4": last4(p.PassportNumber),
			"origin": or(p.OriginAirport, p.OriginCity),
		})
	}
	return out
}

func dashDays(advisor map[string]any) []map[string]any {
	raw, _ := sliceAny(advisor["days"])
	out := make([]map[string]any, 0, len(raw))
	for i, item := range raw {
		d := asMapAny(item)
		title := str(d["title"])
		if title == "" {
			title = fmt.Sprintf("Day %d", i+1)
		}
		out = append(out, map[string]any{"title": title, "body": str(d["body"]), "food_cad": numPtr(d["food_cad"])})
	}
	return out
}

func dashPlaces(rest map[string]any, city string) []map[string]any {
	raw, _ := sliceAny(rest["places"])
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		p := asMapAny(item)
		name := str(p["name"])
		if name == "" {
			continue
		}
		q := strings.TrimSpace(name + " " + str(p["neighborhood"]) + " " + city)
		out = append(out, map[string]any{
			"name": name, "neighborhood": str(p["neighborhood"]), "why": str(p["why"]),
			"dish": str(p["dish"]), "est_cad": numPtr(p["est_cad"]),
			"map": "https://www.google.com/maps/search/?api=1&query=" + url.QueryEscape(q),
		})
	}
	return out
}

func dashFlights(itin map[string]any) []map[string]any {
	selected := str(itin["selected_flight_id"])
	lockedPrice := num(asMapAny(itin["flights"])["round_trip_each"])
	raw, _ := sliceAny(itin["flight_options"])
	if len(raw) == 0 {
		flights := asMapAny(itin["flights"])
		out := asMapAny(flights["embarking"])
		if str(out["airline"]) != "" || num(flights["round_trip_each"]) > 0 {
			price := num(flights["round_trip_each"])
			if price == 0 {
				price = num(out["price"])
			}
			return []map[string]any{{
				"offer_id": or(str(out["offer_id"]), "locked"),
				"airline": str(out["airline"]), "origin": str(out["origin"]), "destination": str(out["destination"]),
				"summary": str(out["summary"]), "price": price, "selected": true,
			}}
		}
	}
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		f := asMapAny(item)
		id := str(f["offer_id"])
		out = append(out, map[string]any{
			"offer_id": id, "airline": str(f["airline"]), "origin": str(f["origin"]),
			"destination": str(f["destination"]), "summary": str(f["summary"]),
			"depart_date": str(f["depart_date"]), "return_date": str(f["return_date"]),
			"price": num(f["price"]),
			"selected": id != "" && id == selected || (selected == "" && lockedPrice > 0 && num(f["price"]) == lockedPrice),
		})
	}
	return out
}

func dashHotels(itin map[string]any) []map[string]any {
	selected := str(itin["selected_hotel_id"])
	current := str(asMapAny(itin["hotel"])["name"])
	raw, _ := sliceAny(itin["hotel_options"])
	if len(raw) == 0 {
		h := asMapAny(itin["hotel"])
		if str(h["name"]) == "" {
			return nil
		}
		return []map[string]any{{
			"offer_id": or(str(h["offer_id"]), "locked"),
			"name": str(h["name"]), "city": str(h["city"]),
			"nightly": num(h["price_per_night"]), "total": num(h["total_price"]),
			"rating": numPtr(h["rating"]), "image": str(h["image_url"]), "selected": true,
		}}
	}
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		h := asMapAny(item)
		id := str(h["offer_id"])
		name := str(h["name"])
		picked := (id != "" && id == selected) || (selected == "" && current != "" && strings.EqualFold(name, current))
		out = append(out, map[string]any{
			"offer_id": id, "name": name, "city": str(h["city"]),
			"nightly": num(h["price_per_night"]), "total": num(h["total_price"]),
			"rating": numPtr(h["rating"]), "image": str(h["image_url"]), "selected": picked,
		})
	}
	return out
}

func dashMessages(msgs []models.Message) []map[string]any {
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, map[string]any{
			"id": m.ID, "sender": m.SenderName, "text": m.Text, "bot": m.IsBot,
			"at": m.SentAt.Format(time.RFC3339),
		})
	}
	return out
}

func findOffer(itin map[string]any, key, id string) (map[string]any, bool) {
	if itin == nil || id == "" {
		return nil, false
	}
	raw, _ := sliceAny(itin[key])
	for _, item := range raw {
		m := asMapAny(item)
		if str(m["offer_id"]) == id {
			return m, true
		}
	}
	return nil, false
}

func sliceAny(v any) ([]any, bool) {
	switch t := v.(type) {
	case []any:
		return t, true
	case nil:
		return nil, false
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			return nil, false
		}
		var out []any
		if json.Unmarshal(raw, &out) != nil {
			return nil, false
		}
		return out, true
	}
}

func cloneMap(in map[string]any) map[string]any {
	if in == nil {
		return map[string]any{}
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func asMapAny(v any) map[string]any {
	if v == nil {
		return map[string]any{}
	}
	if m, ok := v.(map[string]any); ok {
		return m
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return map[string]any{}
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return map[string]any{}
	}
	return m
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func num(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int32:
		return float64(n)
	case int64:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	case string:
		f, _ := strconv.ParseFloat(strings.TrimSpace(n), 64)
		return f
	default:
		return 0
	}
}

func numPtr(v any) any {
	if v == nil {
		return nil
	}
	n := num(v)
	if n == 0 {
		return nil
	}
	return n
}

func money(n float64) string { return formatting.Money(&n) }

func or(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

func last4(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= 4 {
		if s == "" {
			return ""
		}
		return s
	}
	return s[len(s)-4:]
}
