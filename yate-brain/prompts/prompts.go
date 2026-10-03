// Package prompts holds system prompts + the JSON schemas Claude fills in.
//
// Pattern: each stage asks Claude for JSON matching a schema (structured outputs), so the answer
// always parses. Code then decides what to do with it.
// Note: forced tool_choice is NOT supported on Sonnet 5.5 / Opus 5.5, which is why structured
// outputs are used instead of the older "force a tool call" trick.
// Tweak wording here freely; keep the schemas in sync with models/models.go.
package prompts

import "fmt"

func ExtractSystem(today string) string {
	return fmt.Sprintf(`You read a group chat where friends are planning a trip together.
Today's date is %s.

Record every human participant's travel preferences as JSON.
- Only record what people said or clearly implied. Leave out fields you don't know. Never invent.
- Resolve relative dates ("next weekend", "the 20th") using today's date. Dates are YYYY-MM-DD.
- origin_airport: the main IATA code for their home city (Vancouver -> YVR). Everyone currently
  flies from the same origin, so if only one person states theirs, apply it to everyone.
- general_preferences.availability: every individual date they said they're free (not ranges).
- general_preferences.duration_nights: how many nights they want the trip to be, if stated.
- general_preferences.activity: a short tag for the kind of trip (nature, sightseeing, nightlife, etc).
- general_preferences.culinary: a whitelist of food they want (not allergies/dealbreakers).
- flight_preferences.is_direct / class (economy, economy_plus, business) if they said so.
- accommodation_preferences.type (hotel, airbnb, hostel) / rating if they said so.
- missing_info: things you'd need to ask to plan well (e.g. "Sam's home city").`, today)
}

func ProposeSystem(botName, today, feedback string) string {
	return fmt.Sprintf(`You are %s, an AI travel agent inside a group chat.
Today's date is %s.

Given each participant's preferences, propose 2-3 trip options as JSON.
Hard rules:
- embarking_date..returning_date must fit inside EVERY participant's availability (their
  general_preferences.availability list of exact dates).
- destination_airport is the main IATA code for the destination.
Soft rules:
- Balance everyone's activity and culinary preferences; say plainly who compromises and on what.
- cost_per_person is a rough estimate: flights + share of lodging, in CAD.
- Respect flight_preferences (direct flights, cabin class) and accommodation_preferences
  (type, rating) when you can.
- Text goes straight into a group chat: short, friendly, no markdown headers.
%s`, botName, today, feedback)
}

func InterpretSystem(state, options string) string {
	return fmt.Sprintf(`You classify a group-chat reply to a travel agent.
Current stage: %s. Options shown: %s.
Return the intent as JSON:
- choose: they picked an option (set option_number)
- approve: they approve booking the summarized trip
- reject: they don't want this plan
- revise: they want changes ("cheaper", "swap to the beach one") -> revision_request
- cancel: they want to stop planning entirely
- question: they asked the agent something
- other: chatter not aimed at the agent`, state, options)
}

func AgentSystem(botName, context string) string {
	return fmt.Sprintf(`You are %s, a friendly AI travel agent in a group chat.
Answer the question briefly (1-3 sentences, chat style). Use tools if you need live data.
Trip context: %s`, botName, context)
}

// ---------------- output schemas ----------------
// Sent as structured outputs (output_config json_schema), so Claude's reply is guaranteed to
// parse. Structured-output schema rules (per Anthropic docs): every object needs
// "additionalProperties": false; no "format", "default", "minItems"/"maxItems", or ["x","null"]
// types. Unknown values are simply left out (so keep them out of "required").

var generalPreferences = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"duration_nights": map[string]any{"type": "integer"},
		"availability":    map[string]any{"type": "array", "items": map[string]any{"type": "string", "description": "YYYY-MM-DD"}},
		"activity":        map[string]any{"type": "string", "description": "e.g. nature, sightseeing, nightlife"},
		"culinary":        map[string]any{"type": "string", "description": "a whitelist of food they want, not allergies"},
	},
	"required":             []string{"availability"},
	"additionalProperties": false,
}

var flightPreferences = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"is_direct": map[string]any{"type": "boolean"},
		"class":     map[string]any{"type": "string", "enum": []string{"economy", "economy_plus", "business"}},
	},
	"required":             []string{"is_direct"},
	"additionalProperties": false,
}

var accommodationPreferences = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"type":   map[string]any{"type": "string", "enum": []string{"hotel", "airbnb", "hostel"}},
		"rating": map[string]any{"type": "number"},
	},
	"required":             []string{},
	"additionalProperties": false,
}

var RecordPreferences = map[string]any{
	"name": "record_preferences",
	"schema": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"participants": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"whatsapp_name":             map[string]any{"type": "string"},
						"origin_city":               map[string]any{"type": "string"},
						"origin_airport":            map[string]any{"type": "string", "description": "IATA code"},
						"general_preferences":       generalPreferences,
						"flight_preferences":        flightPreferences,
						"accommodation_preferences": accommodationPreferences,
					},
					"required":             []string{"whatsapp_name", "general_preferences"},
					"additionalProperties": false,
				},
			},
			"missing_info": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		},
		"required":             []string{"participants", "missing_info"},
		"additionalProperties": false,
	},
}

var ProposeOptions = map[string]any{
	"name": "propose_options",
	"schema": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"intro": map[string]any{"type": "string", "description": "One-line chat message before the options."},
			"options": map[string]any{
				"type":        "array",
				"description": "2 or 3 options, best first.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"destination":          map[string]any{"type": "string"},
						"destination_airport":  map[string]any{"type": "string", "description": "IATA code"},
						"activity_description": map[string]any{"type": "string"},
						"culinary_description": map[string]any{"type": "string"},
						"duration_nights":      map[string]any{"type": "integer"},
						"cost_per_person":      map[string]any{"type": "number"},
						"embarking_date":       map[string]any{"type": "string", "description": "YYYY-MM-DD"},
						"returning_date":       map[string]any{"type": "string", "description": "YYYY-MM-DD"},
						"why_it_works":         map[string]any{"type": "string"},
						"tradeoffs":            map[string]any{"type": "string"},
					},
					"required": []string{"destination", "destination_airport", "activity_description",
						"culinary_description", "duration_nights", "cost_per_person", "embarking_date",
						"returning_date", "why_it_works", "tradeoffs"},
					"additionalProperties": false,
				},
			},
		},
		"required":             []string{"intro", "options"},
		"additionalProperties": false,
	},
}

var InterpretReply = map[string]any{
	"name": "interpret_reply",
	"schema": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"intent": map[string]any{"type": "string", "enum": []string{"choose", "approve", "reject", "revise",
				"cancel", "question", "other"}},
			"option_number":    map[string]any{"type": "integer", "description": "only for intent=choose"},
			"revision_request": map[string]any{"type": "string", "description": "only for intent=revise"},
		},
		"required":             []string{"intent"},
		"additionalProperties": false,
	},
}
