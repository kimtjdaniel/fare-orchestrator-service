package orchestrator

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"fare-brain/formatting"
	"fare-brain/models"
	"fare-brain/tools"
)

func (b *Brain) sendHotelPhoto(ctx context.Context, trip *models.Trip) error {
	hotel, err := b.hotelForTrip(ctx, trip)
	if err != nil || hotel == nil || strings.TrimSpace(hotel.Name) == "" {
		return b.say(ctx, trip.GroupID, "We haven't locked a stay yet. Once a city's picked I can send the pin.", nil)
	}
	night := hotel.PricePerNight
	caption := fmt.Sprintf("%s in %s. About %s a night, CAD.", hotel.Name, hotel.City, formatting.Money(&night))
	if err := b.say(ctx, trip.GroupID, caption, nil); err != nil {
		return err
	}
	query := strings.TrimSpace(hotel.Name + " " + hotel.City)
	maps := "https://www.google.com/maps/search/?api=1&query=" + url.QueryEscape(query)
	return b.say(ctx, trip.GroupID, "Map: "+maps, nil)
}

func (b *Brain) hotelForTrip(ctx context.Context, trip *models.Trip) (*models.HotelOffer, error) {
	if trip.Itinerary != nil {
		if raw, ok := trip.Itinerary["hotel"]; ok && raw != nil {
			var hotel models.HotelOffer
			if err := decodeInto(raw, &hotel); err == nil && hotel.Name != "" {
				return &hotel, nil
			}
		}
	}
	city := trip.Destination
	checkIn, checkOut := trip.EmbarkingDate, trip.ReturningDate
	if city == "" && len(trip.Options) > 0 {
		city = trip.Options[0].Destination
		checkIn = trip.Options[0].EmbarkingDate
		checkOut = trip.Options[0].ReturningDate
	}
	if city == "" || checkIn == "" || checkOut == "" {
		return nil, nil
	}
	guests := len(trip.Participants)
	if guests < 1 {
		guests = 2
	}
	offers, err := tools.SearchHotels(ctx, b.Config, city, checkIn, checkOut, guests, nil)
	if err != nil || len(offers) == 0 {
		return nil, err
	}
	h := offers[0]
	return &h, nil
}
