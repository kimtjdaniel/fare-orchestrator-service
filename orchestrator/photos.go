package orchestrator

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

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
	imageURL := strings.TrimSpace(hotel.ImageURL)
	if imageURL == "" {
		imageURL = hotelImageForName(hotel.Name, hotel.City)
	}
	media := messaging.Media{URL: imageURL, Filename: "hotel.jpg", Mimetype: "image/jpeg"}
	if b64, mime, ferr := fetchImageBase64(ctx, imageURL); ferr != nil {
		slog.Warn("hotel photo download failed, sending url", "err", ferr, "url", imageURL)
	} else {
		media.DataBase64 = b64
		media.URL = ""
		if mime != "" {
			media.Mimetype = mime
		}
	}
	if err := b.Messenger.SendMedia(ctx, trip.GroupID, caption, media); err != nil {
		slog.Warn("hotel photo send failed", "err", err)
		return b.say(ctx, trip.GroupID, caption, nil)
	}
	slog.Info("hotel photo sent", "hotel", hotel.Name, "has_bytes", media.DataBase64 != "")
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

func fetchImageBase64(ctx context.Context, rawURL string) (string, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "image/avif,image/webp,image/apng,image/*,*/*;q=0.8")
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", "", fmt.Errorf("image fetch status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
	if err != nil {
		return "", "", err
	}
	if len(body) < 32 {
		return "", "", fmt.Errorf("image too small")
	}
	mime := resp.Header.Get("content-type")
	if i := strings.Index(mime, ";"); i >= 0 {
		mime = mime[:i]
	}
	mime = strings.TrimSpace(mime)
	if mime == "" || !strings.HasPrefix(mime, "image/") {
		mime = http.DetectContentType(body)
	}
	if !strings.HasPrefix(mime, "image/") {
		return "", "", fmt.Errorf("not an image: %s", mime)
	}
	return base64.StdEncoding.EncodeToString(body), mime, nil
}
