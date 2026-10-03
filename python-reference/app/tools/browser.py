"""Hotel booking via Skyvern (P4 owns the checkout page + prompt tuning; signatures are the contract).

start_hotel_booking() returns immediately with a run id; wait_for_booking() polls until done.
The orchestrator calls these ONLY from the approval flow, after state.assert_can_book().
"""
from __future__ import annotations

import asyncio
import random
import string
from typing import Optional

from ..config import settings
from ..models import HotelBookingResult, HotelOffer

TERMINAL = {"completed", "failed", "terminated", "timed_out", "canceled"}

CONFIRMATION_SCHEMA = {
    "type": "object",
    "properties": {
        "confirmation_number": {"type": "string"},
        "hotel_name": {"type": "string"},
        "total_price": {"type": "string"},
        "check_in": {"type": "string"},
        "check_out": {"type": "string"},
    },
}

_skyvern = None


def _client():
    global _skyvern
    if _skyvern is None:
        from skyvern import Skyvern  # lazy: only needed when MOCK_BROWSER=false
        _skyvern = Skyvern(api_key=settings.skyvern_api_key)
    return _skyvern


def build_prompt(hotel: HotelOffer, guests: int, lead_name: str, lead_email: str) -> str:
    return f"""Book a room at {hotel.name} for {guests} guests,
check-in {hotel.check_in}, check-out {hotel.check_out}.
Guest name: {lead_name}. Email: {lead_email}.
Pay with test card 4242 4242 4242 4242, expiry 12/34, CVC 123, postal code V5H 1A1.
The task is complete when a page shows a booking confirmation number.
If the total shown is more than {hotel.total_price * 1.1:.0f} {hotel.currency}, stop without paying."""


async def start_hotel_booking(hotel: HotelOffer, guests: int, lead_name: str,
                              lead_email: str = "demo@fare.travel",
                              title: str = "Fare hotel booking") -> tuple[str, Optional[str]]:
    """Kick off the booking. Returns (run_id, live_url). Does not wait."""
    if settings.mock_browser:
        return "mock_run_" + "".join(random.choices(string.ascii_lowercase, k=8)), None
    run = await _client().run_task(
        prompt=build_prompt(hotel, guests, lead_name, lead_email),
        url=hotel.checkout_url or settings.hotel_checkout_url,
        data_extraction_schema=CONFIRMATION_SCHEMA,
        max_steps=settings.skyvern_max_steps,
        title=title,
    )
    return run.run_id, getattr(run, "app_url", None)


async def wait_for_booking(run_id: str, poll_seconds: float = 3,
                           timeout: float = 600) -> HotelBookingResult:
    if settings.mock_browser:
        await asyncio.sleep(settings.mock_booking_delay)
        code = "".join(random.choices(string.ascii_uppercase + string.digits, k=8))
        return HotelBookingResult(status="completed", confirmation_number=f"HV-{code}")

    loop = asyncio.get_running_loop()
    deadline = loop.time() + timeout
    while True:
        run = await _client().get_run(run_id)
        if run.status in TERMINAL:
            break
        if loop.time() > deadline:
            return HotelBookingResult(status="timed_out", failure_reason="Gave up waiting for Skyvern")
        await asyncio.sleep(poll_seconds)

    output = run.output or {}
    if isinstance(output, list):  # schema-less runs sometimes return a list
        output = output[0] if output else {}
    price = output.get("total_price")
    try:
        price = float(str(price).replace("$", "").replace(",", "").split()[0]) if price else None
    except ValueError:
        price = None
    return HotelBookingResult(
        status=run.status,
        confirmation_number=output.get("confirmation_number"),
        total_price=price,
        recording_url=getattr(run, "recording_url", None),
        screenshot_urls=list(getattr(run, "screenshot_urls", None) or []),
        failure_reason=getattr(run, "failure_reason", None),
    )
