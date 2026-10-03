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
		// TODO(P3): Duffel Stays (or Amadeus) search. Set CheckoutURL to P4's fake checkout page
		// with the hotel + dates in the query string, so Skyvern books on a page we control.
		return nil, fmt.Errorf("real hotel search not wired yet (set MOCK_TRAVEL=true)")
	}

	type seed struct {
		slug, name string
		nightly    float64
		rating     float64
	}
	seeds := []seed{
		{"harbor", fmt.Sprintf("Harbor View Suites %s", city), 189.0, 4.4},
		{"casa", fmt.Sprintf("Casa Linda %s", city), 149.0, 4.1},
		{"grand", fmt.Sprintf("The Grand %s", city), 329.0, 4.8},
	}
	offers := make([]models.HotelOffer, 0, len(seeds))
	for _, s := range seeds {
		rating := s.rating
		offers = append(offers, models.HotelOffer{
			OfferID: "mock_hotel_" + s.slug, Name: s.name, City: city,
			CheckIn: checkIn, CheckOut: checkOut,
			PricePerNight: s.nightly, TotalPrice: s.nightly * float64(nights), Currency: "CAD",
			Rating: &rating,
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
