package llm

// Canned Gemini outputs for MOCK_LLM=true. They match testdata/demo_transcript.json. When the
// demo script changes, update both files together.

var mayaAvailability = []any{
	"2026-11-13", "2026-11-14", "2026-11-15", "2026-11-16", "2026-11-17", "2026-11-18",
	"2026-11-19", "2026-11-20", "2026-11-21", "2026-11-22", "2026-11-23",
}

var jordanAvailability = []any{
	"2026-11-13", "2026-11-14", "2026-11-15", "2026-11-16",
	"2026-11-20", "2026-11-21", "2026-11-22", "2026-11-23",
}

var samAvailability = []any{"2026-11-20", "2026-11-21", "2026-11-22", "2026-11-23"}

var Preferences = map[string]any{
	"participants": []any{
		map[string]any{
			"whatsapp_name": "Maya", "origin_city": "Vancouver", "origin_airport": "YVR",
			"general_preferences": map[string]any{
				"availability": mayaAvailability, "duration_nights": 3, "activity": "beach", "culinary": "",
			},
			"flight_preferences":        map[string]any{"is_direct": false},
			"accommodation_preferences": map[string]any{},
		},
		map[string]any{
			"whatsapp_name": "Jordan", "origin_city": "Toronto", "origin_airport": "YYZ",
			"general_preferences": map[string]any{
				"availability": jordanAvailability, "duration_nights": 3, "activity": "city", "culinary": "food scene",
			},
			"flight_preferences":        map[string]any{"is_direct": false},
			"accommodation_preferences": map[string]any{},
		},
		map[string]any{
			"whatsapp_name": "Sam", "origin_city": "Calgary", "origin_airport": "YYC",
			"general_preferences": map[string]any{
				"availability": samAvailability, "duration_nights": 3,
			},
			"flight_preferences":        map[string]any{"is_direct": false},
			"accommodation_preferences": map[string]any{},
		},
	},
	"missing_info": []any{},
}

var Options = map[string]any{
	"intro": "Only Nov 20–23 works for all three of you. Here's what fits:",
	"options": []any{
		map[string]any{
			"destination": "San Diego", "destination_airport": "SAN",
			"activity_description": "beach walks and a bit of sightseeing", "culinary_description": "taco shops and a serious food scene",
			"duration_nights": 3.0, "cost_per_person": 720.0,
			"embarking_date": "2026-11-20", "returning_date": "2026-11-23",
			"why_it_works": "Beach for Maya, serious taco and food scene for Jordan, and keeps costs reasonable.",
			"tradeoffs":    "Ocean's chilly in November, so it's beach walks more than swimming.",
		},
		map[string]any{
			"destination": "Los Angeles", "destination_airport": "LAX",
			"activity_description": "city exploring with a beach day", "culinary_description": "biggest food scene of the three",
			"duration_nights": 3.0, "cost_per_person": 780.0,
			"embarking_date": "2026-11-20", "returning_date": "2026-11-23",
			"why_it_works": "Biggest food city of the three for Jordan, with Santa Monica beach for Maya.",
			"tradeoffs":    "A bit pricier, and you'll spend time in traffic.",
		},
		map[string]any{
			"destination": "Puerto Vallarta", "destination_airport": "PVR",
			"activity_description": "warm beach days", "culinary_description": "beachside seafood",
			"duration_nights": 3.0, "cost_per_person": 890.0,
			"embarking_date": "2026-11-20", "returning_date": "2026-11-23",
			"why_it_works": "Warmest beach by far, and Maya's dream pick.",
			"tradeoffs":    "Priciest option, and it's less of a city for Jordan.",
		},
	},
}
