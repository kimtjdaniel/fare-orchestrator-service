package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"fare-brain/formatting"
	"fare-brain/messaging"
	"fare-brain/models"
	"fare-brain/tools"
)

func (b *Brain) sendHotelPhoto(ctx context.Context, trip *models.Trip) error {
	hotel, err := b.hotelForTrip(ctx, trip)
	if err != nil {
		slog.Warn("hotel lookup failed", "err", err)
	}
	if hotel == nil || strings.TrimSpace(hotel.Name) == "" {
		return b.say(ctx, trip.GroupID, "We haven't locked a stay yet. Once a city's picked I can send the hotel.", nil)
	}
	night := hotel.PricePerNight
	caption := fmt.Sprintf("%s in %s. About %s a night, CAD.", hotel.Name, hotel.City, formatting.Money(&night))
	if strings.TrimSpace(hotel.ImageURL) == "" {
		return b.say(ctx, trip.GroupID, caption, nil)
	}
	if err := b.Messenger.SendMedia(ctx, trip.GroupID, caption, messaging.Media{URL: hotel.ImageURL, Filename: "hotel.jpg"}); err != nil {
		slog.Warn("hotel photo send failed", "err", err)
		return b.say(ctx, trip.GroupID, caption, nil)
	}
	_, err = b.Store.SaveMessage(ctx, &models.Message{
		GroupID: trip.GroupID, TripID: trip.ID, SenderID: "bot", SenderName: b.Config.BotName,
		Text: caption, IsBot: true, SentAt: models.Now(),
	})
	return err
}

func (b *Brain) hotelForTrip(ctx context.Context, trip *models.Trip) (*models.HotelOffer, error) {
	if trip.Itinerary != nil {
		if raw, ok := trip.Itinerary["hotel"]; ok && raw != nil {
			var hotel models.HotelOffer
			if err := decodeInto(raw, &hotel); err == nil && hotel.Name != "" {
				if hotel.ImageURL == "" {
					hotel.ImageURL = hotelImageForName(hotel.Name, hotel.City)
				}
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

func hotelImageForName(name, city string) string {
	low := strings.ToLower(name + " " + city)
	switch {
	case strings.Contains(low, "grand"):
		return "https://images.unsplash.com/photo-1542314831-068cd1dbfeeb?auto=format&fit=crop&w=1200&q=80"
	case strings.Contains(low, "casa"):
		return "https://images.unsplash.com/photo-1520250497591-112f2f40a3f4?auto=format&fit=crop&w=1200&q=80"
	default:
		return "https://images.unsplash.com/photo-1566073771259-6a8506099945?auto=format&fit=crop&w=1200&q=80"
	}
}
