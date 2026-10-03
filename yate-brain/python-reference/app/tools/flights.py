"""Flights. P3 owns the real implementation; the signatures below are the contract.

MOCK_TRAVEL=true returns deterministic fake offers so the brain can be built and tested now.
"""
from __future__ import annotations

import hashlib
import random
import string
from datetime import date

from ..config import settings
from ..models import FlightBooking, FlightOffer


def _d(v) -> date:
    return v if isinstance(v, date) else date.fromisoformat(str(v))


async def search_flights(origin: str, destination: str, depart_date, return_date,
                         adults: int = 1) -> list[FlightOffer]:
    """Round-trip offers, cheapest first. Prices are per person, CAD."""
    depart_date, return_date = _d(depart_date), _d(return_date)
    if settings.mock_travel:
        return _mock_search(origin, destination, depart_date, return_date)
    # TODO(P3): Duffel test mode — create an offer request with two slices
    # (origin->destination on depart_date, back on return_date), `adults` passengers,
    # then map each offer to FlightOffer(offer_id=offer.id, price=float(offer.total_amount), ...).
    # Cache the demo route's response to disk so stage demos don't hit the API.
    raise NotImplementedError("Real flight search not wired yet (set MOCK_TRAVEL=true)")


async def book_flight(offer: FlightOffer, passenger_name: str) -> FlightBooking:
    """Book one passenger on an offer from search_flights. Must only be called after approval."""
    if settings.mock_travel:
        pnr = "".join(random.choices(string.ascii_uppercase + string.digits, k=6))
        return FlightBooking(offer_id=offer.offer_id, pnr=pnr, price=offer.price, currency=offer.currency)
    # TODO(P3): Duffel test order for offer.offer_id with passenger details + test balance payment.
    # Return FlightBooking(pnr=order.booking_reference, ...).
    raise NotImplementedError("Real flight booking not wired yet (set MOCK_TRAVEL=true)")


def _mock_search(origin, destination, depart_date, return_date) -> list[FlightOffer]:
    seed = int(hashlib.md5(f"{origin}{destination}".encode()).hexdigest(), 16)
    base = 220 + seed % 260
    airlines = [("Air Canada", "AC"), ("WestJet", "WS"), ("Flair", "F8")]
    offers = []
    for i, (name, code) in enumerate(airlines):
        price = float(base + i * 45 - (60 if code == "F8" else 0))
        offers.append(FlightOffer(
            offer_id=f"mock_off_{origin}{destination}_{code}",
            origin=origin, destination=destination,
            depart_date=depart_date, return_date=return_date,
            airline=name, price=price,
            summary=f"{code} {100 + seed % 800} dep 0{7 + i}:10",
        ))
    return sorted(offers, key=lambda o: o.price)
