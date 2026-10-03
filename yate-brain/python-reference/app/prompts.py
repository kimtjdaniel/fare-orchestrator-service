"""System prompts + the tool schemas Claude fills in.

Pattern: each stage asks Claude for JSON matching a schema (structured outputs), so the answer
always parses. Code then decides what to do with it.
Note: forced tool_choice is NOT supported on Sonnet 5.5 / Opus 5.5, which is why we use
structured outputs instead of the older "force a tool call" trick.
Tweak wording here freely; keep the schemas in sync with models.py.
"""

EXTRACT_SYSTEM = """You read a group chat where friends are planning a trip together.
Today's date is {today}.

Record every human participant's travel preferences as JSON.
- Only record what people said or clearly implied. Leave out fields you don't know. Never invent.
- Resolve relative dates ("next weekend", "the 20th") using today's date. Dates are YYYY-MM-DD.
- origin_airport: the main IATA code for their home city (Vancouver -> YVR).
- budget_max: per person, all-in (flights + hotel), as a number. Assume CAD unless stated.
- available_dates: the windows each person CAN travel. If someone says they can't do a date,
  leave it out of their windows.
- vibe: short tags like "beach", "city", "food", "nightlife", "hiking".
- missing_info: things you'd need to ask to plan well (e.g. "Sam's home city")."""

PROPOSE_SYSTEM = """You are {bot_name}, an AI travel agent inside a group chat.
Today's date is {today}.

Given each participant's preferences, propose 2-3 trip options as JSON.
Hard rules:
- start_date..end_date must fit inside EVERY participant's availability.
- Respect each person's budget_max. If no option fits everyone, say who it doesn't fit.
- destination_airport is the main IATA code for the destination.
Soft rules:
- Balance vibes; say plainly who compromises and on what.
- est_cost_per_person is a rough estimate: flights + share of hotel, in CAD.
- Text goes straight into a group chat: short, friendly, no markdown headers.
{feedback}"""

INTERPRET_SYSTEM = """You classify a group-chat reply to a travel agent.
Current stage: {state}. Options shown: {options}.
Return the intent as JSON:
- choose: they picked an option (set option_number)
- approve: they approve booking the summarized trip
- reject: they don't want this plan
- revise: they want changes ("cheaper", "swap to the beach one") -> revision_request
- cancel: they want to stop planning entirely
- question: they asked the agent something
- other: chatter not aimed at the agent"""

AGENT_SYSTEM = """You are {bot_name}, a friendly AI travel agent in a group chat.
Answer the question briefly (1-3 sentences, chat style). Use tools if you need live data.
Trip context: {context}"""


# ---------------- output schemas ----------------
# Sent as structured outputs (output_config json_schema), so Claude's reply is guaranteed to parse.
# Structured-output schema rules (per Anthropic docs): every object needs
# "additionalProperties": false; no "format", "default", "minItems"/"maxItems", or ["x","null"] types.
# Unknown values are simply left out (so keep them out of "required").

DATE_RANGE = {
    "type": "object",
    "properties": {"start": {"type": "string", "description": "YYYY-MM-DD"},
                   "end": {"type": "string", "description": "YYYY-MM-DD"}},
    "required": ["start", "end"],
    "additionalProperties": False,
}

RECORD_PREFERENCES = {
    "name": "record_preferences",
    "schema": {
        "type": "object",
        "properties": {
            "participants": {
                "type": "array",
                "items": {
                    "type": "object",
                    "properties": {
                        "name": {"type": "string"},
                        "origin_city": {"type": "string"},
                        "origin_airport": {"type": "string", "description": "IATA code"},
                        "budget_max": {"type": "number", "description": "per person, all-in"},
                        "currency": {"type": "string", "description": "ISO code, CAD if unstated"},
                        "available_dates": {"type": "array", "items": DATE_RANGE},
                        "vibe": {"type": "array", "items": {"type": "string"}},
                        "dealbreakers": {"type": "array", "items": {"type": "string"}},
                        "notes": {"type": "string"},
                    },
                    "required": ["name", "available_dates", "vibe"],
                    "additionalProperties": False,
                },
            },
            "missing_info": {"type": "array", "items": {"type": "string"}},
        },
        "required": ["participants", "missing_info"],
        "additionalProperties": False,
    },
}

PROPOSE_OPTIONS = {
    "name": "propose_options",
    "schema": {
        "type": "object",
        "properties": {
            "intro": {"type": "string", "description": "One-line chat message before the options."},
            "options": {
                "type": "array",
                "description": "2 or 3 options, best first.",
                "items": {
                    "type": "object",
                    "properties": {
                        "destination": {"type": "string"},
                        "destination_airport": {"type": "string", "description": "IATA code"},
                        "start_date": {"type": "string", "description": "YYYY-MM-DD"},
                        "end_date": {"type": "string", "description": "YYYY-MM-DD"},
                        "est_cost_per_person": {"type": "number"},
                        "why_it_works": {"type": "string"},
                        "tradeoffs": {"type": "string"},
                    },
                    "required": ["destination", "destination_airport", "start_date", "end_date",
                                 "est_cost_per_person", "why_it_works", "tradeoffs"],
                    "additionalProperties": False,
                },
            },
        },
        "required": ["intro", "options"],
        "additionalProperties": False,
    },
}

INTERPRET_REPLY = {
    "name": "interpret_reply",
    "schema": {
        "type": "object",
        "properties": {
            "intent": {"type": "string", "enum": ["choose", "approve", "reject", "revise",
                                                    "cancel", "question", "other"]},
            "option_number": {"type": "integer", "description": "only for intent=choose"},
            "revision_request": {"type": "string", "description": "only for intent=revise"},
        },
        "required": ["intent"],
        "additionalProperties": False,
    },
}

# Tools Claude may call freely in the agent loop (read-only — booking is never exposed to Claude).
SEARCH_FLIGHTS_TOOL = {
    "name": "search_flights",
    "description": "Search round-trip flights. Returns offers sorted by price.",
    "input_schema": {
        "type": "object",
        "properties": {
            "origin": {"type": "string", "description": "IATA"},
            "destination": {"type": "string", "description": "IATA"},
            "depart_date": {"type": "string", "format": "date"},
            "return_date": {"type": "string", "format": "date"},
            "adults": {"type": "integer", "default": 1},
        },
        "required": ["origin", "destination", "depart_date", "return_date"],
    },
}

SEARCH_HOTELS_TOOL = {
    "name": "search_hotels",
    "description": "Search hotels in a city. Returns offers sorted by total price.",
    "input_schema": {
        "type": "object",
        "properties": {
            "city": {"type": "string"},
            "check_in": {"type": "string", "format": "date"},
            "check_out": {"type": "string", "format": "date"},
            "guests": {"type": "integer"},
            "max_price_per_night": {"type": "number"},
        },
        "required": ["city", "check_in", "check_out", "guests"],
    },
}
