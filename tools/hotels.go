package tools

import (
	"context"
	"fmt"
	"sort"

	"fare-brain/config"
	"fare-brain/models"
)

// SearchHotels returns offers that fit guests in one booking, cheapest total first.
// maxPricePerNight of nil means no cap.
func SearchHotels(ctx context.Context, cfg *config.Settings, city, checkIn, checkOut string, guests int, maxPricePerNight *float64) ([]models.HotelOffer, error) {
	ci, err := models.ParseDate(checkIn)
	if err != nil {
		return nil, err
	}
	co, err := models.ParseDate(checkOut)
	if err != nil {
		return nil, err
	}
	nights := int(co.Sub(ci).Hours() / 24)
	if nights < 1 {
		nights = 1
	}
	if !cfg.MockTravel {
		if guests < 1 {
			guests = 1
		}
		request := map[string]any{"session_id": searchSession(ctx), "destination": city, "check_in": checkIn, "check_out": checkOut, "adults": guests, "rooms": 1, "currency": "CAD"}
		if maxPricePerNight != nil {
			request["budget"] = *maxPricePerNight * float64(nights)
		}
		result, err := runSearchService(ctx, cfg, "hotel", request)
		if err != nil {
			return nil, err
		}
		rows, ok := result["hotels"].([]any)
		if !ok {
			return nil, fmt.Errorf("hotel service returned no hotel list")
		}
		offers := make([]models.HotelOffer, 0, len(rows))
		for i, raw := range rows {
			row, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			var rating *float64
			if n, ok := row["rating"].(float64); ok {
				rating = &n
			}
			offers = append(offers, models.HotelOffer{OfferID: fmt.Sprintf("%s-%d", searchRecordText(result["search_id"]), i), Name: searchRecordText(row["name"]), City: city, CheckIn: checkIn, CheckOut: checkOut, PricePerNight: searchRecordNumber(row["total_price"]) / float64(nights), TotalPrice: searchRecordNumber(row["total_price"]), Currency: searchRecordText(row["currency"]), Rating: rating, CheckoutURL: searchRecordText(row["url"])})
		}
		sort.Slice(offers, func(i, j int) bool { return offers[i].TotalPrice < offers[j].TotalPrice })
		return offers, nil
	}

	type seed struct {
		slug, name, image string
		nightly           float64
		rating            float64
	}
	seeds := []seed{
		{"harbor", fmt.Sprintf("Harbor View Suites %s", city), "https://images.unsplash.com/photo-1566073771259-6a8506099945?auto=format&fit=crop&w=1200&q=80", 189.0, 4.4},
		{"casa", fmt.Sprintf("Casa Linda %s", city), "https://images.unsplash.com/photo-1520250497591-112f2f40a3f4?auto=format&fit=crop&w=1200&q=80", 149.0, 4.1},
		{"grand", fmt.Sprintf("The Grand %s", city), "https://images.unsplash.com/photo-1542314831-068cd1dbfeeb?auto=format&fit=crop&w=1200&q=80", 329.0, 4.8},
	}
	offers := make([]models.HotelOffer, 0, len(seeds))
	for _, s := range seeds {
		rating := s.rating
		offers = append(offers, models.HotelOffer{
			OfferID: "mock_hotel_" + s.slug, Name: s.name, City: city,
			CheckIn: checkIn, CheckOut: checkOut,
			PricePerNight: s.nightly, TotalPrice: s.nightly * float64(nights), Currency: "CAD",
			Rating: &rating, ImageURL: s.image,
			CheckoutURL: fmt.Sprintf("%s/book?hotel=%s&check_in=%s&check_out=%s&guests=%d",
				cfg.HotelCheckoutURL, s.slug, checkIn, checkOut, guests),
		})
	}
	if maxPricePerNight != nil {
		var filtered []models.HotelOffer
		for _, o := range offers {
			if o.PricePerNight <= *maxPricePerNight {
				filtered = append(filtered, o)
			}
		}
		if len(filtered) == 0 {
			filtered = offers[:1]
		}
		offers = filtered
	}
	sort.Slice(offers, func(i, j int) bool { return offers[i].TotalPrice < offers[j].TotalPrice })
	return offers, nil
}
