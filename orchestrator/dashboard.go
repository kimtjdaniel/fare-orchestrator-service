package orchestrator

import (
	"context"
	"encoding/json"
	"fare-brain/llm"
	"fare-brain/models"
	"fare-brain/prompts"
	"fare-brain/store"
	"fare-brain/tools"
	"fmt"
	"strings"
)

func (b *Brain) dashboardEvent(ctx context.Context, t *models.Trip, kind string, payload map[string]any) error {
	if b.Dashboard == nil {
		return nil
	}
	return b.Dashboard.Emit(ctx, t.GroupID, kind, payload)
}
func (b *Brain) dashboardTask(ctx context.Context, t *models.Trip, id, status, message string) error {
	return b.dashboardEvent(ctx, t, "planning.task.updated", map[string]any{"taskId": id, "status": status, "message": message})
}
func dashboardFlights(offers []models.FlightOffer) []map[string]any {
	rows := make([]map[string]any, 0, len(offers))
	for _, f := range offers {
		stops := "Direct"
		if f.Stops > 0 {
			stops = fmt.Sprintf("%d stops", f.Stops)
		}
		duration := f.Duration
		if duration == "" {
			duration = "Duration unavailable"
		}
		rows = append(rows, map[string]any{"id": f.OfferID, "source": f.Source, "airline": f.Airline, "route": f.Origin + " → " + f.Destination, "price": f.Price, "currency": f.Currency, "duration": duration, "stops": stops, "departureTime": f.DepartureTime, "arrivalTime": f.ArrivalTime})
	}
	return rows
}
func dashboardHotels(offers []models.HotelOffer, adults int, mock bool) []map[string]any {
	if adults < 1 {
		adults = 1
	}
	rows := make([]map[string]any, 0, len(offers))
	for _, h := range offers {
		start, _ := models.ParseDate(h.CheckIn)
		end, _ := models.ParseDate(h.CheckOut)
		var rating any
		if h.Rating != nil {
			rating = *h.Rating
			if mock {
				rating = *h.Rating * 2
			}
		}
		rows = append(rows, map[string]any{"id": h.OfferID, "name": h.Name, "neighborhood": h.City, "rating": rating, "price": h.TotalPrice, "currency": h.Currency, "nights": int(end.Sub(start).Hours() / 24), "adults": adults, "url": h.CheckoutURL, "source": h.Source, "propertyType": h.PropertyType, "originalRating": h.OriginalRating, "originalRatingScale": h.OriginalRatingScale, "priceNote": h.PriceNote})
	}
	return rows
}
func (b *Brain) finishDashboard(ctx context.Context, t *models.Trip, flight models.FlightOffer, hotel models.HotelOffer) error {
	if err := b.dashboardTask(ctx, t, "group-budget", "completed", "Calculated the per-person flight and hotel costs"); err != nil {
		return err
	}
	if err := b.dashboardTask(ctx, t, "daily-schedule", "running", "Planning each day around your group’s interests"); err != nil {
		return err
	}
	facts, _ := json.Marshal(tripFacts(t))
	selected, err := json.Marshal(map[string]any{"flight": flight, "hotel": hotel, "selection_reason": t.Itinerary["selection_reason"], "per_person": t.Itinerary["per_person"]})
	if err != nil {
		return err
	}
	user := fmt.Sprintf("Destination: %s\nStart date: %s\nEnd date: %s\nDays: %d\nGroup preferences: %s\nBudget note: %s\nLOCKED FACTS: %s\nInclude local timed activities from 08:00 to 21:00 for each full day, leaving sensible free time and respecting arrival timing. Do not invent travel prices.", t.Destination, t.EmbarkingDate, t.ReturningDate, t.DurationNights, formatKnownPrefs(t.Participants), t.BudgetNote, string(facts))
	user += "\nSelected travel plan: " + string(selected) + "\nBuild activities around these selected offers and the group's interests. Account for flight arrival, hotel check-in, transfers and departure. Include every date through the return date, using partial schedules on travel days. Activity suggestions are estimates, not confirmed bookings or verified opening hours."
	if flight.OfferID == "skipped" {
		user += "\nFlights were skipped. Do not invent flights or fares. Plan each day around the hotel."
	}
	out, err := b.LLM.Structured(ctx, prompts.ItinerarySystem(b.Config.BotName, b.today().Format("2006-01-02")), []llm.Message{{Role: "user", Content: user}}, toSchema(prompts.DayItinerary))
	if err != nil {
		return err
	}
	days := []map[string]any{}
	start, _ := models.ParseDate(t.EmbarkingDate)
	rawDays, _ := out["days"].([]any)
	for index, raw := range rawDays {
		day, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		activities := day["activities"]
		if activities == nil {
			activities = []any{}
		}
		days = append(days, map[string]any{"date": start.AddDate(0, 0, index).Format("2006-01-02"), "title": day["title"], "description": day["body"], "activities": activities})
	}
	if len(days) == 0 {
		return fmt.Errorf("itinerary generation returned no days")
	}
	guests := len(t.Participants)
	if guests < 1 {
		guests = 1
	}
	explanation, _ := out["intro"].(string)
	plan := map[string]any{"flight": flight.Airline, "route": flight.Origin + " → " + flight.Destination, "flightPrice": flight.Price, "flightSource": flight.Source, "hotel": hotel.Name, "nights": t.DurationNights, "hotelPrice": hotel.TotalPrice / float64(guests), "explanation": explanation, "selectionReason": t.Itinerary["selection_reason"], "flightOfferId": flight.OfferID, "hotelOfferId": hotel.OfferID, "hotelSource": hotel.Source, "hotelPropertyType": hotel.PropertyType, "hotelOriginalRating": hotel.OriginalRating, "hotelOriginalRatingScale": hotel.OriginalRatingScale, "hotelPriceNote": hotel.PriceNote, "days": days, "isSampleSchedule": b.Config.MockLLM}
	itinerary := map[string]any{}
	for key, value := range t.Itinerary {
		itinerary[key] = value
	}
	itinerary["advisor"] = out
	itinerary["dashboard_plan"] = plan
	if _, err = b.Store.UpdateTrip(ctx, t.ID, map[string]any{"itinerary": itinerary}); err != nil {
		return err
	}
	if err = b.dashboardTask(ctx, t, "daily-schedule", "completed", "Your day-by-day itinerary is ready"); err != nil {
		return err
	}
	if err = b.dashboardEvent(ctx, t, "planning.completed", map[string]any{"plan": plan, "message": "Itinerary generated and saved"}); err != nil {
		return err
	}
	return b.dashboardEvent(ctx, t, "session.completed", map[string]any{"message": "Your trip plan is ready for group approval"})
}

