package tools

import (
	"context"

	"yate-brain/config"
	"yate-brain/llm"
)

// AgentTools/AgentHandlers are read-only (search) and are the only tools Claude may call freely in
// the agent loop. Booking functions must never be added here — only the approval flow in
// orchestrator/brain.go can book.

var AgentTools = []llm.Tool{
	{
		Name:        "search_flights",
		Description: "Search round-trip flights. Returns offers sorted by price.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"origin":      map[string]any{"type": "string", "description": "IATA"},
				"destination": map[string]any{"type": "string", "description": "IATA"},
				"depart_date": map[string]any{"type": "string", "format": "date"},
				"return_date": map[string]any{"type": "string", "format": "date"},
				"adults":      map[string]any{"type": "integer", "default": 1},
			},
			"required": []string{"origin", "destination", "depart_date", "return_date"},
		},
	},
	{
		Name:        "search_hotels",
		Description: "Search hotels in a city. Returns offers sorted by total price.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"city":                map[string]any{"type": "string"},
				"check_in":            map[string]any{"type": "string", "format": "date"},
				"check_out":           map[string]any{"type": "string", "format": "date"},
				"guests":              map[string]any{"type": "integer"},
				"max_price_per_night": map[string]any{"type": "number"},
			},
			"required": []string{"city", "check_in", "check_out", "guests"},
		},
	},
}

func AgentHandlers(cfg *config.Settings) map[string]llm.Handler {
	return map[string]llm.Handler{
		"search_flights": func(ctx context.Context, input map[string]any) (any, error) {
			origin, _ := input["origin"].(string)
			destination, _ := input["destination"].(string)
			depart, _ := input["depart_date"].(string)
			ret, _ := input["return_date"].(string)
			offers, err := SearchFlights(ctx, cfg, origin, destination, depart, ret)
			if err != nil {
				return nil, err
			}
			return firstN(offers, 5), nil
		},
		"search_hotels": func(ctx context.Context, input map[string]any) (any, error) {
			city, _ := input["city"].(string)
			checkIn, _ := input["check_in"].(string)
			checkOut, _ := input["check_out"].(string)
			guests := 1
			if g, ok := input["guests"].(float64); ok {
				guests = int(g)
			}
			var maxPrice *float64
			if m, ok := input["max_price_per_night"].(float64); ok {
				maxPrice = &m
			}
			offers, err := SearchHotels(ctx, cfg, city, checkIn, checkOut, guests, maxPrice)
			if err != nil {
				return nil, err
			}
			return firstN(offers, 5), nil
		},
	}
}

func firstN[T any](xs []T, n int) []T {
	if len(xs) <= n {
		return xs
	}
	return xs[:n]
}
