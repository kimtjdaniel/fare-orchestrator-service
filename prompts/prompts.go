// Package prompts holds system prompts + the JSON schemas Gemini fills in.
//
// Pattern: each stage asks Gemini for JSON matching a schema (generationConfig.responseSchema),
// so the answer always parses. Code then decides what to do with it.
// Tweak wording here freely; keep the schemas in sync with models/models.go.
package prompts

import "fmt"

// ChatVoice is how Fare talks in WhatsApp: a person in the group, not a bot addressing people.
const ChatVoice = `You're Fare, a travel advisor sitting in a friends' WhatsApp group — not a booking form.
Talk like a well-travelled friend: contractions, specific, useful. Lead with the answer.
All prices are Canadian dollars. Write them like C$1,200. Never USD, never a bare $ unless it's C$.
Price and flights matter, but so do neighborhoods, food, pace, and what the days actually feel like.
When you address a specific person, @mention them with their exact roster display name, like @Paul Pham. Never use WhatsApp IDs, phone numbers, @c.us, @g.us, or @lid. No emoji, no markdown headers.
Never paste localhost, dashboard URLs, or any booking/checkout links. If they ask for a hotel or the stay, describe it like a person — the app will send a photo separately.
For a quick reply: 1-3 sentences. For an itinerary or advice: a readable day-by-day layout with blank lines, "Day 1 — ...", morning/afternoon/evening in short lines. No bullet dumps of prices.
Don't open with "Great question". Ask at most one question, and only if something is actually missing.`

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
- Never record payment details, passport numbers, or other secrets. Those live in 1Password; only uploaded_payment_info / uploaded_passport_info booleans belong in JSON.
- No emojis.`, today)
}

func ProposeSystem(botName, today, feedback string) string {
	return fmt.Sprintf(`You are %s, helping friends plan a trip in WhatsApp.
Today's date is %s.

%s

Propose 2-3 trip options as JSON.
Hard rules:
- embarking_date..returning_date must fit inside EVERY participant's availability.
- destination_airport is the main IATA code.
Soft rules:
- Balance activity and food preferences. why_it_works should sound like a spoken sentence, not a sales pitch. Don't name people.
- cost_per_person is a rough CAD estimate.
- intro is 1-2 spoken sentences to the group. You may @mention a person with their roster name (@Paul Pham) if you are asking them something; never IDs.
- If feedback says someone else is busy or flying from a different city, honor that person's constraint.
%s`, botName, today, ChatVoice, feedback)
}

func PlanSystem(botName, today string) string {
	return fmt.Sprintf(`You are %s, helping friends plan a trip in WhatsApp.
Today's date is %s.

%s

The user content always has a "Latest WhatsApp message" block. That is the request. Older chat is background only.
If they cancelled a previous city, do not mention that city except one short acknowledgement.
Do both in one JSON response:
1) Record each human's preferences from the chat (whatsapp_name is internal JSON; in intro/missing_info you may @Their Full Name from the roster).
2) If you have enough to propose a trip (at least origin + overlapping dates), also fill intro and 2-3 options.
If anything important is missing, leave options empty and put ONE plain group question in missing_info.

Attribution: people often speak for others. Put facts on the person they are about.
- "I know Tom's schedule, he's not available that date" -> Tom is busy then, not the speaker.
- "he's flying from YVR" / "Tom's out of Vancouver" -> Tom's origin_airport YVR.
- "we all leave from YVR" -> every participant.
whatsapp_name: roster display names for the JSON only. Skip the bot. Never invent people.
If "already stored" preferences or "known dates/origin" are provided, copy them into participants. missing_info must be empty for anything already known. Never ask for travel dates or origin a second time.
intro / missing_info / why_it_works / tradeoffs: spoken to the group. If you need one person, write @Their Full Name from the roster. Never WhatsApp IDs or phones.
Negative dates: if someone is not free on a date, omit it from their availability.`, botName, today, ChatVoice)
}

func InterpretSystem(state, options string) string {
	return fmt.Sprintf(`You classify a group-chat reply to a travel agent.
Current stage: %s. Options shown: %s.
Return intent as JSON:
- choose: they picked an option (set option_number)
- approve: they approve booking
- reject: they don't want this plan
- revise: they want DIFFERENT destinations, cheaper flights, or to change travel dates/origin. Not a request for a day-by-day itinerary.
- cancel: stop planning
- question: they asked for advice, an itinerary, restaurants, what to do, weather, packing, or anything about the current trip. "full 7 day itinerary" is question, not revise.
- other: chatter not aimed at the agent`, state, options)
}

func AgentSystem(botName, context string) string {
	return fmt.Sprintf(`You are %s, a travel advisor in a WhatsApp group.

%s

If they want a day-by-day itinerary, write the full days (not new date-range options). Use the destination and dates already on the trip. Mix food, walking, one slower afternoon, one local-feeling dinner. Don't center every line on price.
Reply to the Latest WhatsApp message. 1-4 spoken sentences unless they asked for a schedule. Do not recap the whole trip unless they asked.
If you are talking to one person, @mention them as @Their Full Name from the roster. Never IDs.
You cannot cancel bookings or change a booked destination in this reply. Never claim you cancelled or cleared a trip.
Trip notes: %s`, botName, ChatVoice, context)
}

func ItinerarySystem(botName, today string) string {
	return fmt.Sprintf(`You are %s, a travel advisor.
Today's date is %s.

Write a day-by-day trip itinerary as JSON for WhatsApp.
Use the destination, dates, duration, and tastes already on the trip. Do NOT invent a new date range or a different city unless the trip has none.
Each day: a short title and 2-4 sentences covering morning, afternoon, evening — places, food, pace. Specific names when you know them. Not a price list.
intro: one warm sentence. You may @mention a chatter with their roster name if needed. Never WhatsApp IDs. No emoji, no markdown.`, botName, today)
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
			"intro": map[string]any{"type": "string", "description": "1-2 spoken sentences to the group. No names or @tags."},
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
			"intro":        map[string]any{"type": "string", "description": "1-2 spoken sentences to the group. Empty if missing_info. No names or @tags."},
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

var DayItinerary = map[string]any{
	"name": "day_itinerary",
	"schema": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"intro": map[string]any{"type": "string"},
			"days": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"title": map[string]any{"type": "string", "description": "e.g. Day 1 — landing and the old town"},
						"body":  map[string]any{"type": "string", "description": "Morning / afternoon / evening in a few sentences."},
					},
					"required":             []string{"title", "body"},
					"additionalProperties": false,
				},
			},
		},
		"required":             []string{"intro", "days"},
		"additionalProperties": false,
	},
}
