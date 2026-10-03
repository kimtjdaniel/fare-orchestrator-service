"""Chat message templates. Built by code (not Claude) so they're instant, cheap, and never malformed.
Polish the wording here during the 18-20h rehearsal block."""
from __future__ import annotations

from datetime import date

from .config import settings
from .models import Option, Participant

NUMBER_EMOJI = {1: "1️⃣", 2: "2️⃣", 3: "3️⃣"}


def money(x: float | None, currency: str = "CAD") -> str:
    return "?" if x is None else f"${x:,.0f}"


def dates(start: date, end: date) -> str:
    if start.month == end.month:
        return f"{start:%b} {start.day}–{end.day}"
    return f"{start:%b} {start.day} – {end:%b} {end.day}"


def dashboard_link(trip_id: str) -> str:
    return f"{settings.dashboard_url}/trip/{trip_id}"


def options_message(intro: str, options: list[Option], trip_id: str) -> str:
    lines = [intro, ""]
    for o in options:
        lines.append(f"{NUMBER_EMOJI.get(o.position, o.position)} {o.destination}, "
                     f"{dates(o.start_date, o.end_date)} · ~{money(o.est_cost_per_person)}/person")
        lines.append(f"   ✔ {o.why_it_works}")
        lines.append(f"   ⚖ {o.tradeoffs}")
        lines.append("")
    lines.append(f"Reply with a number to pick one. Live plan: {dashboard_link(trip_id)}")
    return "\n".join(lines)


def summary_message(option: Option, itinerary: dict, people: list[Participant], trip_id: str) -> str:
    hotel = itinerary["hotel"]
    lines = [f"Here's the plan for {option.destination}, {dates(option.start_date, option.end_date)} ✈️", ""]
    for name, f in itinerary["flights"].items():
        lines.append(f"• {name}: {f['origin']}→{f['destination']} on {f['airline']}, {money(f['price'])}")
    nights = (option.end_date - option.start_date).days
    lines.append(f"• Hotel: {hotel['name']}, {nights} nights, {money(hotel['total_price'])} total")
    lines.append("")
    budgets = {p.name: p.budget_max for p in people}
    for name, total in itinerary["per_person"].items():
        cap = budgets.get(name)
        flag = f" ⚠️ over your {money(cap)} budget" if cap and total > cap else ""
        lines.append(f"{name}: {money(total)}{flag}")
    lines.append(f"Group total: {money(itinerary['group_total'])}")
    lines.append("")
    lines.append(f"Reply ✅ to book or ❌ to go back to the options. Details: {dashboard_link(trip_id)}")
    return "\n".join(lines)


def confirmation_message(destination: str, flight_refs: dict[str, str], hotel_ref: str,
                         split: dict) -> str:
    lines = [f"🎉 Booked! You're going to {destination}.", ""]
    for name, pnr in flight_refs.items():
        lines.append(f"✈️ {name}: flight ref {pnr}")
    lines.append(f"🏨 Hotel confirmation: {hotel_ref}")
    lines.append("")
    if split["owes"]:
        lines.append("💸 Settling up:")
        for o in split["owes"]:
            lines.append(f"   {o['from']} → {o['to']}: {money(o['amount'])}")
    lines.append("")
    lines.append("(Sandbox booking. No real money moved.)")
    return "\n".join(lines)
