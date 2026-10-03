"""Canned Claude outputs for MOCK_LLM=true. They match tests/fixtures/demo_transcript.json.
When your team locks the demo script in hour 0, update both files together."""

PREFERENCES = {
    "participants": [
        {"name": "Maya", "origin_city": "Vancouver", "origin_airport": "YVR", "budget_max": 1500,
         "currency": "CAD", "available_dates": [{"start": "2026-11-13", "end": "2026-11-23"}],
         "vibe": ["beach"], "dealbreakers": ["rain"], "notes": "Done with Vancouver rain"},
        {"name": "Jordan", "origin_city": "Toronto", "origin_airport": "YYZ", "budget_max": 1800,
         "currency": "CAD", "available_dates": [{"start": "2026-11-13", "end": "2026-11-16"},
                                                {"start": "2026-11-20", "end": "2026-11-23"}],
         "vibe": ["city", "food"], "dealbreakers": [], "notes": None},
        {"name": "Sam", "origin_city": "Calgary", "origin_airport": "YYC", "budget_max": 800,
         "currency": "CAD", "available_dates": [{"start": "2026-11-20", "end": "2026-11-23"}],
         "vibe": [], "dealbreakers": [], "notes": "Works the Nov 13 weekend; tight budget"},
    ],
    "missing_info": [],
}

OPTIONS = {
    "intro": "Only Nov 20–23 works for all three of you, and Sam's $800 cap rules out anything long-haul. Here's what fits:",
    "options": [
        {"destination": "San Diego", "destination_airport": "SAN",
         "start_date": "2026-11-20", "end_date": "2026-11-23", "est_cost_per_person": 720,
         "why_it_works": "Beach for Maya, serious taco and food scene for Jordan, and it fits under Sam's $800.",
         "tradeoffs": "Ocean's chilly in November, so it's beach walks more than swimming."},
        {"destination": "Los Angeles", "destination_airport": "LAX",
         "start_date": "2026-11-20", "end_date": "2026-11-23", "est_cost_per_person": 780,
         "why_it_works": "Biggest food city of the three for Jordan, with Santa Monica beach for Maya.",
         "tradeoffs": "Lands right at Sam's limit, and you'll spend time in traffic."},
        {"destination": "Puerto Vallarta", "destination_airport": "PVR",
         "start_date": "2026-11-20", "end_date": "2026-11-23", "est_cost_per_person": 890,
         "why_it_works": "Warmest beach by far, and Maya's dream pick.",
         "tradeoffs": "About $90 over Sam's budget, and it's less of a city for Jordan."},
    ],
}