func (b *Brain) searchDashboard(ctx context.Context, t *models.Trip, origin string, option *models.Option) ([]models.FlightOffer, []models.HotelOffer, error, error) {
	if b.Dashboard != nil {
		sessionID, err := b.Dashboard.Begin(ctx, t)
		if err != nil {
			return nil, nil, err, err
		}
		ctx = tools.WithSearchEvents(ctx, sessionID, func(kind string, payload map[string]any) { _ = b.dashboardEvent(ctx, t, kind, payload) })
	}
	flightDone := make(chan struct{})
	hotelDone := make(chan struct{})
	var flights []models.FlightOffer
	var hotels []models.HotelOffer
	var flightErr, hotelErr error
	destAP := option.DestinationAirport
	if destAP == "" {
		if cands := destAirportCandidates("", option.Destination); len(cands) > 0 {
			destAP = cands[0]
		} else {
			destAP = option.Destination
		}
	}
	go func() {
		defer close(flightDone)
		if !validIATA(origin) || !validIATA(destAP) || !isoDateOK(option.EmbarkingDate) || !isoDateOK(option.ReturningDate) {
			flightErr = fmt.Errorf("flight search needs airport codes and dates (origin %q, destination %q, %s to %s)", origin, destAP, option.EmbarkingDate, option.ReturningDate)
			_ = b.dashboardEvent(ctx, t, "flight_search.failed", map[string]any{"message": flightErr.Error()})
			return
		}
		if err := b.dashboardEvent(ctx, t, "flight_search.started", map[string]any{"message": "Flight agent started"}); err != nil {
			flightErr = err
			return
		}
		flights, flightErr = tools.SearchFlights(ctx, b.Config, origin, destAP, option.EmbarkingDate, option.ReturningDate)
		if flightErr != nil {
			_ = b.dashboardEvent(ctx, t, "flight_search.failed", map[string]any{"message": flightErr.Error()})
		}
		if flightErr == nil && len(flights) > 0 {
			flightErr = b.dashboardEvent(ctx, t, "flight_search.completed", map[string]any{"flights": dashboardFlights(flights), "message": fmt.Sprintf("Found %d flight options", len(flights))})
		}
	}()
	go func() {
		defer close(hotelDone)
		if err := b.dashboardEvent(ctx, t, "hotel_search.started", map[string]any{"message": "Hotel agent started"}); err != nil {
			hotelErr = err
			return
		}
		hotels, hotelErr = tools.SearchHotels(ctx, b.Config, option.Destination, option.EmbarkingDate, option.ReturningDate, len(t.Participants), nil)
		if hotelErr != nil {
			_ = b.dashboardEvent(ctx, t, "hotel_search.failed", map[string]any{"message": hotelErr.Error()})
		}
		if hotelErr == nil && len(hotels) > 0 {
			hotelErr = b.dashboardEvent(ctx, t, "hotel_search.completed", map[string]any{"hotels": dashboardHotels(hotels, len(t.Participants), b.Config.MockTravel), "message": fmt.Sprintf("Found %d stays", len(hotels))})
		}
	}()
	<-flightDone
	<-hotelDone
	return flights, hotels, flightErr, hotelErr
}

