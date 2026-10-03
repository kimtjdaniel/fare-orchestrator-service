"""Read-only tools Claude may call in the agent loop. Booking functions are deliberately NOT here:
only the approval flow in orchestrator.py can book."""
from ..prompts import SEARCH_FLIGHTS_TOOL, SEARCH_HOTELS_TOOL
from .flights import search_flights
from .hotels import search_hotels


async def _search_flights(**kw):
    return [o.model_dump(mode="json") for o in (await search_flights(**kw))[:5]]


async def _search_hotels(**kw):
    return [o.model_dump(mode="json") for o in (await search_hotels(**kw))[:5]]


AGENT_TOOLS = [SEARCH_FLIGHTS_TOOL, SEARCH_HOTELS_TOOL]
AGENT_HANDLERS = {"search_flights": _search_flights, "search_hotels": _search_hotels}
