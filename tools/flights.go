// Package tools implements the travel/booking primitives: flights, hotels, Skyvern browser
// automation, and cost splitting. P3 owns the real flight/hotel implementations; P4 owns the
// Skyvern prompt. The signatures here are the contract (CONTRACTS.md §3-4) — keep them stable.
package tools

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"fmt"
	"math/big"
	"sort"

	"fare-brain/config"
	"fare-brain/models"
)

// SearchFlights returns round-trip offers, cheapest first. Prices are per person, CAD.
func SearchFlights(ctx context.Context, cfg *config.Settings, origin, destination, departDate, returnDate string) ([]models.FlightOffer, error) {
	if cfg.MockTravel {
		return mockSearchFlights(origin, destination, departDate, returnDate), nil
	}
	// TODO(P3): Duffel test mode — create an offer request with two slices (origin->destination
	// on departDate, back on returnDate), map each offer to a models.FlightOffer. Cache the demo
	// route's response to disk so stage demos don't hit the API.
	return nil, fmt.Errorf("real flight search not wired yet (set MOCK_TRAVEL=true)")
}

// BookFlight books one passenger on an offer from SearchFlights. Must only be called after approval.
func BookFlight(ctx context.Context, cfg *config.Settings, offer models.FlightOffer, passengerName string) (*models.FlightBooking, error) {
	if cfg.MockTravel {
		return &models.FlightBooking{OfferID: offer.OfferID, PNR: randomCode(6, upperAlnum), Price: offer.Price, Currency: offer.Currency}, nil
	}
	// TODO(P3): Duffel test order for offer.OfferID with passenger details + test balance payment.
	return nil, fmt.Errorf("real flight booking not wired yet (set MOCK_TRAVEL=true)")
}

func mockSearchFlights(origin, destination, departDate, returnDate string) []models.FlightOffer {
	sum := md5.Sum([]byte(origin + destination))
	seed := new(big.Int).SetBytes(sum[:]).Uint64()
	base := 220 + int(seed%260)
	type airline struct{ name, code string }
	airlines := []airline{{"Air Canada", "AC"}, {"WestJet", "WS"}, {"Flair", "F8"}}
	offers := make([]models.FlightOffer, 0, len(airlines))
	for i, a := range airlines {
		price := float64(base + i*45)
		if a.code == "F8" {
			price -= 60
		}
		offers = append(offers, models.FlightOffer{
			OfferID: fmt.Sprintf("mock_off_%s%s_%s", origin, destination, a.code),
			Origin:  origin, Destination: destination,
			DepartDate: departDate, ReturnDate: returnDate,
			Airline: a.name, Price: price, Currency: "CAD",
			Summary: fmt.Sprintf("%s %d dep 0%d:10", a.code, 100+int(seed%800), 7+i),
		})
	}
	sort.Slice(offers, func(i, j int) bool { return offers[i].Price < offers[j].Price })
	return offers
}

const upperAlnum = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

func randomCode(n int, alphabet string) string {
	out := make([]byte, n)
	max := big.NewInt(int64(len(alphabet)))
	for i := range out {
		idx, err := rand.Int(rand.Reader, max)
		if err != nil {
			// crypto/rand failure is not expected in practice; fall back to a fixed index.
			idx = big.NewInt(0)
		}
		out[i] = alphabet[idx.Int64()]
	}
	return string(out)
}
