"""Data shapes shared by every module. If you change one, update CONTRACTS.md too."""
from datetime import date, datetime, timezone
from enum import Enum
from typing import Any, Optional

from pydantic import BaseModel, Field


def now() -> datetime:
    return datetime.now(timezone.utc)


class TripState(str, Enum):
    COLLECTING = "COLLECTING"                # listening, no plan yet
    AWAITING_CHOICE = "AWAITING_CHOICE"      # options posted, waiting for "1/2/3"
    SEARCHING = "SEARCHING"                  # transient: pulling flights + hotel
    AWAITING_APPROVAL = "AWAITING_APPROVAL"  # summary posted, waiting for ✅ / ❌
    BOOKING = "BOOKING"                      # approved; flights + hotel in progress
    CONFIRMED = "CONFIRMED"                  # done (terminal)
    CANCELLED = "CANCELLED"                  # abandoned (terminal)


ACTIVE_STATES = {s for s in TripState if s not in (TripState.CONFIRMED, TripState.CANCELLED)}


# ---------- incoming (from P2's messaging layer) ----------

class IncomingMessage(BaseModel):
    """What the WhatsApp robot / Telegram adapter POSTs to /webhook."""
    group_id: str
    group_name: str = ""
    sender_id: str
    sender_name: str
    text: str
    tagged: bool = False          # was the bot @mentioned?
    timestamp: int = 0            # unix seconds
    message_id: Optional[str] = None  # channel id, used to ignore duplicate deliveries


# ---------- stored records ----------

class Message(BaseModel):
    id: Optional[int] = None
    group_id: str
    trip_id: Optional[str] = None
    external_id: Optional[str] = None
    sender_id: str
    sender_name: str
    text: str
    tagged: bool = False
    is_bot: bool = False
    sent_at: datetime = Field(default_factory=now)


class Trip(BaseModel):
    id: str
    group_id: str
    group_name: str = ""
    state: TripState = TripState.COLLECTING
    chosen_option_id: Optional[str] = None
    itinerary: Optional[dict[str, Any]] = None
    approved_by: Optional[str] = None
    approved_at: Optional[datetime] = None
    history_start: Optional[datetime] = None
    created_at: datetime = Field(default_factory=now)
    updated_at: datetime = Field(default_factory=now)


class DateRange(BaseModel):
    start: date
    end: date


class Participant(BaseModel):
    """One person's preferences. This is the shape Claude fills via the record_preferences tool."""
    name: str
    origin_city: Optional[str] = None
    origin_airport: Optional[str] = None   # IATA code
    budget_max: Optional[float] = None     # per person, all-in
    currency: str = "CAD"
    available_dates: list[DateRange] = []
    vibe: list[str] = []
    dealbreakers: list[str] = []
    notes: Optional[str] = None


class Option(BaseModel):
    id: Optional[str] = None
    position: int
    destination: str
    destination_airport: str
    start_date: date
    end_date: date
    est_cost_per_person: Optional[float] = None
    why_it_works: str = ""
    tradeoffs: str = ""
    chosen: bool = False


class Booking(BaseModel):
    id: Optional[str] = None
    trip_id: str
    kind: str                      # "flight" | "hotel"
    participant: Optional[str] = None
    status: str = "pending"        # pending | running | confirmed | failed
    provider_ref: Optional[str] = None
    cost: Optional[float] = None
    currency: str = "CAD"
    details: Optional[dict[str, Any]] = None
    skyvern_run_id: Optional[str] = None
    live_url: Optional[str] = None
    recording_url: Optional[str] = None
    screenshot_urls: Optional[list[str]] = None
    failure_reason: Optional[str] = None


# ---------- travel tool results (P3 / P4 return these) ----------

class FlightOffer(BaseModel):
    offer_id: str
    origin: str
    destination: str
    depart_date: date
    return_date: date
    airline: str
    price: float
    currency: str = "CAD"
    summary: str = ""   # "AC 554 dep 08:10, arr 11:35"


class HotelOffer(BaseModel):
    offer_id: str
    name: str
    city: str
    check_in: date
    check_out: date
    price_per_night: float
    total_price: float
    currency: str = "CAD"
    rating: Optional[float] = None
    checkout_url: Optional[str] = None   # page Skyvern drives to book it


class FlightBooking(BaseModel):
    offer_id: str
    pnr: str
    price: float
    currency: str = "CAD"


class HotelBookingResult(BaseModel):
    status: str                      # completed | failed | ...
    confirmation_number: Optional[str] = None
    total_price: Optional[float] = None
    recording_url: Optional[str] = None
    screenshot_urls: list[str] = []
    failure_reason: Optional[str] = None
