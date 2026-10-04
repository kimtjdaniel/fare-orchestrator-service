package orchestrator

import (
	"context"
	"encoding/json"
	"fare-brain/llm"
	"fare-brain/models"
	"fare-brain/prompts"
	"fare-brain/tools"
	"fmt"
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
		rows = append(rows, map[string]any{"id": f.OfferID, "airline": f.Airline, "route": f.Origin + " → " + f.Destination, "price": f.Price, "currency": f.Currency, "duration": duration, "stops": stops, "departureTime": f.DepartureTime, "arrivalTime": f.ArrivalTime})
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
		rows = append(rows, map[string]any{"id": h.OfferID, "name": h.Name, "neighborhood": h.City, "rating": rating, "price": h.TotalPrice, "currency": h.Currency, "nights": int(end.Sub(start).Hours() / 24), "adults": adults, "url": h.CheckoutURL})
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
	plan := map[string]any{"flight": flight.Airline, "route": flight.Origin + " → " + flight.Destination, "flightPrice": flight.Price, "hotel": hotel.Name, "nights": t.DurationNights, "hotelPrice": hotel.TotalPrice / float64(guests), "explanation": explanation, "selectionReason": t.Itinerary["selection_reason"], "flightOfferId": flight.OfferID, "hotelOfferId": hotel.OfferID, "days": days, "isSampleSchedule": b.Config.MockLLM}
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
	go func() {
		defer close(flightDone)
		if err := b.dashboardEvent(ctx, t, "flight_search.started", map[string]any{"message": "Flight agent started"}); err != nil {
			flightErr = err
			return
		}
		flights, flightErr = tools.SearchFlights(ctx, b.Config, origin, option.DestinationAirport, option.EmbarkingDate, option.ReturningDate)
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
