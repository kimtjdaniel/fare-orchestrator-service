package orchestrator

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"fare-brain/formatting"
	"fare-brain/models"
)

func (b *Brain) sendHotelMap(ctx context.Context, trip *models.Trip) error {
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
	// Reading or sharing a map must not launch a new search or choose a stay.
	return nil, nil
}
