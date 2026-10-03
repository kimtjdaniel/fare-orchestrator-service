"""Cost split. Real implementation (pure function, no APIs) — done tonight, tested in tests/."""
from __future__ import annotations


def compute_split(flight_costs: dict[str, float], hotel_total: float, payer: str) -> dict:
    """Everyone pays their own flight + an equal share of the hotel.
    The agent charged everything to `payer`'s card, so everyone else owes `payer`.

    Returns {"per_person": {name: total}, "owes": [{"from", "to", "amount"}], "group_total"}.
    """
    names = list(flight_costs)
    if not names:
        return {"per_person": {}, "owes": [], "group_total": 0.0}
    hotel_share = round(hotel_total / len(names), 2)
    per_person = {n: round(flight_costs[n] + hotel_share, 2) for n in names}
    # rounding remainder (pennies) goes to the payer so the totals match exactly
    group_total = round(sum(flight_costs.values()) + hotel_total, 2)
    if payer in per_person:
        per_person[payer] = round(per_person[payer] + group_total - sum(per_person.values()), 2)
    owes = [{"from": n, "to": payer, "amount": amt} for n, amt in per_person.items() if n != payer]
    return {"per_person": per_person, "owes": owes, "group_total": group_total}
