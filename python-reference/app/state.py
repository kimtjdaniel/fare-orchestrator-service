"""Trip state machine. The ONLY place that decides which stage can follow which.

Rule: code moves the trip between states, never Claude. Claude reads and writes text;
this file guarantees nothing gets booked without a ✅.
"""
from .models import Trip, TripState as S


class InvalidTransition(Exception):
    pass


ALLOWED: dict[S, set[S]] = {
    S.COLLECTING:        {S.AWAITING_CHOICE, S.CANCELLED},
    S.AWAITING_CHOICE:   {S.SEARCHING, S.AWAITING_CHOICE, S.CANCELLED},   # re-propose = stay
    S.SEARCHING:         {S.AWAITING_APPROVAL, S.AWAITING_CHOICE, S.CANCELLED},  # back if search fails
    S.AWAITING_APPROVAL: {S.BOOKING, S.AWAITING_CHOICE, S.CANCELLED},     # ❌ = back to options
    S.BOOKING:           {S.CONFIRMED, S.AWAITING_APPROVAL},              # failure = ✅ again to retry
    S.CONFIRMED:         set(),
    S.CANCELLED:         set(),
}


def check_transition(current: S, new: S) -> None:
    if new not in ALLOWED[current]:
        raise InvalidTransition(f"{current.value} -> {new.value} is not allowed")


def assert_can_book(trip: Trip) -> None:
    """Call at the top of every booking function. Belt and braces for the approval gate."""
    if trip.state != S.BOOKING or not trip.approved_by:
        raise InvalidTransition(
            f"Refusing to book trip {trip.id}: state={trip.state.value}, approved_by={trip.approved_by}"
        )
