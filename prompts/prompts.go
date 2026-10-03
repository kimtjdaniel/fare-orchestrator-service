// Package prompts holds system prompts + the JSON schemas Gemini fills in.
//
// Pattern: each stage asks Gemini for JSON matching a schema (generationConfig.responseSchema),
// so the answer always parses. Code then decides what to do with it.
// Tweak wording here freely; keep the schemas in sync with models/models.go.
package prompts

import "fmt"

func ExtractSystem(today string) string {
	return fmt.Sprintf(`You read a group chat where friends are planning a trip together.
Today's date is %s.

Record every human participant's travel preferences as JSON.
- Only record what people said or clearly implied. Leave out fields you don't know. Never invent.
- Resolve relative dates ("next weekend", "the 20th") using today's date. Dates are YYYY-MM-DD.
- origin_airport: the main IATA code for their home city (Vancouver -> YVR). If only one origin is stated for the group ("we all fly from YVR"), apply it to everyone. If it is stated for one named person, apply it only to that person.
- Someone may speak for someone else. Attribute the fact to the person it is about, not the speaker.
  Examples: "I know Tom's schedule, he's not available that date" -> Tom is busy that date.
  "he's flying from YVR" -> the named/referred person (Tom), origin YVR.
  "Priya can do the 20th" -> Priya's availability.
- Pronouns (he/she/they) refer to the most recently named group member, matched to the roster.
- Negative constraints count: if they say someone is not free on a date, do not list that date as available for them. If they previously looked free then someone rules a date out, drop it.
- general_preferences.availability: dates that person is free.
- general_preferences.duration_nights, activity, culinary: as stated for that person.
- missing_info: the smallest set of questions still needed.
- If a roster is provided, use those exact display names for whatsapp_name (first + last is fine). Skip the bot. Do not invent people.
- Never store WhatsApp IDs, phone numbers, or @c.us/@g.us/@lid as names.
- No emojis.`, today)
}

func ProposeSystem(botName, today, feedback string) string {
	return fmt.Sprintf(`You are %s, a travel agent in a WhatsApp group.
Today's date is %s.

Propose 2-3 trip options as JSON.
Hard rules:
- embarking_date..returning_date must fit inside EVERY participant's availability.
- destination_airport is the main IATA code.
Soft rules:
- Balance activity and food preferences. Say who compromises, plainly.
- cost_per_person is a rough CAD estimate.
- intro, why_it_works, and tradeoffs: short, no emojis, no markdown.
- If feedback says someone else is busy or flying from a different city, honor that person's constraint.
%s`, botName, today, feedback)
}

func PlanSystem(botName, today string) string {
	return fmt.Sprintf(`You are %s, a travel agent in a WhatsApp group.
Today's date is %s.

Do both in one JSON response:
1) Record each human's preferences from the chat.
2) If you have enough to propose a trip (at least origin + overlapping dates), also fill intro and 2-3 options.
If anything important is missing, leave options empty and list questions in missing_info.

Attribution: people often speak for others. Put facts on the person they are about.
- "I know Tom's schedule, he's not available that date" -> Tom is busy then, not the speaker.
- "he's flying from YVR" / "Tom's out of Vancouver" -> Tom's origin_airport YVR.
- "we all leave from YVR" -> every participant.
Names: use first names from the WhatsApp roster. Skip the bot. Never invent people.
Never write WhatsApp IDs, phone numbers, @c.us, @g.us, @lid, or @tags. Say "Priya", not "@Priya" and not "14165551234@c.us".
Negative dates: if someone is not free on a date, omit it from their availability.
Copy: intro/why_it_works/tradeoffs are short, no emojis, no markdown.`, botName, today)
}

func InterpretSystem(state, options string) string {
	return fmt.Sprintf(`You classify a group-chat reply to a travel agent.
Current stage: %s. Options shown: %s.
Return intent as JSON:
- choose: they picked an option (set option_number)
- approve: they approve booking
- reject: they don't want this plan
- revise: they want different options, OR they are updating anyone's prefs (including speaking for someone else: "Tom can't do those dates", "he's flying from YVR", "I know her schedule") -> revision_request should quote the new fact
- cancel: stop planning
- question: they asked the agent something
- other: chatter not aimed at the agent`, state, options)
}

func AgentSystem(botName, context string) string {
	return fmt.Sprintf(`You are %s, a travel agent in a WhatsApp group.
Answer in 1-2 short sentences. Use people's first names. No WhatsApp IDs, no @tags, no emojis, no markdown.
Trip context: %s`, botName, context)
}

// ---------------- output schemas ----------------
// Plain JSON Schema (lowercase types, "additionalProperties": false); llm.GeminiLLM converts this
// into Gemini's OpenAPI-subset Schema shape (uppercase type enum, additionalProperties stripped)
// before sending it as generationConfig.responseSchema. Keep writing schemas in this plain form —
// the conversion is the client's job, not the prompt author's.

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

var PlanTrip = map[string]any{
	"name": "plan_trip",
	"schema": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"participants": RecordPreferences["schema"].(map[string]any)["properties"].(map[string]any)["participants"],
			"missing_info": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"intro":        map[string]any{"type": "string", "description": "One-line chat message before the options. Empty if missing_info."},
			"options":      ProposeOptions["schema"].(map[string]any)["properties"].(map[string]any)["options"],
		},
		"required":             []string{"participants", "missing_info"},
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
