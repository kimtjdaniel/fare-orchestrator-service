"""Hotels. P3 owns the real search; P4's fake checkout page is where bookings happen (via Skyvern)."""
from __future__ import annotations

from datetime import date
from typing import Optional

from ..config import settings
from ..models import HotelOffer


def _d(v) -> date:
    return v if isinstance(v, date) else date.fromisoformat(str(v))


async def search_hotels(city: str, check_in, check_out, guests: int,
                        max_price_per_night: Optional[float] = None) -> list[HotelOffer]:
    """Offers that fit `guests` in one booking, cheapest total first."""
    check_in, check_out = _d(check_in), _d(check_out)
    nights = max((check_out - check_in).days, 1)
    if settings.mock_travel:
        offers = []
        for slug, name, nightly, rating in [
            ("harbor", f"Harbor View Suites {city}", 189.0, 4.4),
            ("casa", f"Casa Linda {city}", 149.0, 4.1),
            ("grand", f"The Grand {city}", 329.0, 4.8),
        ]:
            offers.append(HotelOffer(
                offer_id=f"mock_hotel_{slug}", name=name, city=city,
                check_in=check_in, check_out=check_out,
                price_per_night=nightly, total_price=nightly * nights, rating=rating,
                checkout_url=f"{settings.hotel_checkout_url}/book?hotel={slug}"
                             f"&check_in={check_in}&check_out={check_out}&guests={guests}",
            ))
        if max_price_per_night is not None:
            offers = [o for o in offers if o.price_per_night <= max_price_per_night] or offers[:1]
        return sorted(offers, key=lambda o: o.total_price)
    # TODO(P3): Duffel Stays (or Amadeus) search. Set checkout_url to P4's fake checkout page
    # with the hotel + dates in the query string, so Skyvern books on a page we control.
    raise NotImplementedError("Real hotel search not wired yet (set MOCK_TRAVEL=true)")