func validIATA(code string) bool {
	if len(code) != 3 {
		return false
	}
	for _, r := range code {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}

// RetryFlightSearch runs the flight agent again for the group's current trip.
func (b *Brain) RetryFlightSearch(ctx context.Context, groupID string) error {
	trip, err := b.Store.GetTrip(ctx, groupID)
	if err != nil || trip == nil {
		return &DashboardError{Status: 404, Msg: "trip not found"}
	}
	option := trip.ChosenOption()
	if option == nil && len(trip.Options) > 0 {
		option = &trip.Options[0]
	}
	if option == nil {
		return &DashboardError{Status: 400, Msg: "There is no destination to search yet."}
	}
	origin := originAirportCode(trip.Origin, trip.Participants)
	dest := option.DestinationAirport
	if cands := destAirportCandidates(dest, option.Destination); len(cands) > 0 {
		dest = cands[0]
	} else if cands := destAirportCandidates("", trip.Destination); len(cands) > 0 {
		dest = cands[0]
	}
	if !validIATA(origin) || !validIATA(dest) || !isoDateOK(option.EmbarkingDate) || !isoDateOK(option.ReturningDate) {
		msg := fmt.Sprintf("Flight search needs airport codes and dates. Origin is %q, destination is %q, dates are %s to %s.", blank(origin), blank(dest), blank(option.EmbarkingDate), blank(option.ReturningDate))
		_ = b.dashboardEvent(ctx, trip, "flight_search.failed", map[string]any{"message": msg})
		return &DashboardError{Status: 400, Msg: msg}
	}
	go b.runFlightSearch(context.Background(), trip, origin, dest, option.EmbarkingDate, option.ReturningDate)
	return nil
}

func blank(s string) string {
	if strings.TrimSpace(s) == "" {
		return "missing"
	}
	return s
}

func (b *Brain) runFlightSearch(ctx context.Context, t *models.Trip, origin, dest, depart, ret string) {
	if b.Dashboard != nil {
		if sessionID, err := b.Dashboard.Begin(ctx, t); err == nil {
			ctx = tools.WithSearchEvents(ctx, sessionID, func(kind string, payload map[string]any) { _ = b.dashboardEvent(ctx, t, kind, payload) })
		}
	}
	if err := b.dashboardEvent(ctx, t, "flight_search.started", map[string]any{"message": "Retrying the flight search."}); err != nil {
		return
	}
	flights, err := tools.SearchFlights(ctx, b.Config, origin, dest, depart, ret)
	if err != nil {
		_ = b.dashboardEvent(ctx, t, "flight_search.failed", map[string]any{"message": err.Error()})
		return
	}
	if len(flights) == 0 {
		_ = b.dashboardEvent(ctx, t, "flight_search.failed", map[string]any{"message": "The flight search finished without any fares."})
		return
	}
	_ = b.dashboardEvent(ctx, t, "flight_search.completed", map[string]any{"flights": dashboardFlights(flights), "message": fmt.Sprintf("Found %d flight options", len(flights))})
	fresh, err := b.Store.GetTrip(ctx, t.GroupID)
	if err != nil || fresh == nil {
		return
	}
	hotels := pendingHotels(fresh)
	if len(hotels) == 0 {
		return
	}
	option := fresh.ChosenOption()
	if option == nil && len(fresh.Options) > 0 {
		option = &fresh.Options[0]
	}
	if option == nil {
		return
	}
	_ = b.lockSearchedPlan(ctx, fresh, option, fresh.Participants, flights, hotels, false)
}

func (b *Brain) rememberPendingHotels(ctx context.Context, trip *models.Trip, hotels []models.HotelOffer) error {
	itin := map[string]any{}
	for k, v := range trip.Itinerary {
		itin[k] = v
	}
	raw, err := json.Marshal(hotels)
	if err != nil {
		return err
	}
	var rows []any
	if err := json.Unmarshal(raw, &rows); err != nil {
		return err
	}
	itin["pending_hotels"] = rows
	_, err = b.Store.UpdateTrip(ctx, trip.ID, map[string]any{"itinerary": itin})
	return err
}

func pendingHotels(trip *models.Trip) []models.HotelOffer {
	if trip == nil || trip.Itinerary == nil {
		return nil
	}
	raw, ok := trip.Itinerary["pending_hotels"]
	if !ok || raw == nil {
		return nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var hotels []models.HotelOffer
	if json.Unmarshal(encoded, &hotels) != nil {
		return nil
	}
	return hotels
}

func cheapestHotel(hotels []models.HotelOffer) models.HotelOffer {
	best := hotels[0]
	for _, hotel := range hotels[1:] {
		if hotel.TotalPrice > 0 && (best.TotalPrice == 0 || hotel.TotalPrice < best.TotalPrice) {
			best = hotel
		}
	}
	return best
}

// SkipFlights keeps the saved stays and continues the itinerary without a fare.
func (b *Brain) SkipFlights(ctx context.Context, groupID string) error {
	trip, err := b.Store.GetTrip(ctx, groupID)
	if err != nil || trip == nil {
		return &DashboardError{Status: 404, Msg: "trip not found"}
	}
	hotels := pendingHotels(trip)
	if len(hotels) == 0 {
		return &DashboardError{Status: 400, Msg: "There is no saved stay to continue with."}
	}
	option := trip.ChosenOption()
	if option == nil && len(trip.Options) > 0 {
		option = &trip.Options[0]
	}
	if option == nil {
		return &DashboardError{Status: 400, Msg: "There is no destination to plan yet."}
	}
	go b.continueWithoutFlights(context.Background(), trip.GroupID)
	return nil
}

func (b *Brain) continueWithoutFlights(ctx context.Context, groupID string) {
	trip, err := b.Store.GetTrip(ctx, groupID)
	if err != nil || trip == nil {
		return
	}
	if trip.State == models.AwaitingChoice {
		trip, err = store.SetState(ctx, b.Store, trip.ID, models.Searching, nil)
		if err != nil || trip == nil {
			return
		}
	}
	hotels := pendingHotels(trip)
	option := trip.ChosenOption()
	if option == nil && len(trip.Options) > 0 {
		option = &trip.Options[0]
	}
	if option == nil || len(hotels) == 0 {
		return
	}
	_ = b.dashboardEvent(ctx, trip, "flight_search.completed", map[string]any{"flights": []map[string]any{}, "message": "Flights skipped. Planning continues with the stay."})
	if err := b.lockSearchedPlan(ctx, trip, option, trip.Participants, nil, hotels, true); err != nil {
		_ = b.say(ctx, trip.GroupID, err.Error(), nil)
	}
}
