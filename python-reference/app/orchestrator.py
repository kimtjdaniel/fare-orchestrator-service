"""The brain. Every incoming group message lands in Brain.handle().

Flow (code moves the state, Claude only reads/writes text):

  COLLECTING --@mention--> extract prefs + propose options --> AWAITING_CHOICE
  AWAITING_CHOICE --"2"--> search flights + hotel --> AWAITING_APPROVAL
  AWAITING_APPROVAL --✅--> book flights + hotel (Skyvern) --> CONFIRMED
                    --❌--> back to AWAITING_CHOICE
"""
from __future__ import annotations

import asyncio
import logging
import re
from collections import defaultdict
from datetime import date, datetime, timezone
from typing import Optional
from zoneinfo import ZoneInfo

from pydantic import ValidationError

from . import formatting as fmt
from . import prompts
from .config import settings
from .llm import LLM
from .messaging import Messenger
from .models import (
    Booking, FlightOffer, HotelOffer, IncomingMessage, Message, Option, Participant, Trip, TripState as S, now,
)
from .state import assert_can_book
from .store import Store
from .tools import AGENT_HANDLERS, AGENT_TOOLS
from .tools.browser import start_hotel_booking, wait_for_booking
from .tools.flights import book_flight, search_flights
from .tools.hotels import search_hotels
from .tools.split import compute_split

log = logging.getLogger("fare.brain")

# Fast paths: obvious replies are handled without an LLM call (faster + free).
CHOICE_ONLY = re.compile(r"^\s*(?:option\s*)?([1-3])\s*[.!]?\s*$", re.I)
APPROVE_ONLY = re.compile(r"^\s*(✅|👍|yes|yep|book it|approve)\s*!*\s*$", re.I)
REJECT_ONLY = re.compile(r"^\s*(❌|👎|no|nope)\s*!*\s*$", re.I)
MENTION = re.compile(rf"@{re.escape(settings.bot_name)}\b", re.I)


def today() -> date:
    """Today in the team's timezone. Railway runs in UTC, which is already 'tomorrow'
    after 5pm in Vancouver, and that would shift "this weekend" by a day."""
    return datetime.now(ZoneInfo(settings.timezone)).date()


def validate_options(options: list[Option], people: list[Participant]) -> dict[str, list[str]]:
    """Check each option against every participant's stated budget and availability.
    Returns {option.destination: [violation, ...]} for options that don't fit someone.
    Claude is already told these rules (see PROPOSE_SYSTEM); this just catches when it slips."""
    violations: dict[str, list[str]] = {}
    for o in options:
        issues = []
        for p in people:
            if (p.budget_max is not None and o.est_cost_per_person is not None
                    and o.est_cost_per_person > p.budget_max):
                issues.append(f"{p.name}'s budget is {p.budget_max:.0f} but {o.destination} "
                              f"is ~{o.est_cost_per_person:.0f}")
            if p.available_dates and not any(
                    w.start <= o.start_date and o.end_date <= w.end for w in p.available_dates):
                issues.append(f"{o.start_date}–{o.end_date} doesn't fit {p.name}'s available dates")
        if issues:
            violations[o.destination] = issues
    return violations


def match_name(name: str, people: list[Participant]) -> Optional[Participant]:
    """Match a chat display name to an extracted participant. Chat names and Claude's extracted
    names often disagree on nicknames or a last name ("Jordan Lee" in chat vs "Jordan" extracted,
    or the reverse), so try exact, then substring, then first-name before giving up."""
    needle = name.strip().lower()
    for p in people:
        if p.name.strip().lower() == needle:
            return p
    for p in people:
        hay = p.name.strip().lower()
        if needle in hay or hay in needle:
            return p
    for p in people:
        if p.name.split()[0].lower() == needle.split()[0]:
            return p
    return None


class Brain:
    def __init__(self, store: Store, llm: LLM, messenger: Messenger) -> None:
        self.store, self.llm, self.messenger = store, llm, messenger
        self._locks: dict[str, asyncio.Lock] = defaultdict(asyncio.Lock)

    # ------------------------------------------------------------------ entry point
    async def handle(self, m: IncomingMessage) -> None:
        async with self._locks[m.group_id]:   # one message at a time per group
            try:
                await self._handle(m)
            except Exception:
                log.exception("failed handling message in %s", m.group_id)
                await self.say(m.group_id, "Oops, something broke on my end 🛠️ Give me a sec and try again.")

    async def _handle(self, m: IncomingMessage) -> None:
        # Fallback mention detection: WhatsApp uses several ID formats for one account, so the
        # robot's mentionedIds check can miss. Typed "@Fare" in the text always counts.
        if not m.tagged and MENTION.search(m.text):
            m = m.model_copy(update={"tagged": True})
        trip = await self.store.get_active_trip(m.group_id)
        sent_at = datetime.fromtimestamp(m.timestamp, timezone.utc) if m.timestamp else now()
        is_new = await self.store.save_message(Message(
            group_id=m.group_id, trip_id=trip.id if trip else None, external_id=m.message_id,
            sender_id=m.sender_id, sender_name=m.sender_name, text=m.text, tagged=m.tagged,
            sent_at=sent_at))
        if not is_new:
            return  # duplicate delivery (webhook retry)

        if trip is None:
            if m.tagged:
                trip = await self.store.create_trip(m.group_id, m.group_name)
                await self.plan(trip)
            return

        if trip.state == S.COLLECTING:
            if m.tagged:
                await self.plan(trip)
        elif trip.state in (S.AWAITING_CHOICE, S.AWAITING_APPROVAL):
            await self.on_reply(trip, m)
        elif trip.state in (S.SEARCHING, S.BOOKING) and m.tagged:
            await self.say(m.group_id, "On it, hang tight ⏳")

    # ------------------------------------------------------------------ stage: plan
    async def plan(self, trip: Trip, feedback: Optional[str] = None) -> None:
        history = await self.store.get_messages(trip.group_id, since=trip.history_start, include_bot=False)
        transcript = "\n".join(f"{msg.sender_name}: {msg.text}" for msg in history)
        day = today().isoformat()
        # Two Claude calls take 5-20s; an instant ack keeps the group chat from going silent.
        await self.say(trip.group_id, "Reading the chat 🧠 give me a few seconds…")

        extracted = await self.llm.structured(
            system=prompts.EXTRACT_SYSTEM.format(today=day),
            messages=[{"role": "user", "content": f"Group chat:\n{transcript}"}],
            schema=prompts.RECORD_PREFERENCES)
        people = [Participant(**p) for p in extracted.get("participants", [])]
        if not people:
            await self.say(trip.group_id, "I need a bit more to go on. Where's everyone flying from, "
                                          "which dates work, and what's your budget?")
            return
        await self.store.save_participants(trip.id, people)

        # Can't search flights without an origin: ask now, re-plan on the next @mention.
        # Also surface whatever else Claude flagged as missing (budget, dates, ...) rather than
        # silently proposing a plan built on guesses.
        no_origin = [p.name for p in people if not p.origin_airport]
        missing_info = extracted.get("missing_info") or []
        if no_origin or missing_info:
            asks = ([f"Where are {', '.join(no_origin)} flying from?"] if no_origin else []) + list(missing_info)
            await self.say(trip.group_id, "Almost there! " + " ".join(asks) +
                                          f" Tell me and @{settings.bot_name} again.")
            return

        result = await self.propose(trip, people, day, feedback)
        if result is None:
            return
        intro, options = result

        if trip.state != S.AWAITING_CHOICE:
            trip = await self.store.set_state(trip.id, S.AWAITING_CHOICE)
        await self.say(trip.group_id, fmt.options_message(intro, options, trip.id),
                       buttons=[(str(o.position), str(o.position)) for o in options])

    async def propose(self, trip: Trip, people: list[Participant], day: str,
                      feedback: Optional[str] = None, *,
                      retried: bool = False) -> Optional[tuple[str, list[Option]]]:
        """Ask Claude for 2-3 options. If every one breaks someone's dates or budget, re-ask
        once with the specifics instead of showing the group a plan nobody can actually take."""
        proposal = await self.llm.structured(
            system=prompts.PROPOSE_SYSTEM.format(
                bot_name=settings.bot_name, today=day,
                feedback=f"\nThe group asked for changes: {feedback}" if feedback else ""),
            messages=[{"role": "user", "content": "Participants:\n" +
                       "\n".join(p.model_dump_json() for p in people)}],
            schema=prompts.PROPOSE_OPTIONS)
        options = []
        for o in proposal["options"][:3]:
            try:  # structured outputs can't enforce date formats, so a bad option is skipped, not fatal
                options.append(Option(position=len(options) + 1, **o))
            except ValidationError as e:
                log.warning("dropping invalid option %s: %s", o, e)
        if not options:
            await self.say(trip.group_id, "I couldn't come up with a plan that fits everyone. "
                                          "Can you loosen the dates or budget a little?")
            return None

        violations = validate_options(options, people)
        if violations and len(violations) == len(options) and not retried:
            detail = " ".join(f"{dest}: {'; '.join(issues)}." for dest, issues in violations.items())
            log.info("trip %s: every option violates someone's dates/budget, re-asking once", trip.id)
            return await self.propose(
                trip, people, day, retried=True,
                feedback=f"Every option broke someone's dates or budget. {detail} "
                        f"Try again, fitting every participant's dates and budget exactly.")

        return proposal["intro"], await self.store.save_options(trip.id, options)

    async def replan(self, trip: Trip, feedback: str) -> None:
        """Re-propose options for already-known participants ("make it cheaper", "swap to the
        beach one"). No re-extraction: the group is reacting to the options, not restating
        preferences, so re-reading the whole chat through Claude again would just add latency."""
        people = await self.store.get_participants(trip.id)
        result = await self.propose(trip, people, today().isoformat(), feedback)
        if result is None:
            return
        intro, options = result
        await self.say(trip.group_id, fmt.options_message(intro, options, trip.id),
                       buttons=[(str(o.position), str(o.position)) for o in options])

    # ------------------------------------------------------------------ replies
    async def on_reply(self, trip: Trip, m: IncomingMessage) -> None:
        intent = await self.interpret(trip, m)
        if intent is None:
            return
        kind = intent["intent"]
        log.info("trip %s state=%s intent=%s", trip.id, trip.state.value, intent)

        if kind == "choose" and trip.state == S.AWAITING_CHOICE:
            await self.select_option(trip, intent.get("option_number"))
        elif kind == "approve" and trip.state == S.AWAITING_APPROVAL:
            await self.book(trip, approver=m.sender_name)
        elif kind == "reject" and trip.state == S.AWAITING_APPROVAL:
            trip = await self.store.set_state(trip.id, S.AWAITING_CHOICE)
            options = await self.store.get_options(trip.id)
            await self.say(trip.group_id, fmt.options_message("No problem. Here are the options again:",
                                                              options, trip.id),
                           buttons=[(str(o.position), str(o.position)) for o in options])
        elif kind == "revise":
            if trip.state == S.AWAITING_APPROVAL:
                trip = await self.store.set_state(trip.id, S.AWAITING_CHOICE)
            await self.replan(trip, intent.get("revision_request") or m.text)
        elif kind == "cancel":
            await self.store.set_state(trip.id, S.CANCELLED)
            await self.say(trip.group_id, "Trip planning cancelled. @ me whenever you want to start again 👋")
        elif kind == "question":
            await self.answer_question(trip, m)
        elif kind == "approve" and trip.state == S.AWAITING_CHOICE:
            await self.say(trip.group_id, "Pick an option first. Reply 1, 2 or 3 👆")

    async def interpret(self, trip: Trip, m: IncomingMessage) -> Optional[dict]:
        """Regex for the obvious replies; Claude only for @mentions that need understanding."""
        if trip.state == S.AWAITING_CHOICE and (hit := CHOICE_ONLY.match(m.text)):
            return {"intent": "choose", "option_number": int(hit.group(1))}
        if trip.state == S.AWAITING_APPROVAL and APPROVE_ONLY.match(m.text):
            return {"intent": "approve"}
        if trip.state == S.AWAITING_APPROVAL and REJECT_ONLY.match(m.text):
            return {"intent": "reject"}
        if not m.tagged:
            return None  # ordinary chatter: saved, but no reply and no LLM spend
        options = await self.store.get_options(trip.id)
        return await self.llm.structured(
            system=prompts.INTERPRET_SYSTEM.format(
                state=trip.state.value,
                options="; ".join(f"{o.position}. {o.destination}" for o in options) or "none"),
            messages=[{"role": "user", "content": m.text}],
            schema=prompts.INTERPRET_REPLY)

    async def answer_question(self, trip: Trip, m: IncomingMessage) -> None:
        view = await self.store.trip_view(trip.id)
        context = {"state": trip.state.value, "options": view["options"], "itinerary": trip.itinerary}
        answer = await self.llm.agent(
            system=prompts.AGENT_SYSTEM.format(bot_name=settings.bot_name, context=context),
            messages=[{"role": "user", "content": f"{m.sender_name}: {m.text}"}],
            tools=AGENT_TOOLS, handlers=AGENT_HANDLERS)
        await self.say(trip.group_id, answer)

    # ------------------------------------------------------------------ stage: search
    async def select_option(self, trip: Trip, number: Optional[int]) -> None:
        options = await self.store.get_options(trip.id)
        option = next((o for o in options if o.position == number), None)
        if option is None:
            await self.say(trip.group_id, f"I only have options 1–{len(options)}. Which one?")
            return
        await self.store.choose_option(trip.id, option.id)
        trip = await self.store.set_state(trip.id, S.SEARCHING)
        await self.say(trip.group_id, f"{option.destination} it is! Finding flights and a hotel 🔎")

        try:
            people = await self.store.get_participants(trip.id)
            flights = {}
            for p in people:
                offers = await search_flights(p.origin_airport, option.destination_airport,
                                              option.start_date, option.end_date)
                if not offers:
                    raise RuntimeError(f"no flights {p.origin_airport}->{option.destination_airport}")
                flights[p.name] = offers[0]
            hotels = await search_hotels(option.destination, option.start_date, option.end_date,
                                         guests=len(people))
            if not hotels:
                raise RuntimeError(f"no hotels in {option.destination}")
            hotel = hotels[0]
        except Exception as e:
            log.exception("search failed")
            await self.store.set_state(trip.id, S.AWAITING_CHOICE)
            await self.say(trip.group_id, f"Couldn't find availability for {option.destination} ({e}). "
                                          "Pick another option?")
            return

        preview = compute_split({n: f.price for n, f in flights.items()}, hotel.total_price,
                                payer=people[0].name)
        itinerary = {
            "option_id": option.id,
            "flights": {n: f.model_dump(mode="json") for n, f in flights.items()},
            "hotel": hotel.model_dump(mode="json"),
            "per_person": preview["per_person"],
            "group_total": preview["group_total"],
        }
        trip = await self.store.set_state(trip.id, S.AWAITING_APPROVAL, itinerary=itinerary)
        await self.say(trip.group_id, fmt.summary_message(option, itinerary, people, trip.id),
                       buttons=[("✅ Book it", "✅"), ("❌ Back", "❌")])

    # ------------------------------------------------------------------ stage: book
    async def book(self, trip: Trip, approver: str) -> None:
        trip = await self.store.set_state(trip.id, S.BOOKING, approved_by=approver, approved_at=now())
        assert_can_book(trip)
        people = await self.store.get_participants(trip.id)
        itin = trip.itinerary
        option = next(o for o in await self.store.get_options(trip.id) if o.id == itin["option_id"])
        await self.say(trip.group_id, f"Approved by {approver}! Booking now 🚀")

        # Flights: skip anyone already booked (makes ✅-to-retry safe after a hotel failure).
        existing = await self.store.get_bookings(trip.id)
        flight_refs = {b.participant: b.provider_ref for b in existing
                       if b.kind == "flight" and b.status == "confirmed"}
        for name, f in itin["flights"].items():
            if name in flight_refs:
                continue
            fb = await book_flight(FlightOffer(**f), passenger_name=name)
            await self.store.save_booking(Booking(
                trip_id=trip.id, kind="flight", participant=name, status="confirmed",
                provider_ref=fb.pnr, cost=fb.price, details=f))
            flight_refs[name] = fb.pnr

        # Hotel: Skyvern (or mock). Starts instantly, then we wait for the browser to finish.
        hotel = HotelOffer(**itin["hotel"])
        booking = await self.store.save_booking(Booking(
            trip_id=trip.id, kind="hotel", status="pending", cost=hotel.total_price, details=itin["hotel"]))
        run_id, live_url = await start_hotel_booking(hotel, guests=len(people), lead_name=approver,
                                                     title=f"Trip {trip.id[:8]} hotel")
        await self.store.update_booking(booking.id, status="running", skyvern_run_id=run_id, live_url=live_url)
        if live_url:
            await self.say(trip.group_id, f"Watch me book the hotel live 👀 {live_url}")

        result = await wait_for_booking(run_id)
        if result.status != "completed" or not result.confirmation_number:
            await self.store.update_booking(booking.id, status="failed",
                                            failure_reason=result.failure_reason or result.status,
                                            recording_url=result.recording_url,
                                            screenshot_urls=result.screenshot_urls)
            await self.store.set_state(trip.id, S.AWAITING_APPROVAL)
            await self.say(trip.group_id, "Flights are booked ✈️ but the hotel booking hit a snag. "
                                          "Reply ✅ to retry the hotel.")
            return

        await self.store.update_booking(booking.id, status="confirmed",
                                        provider_ref=result.confirmation_number,
                                        cost=result.total_price or hotel.total_price,
                                        recording_url=result.recording_url,
                                        screenshot_urls=result.screenshot_urls)

        payer = (match_name(approver, people) or people[0]).name
        split = compute_split({n: f["price"] for n, f in itin["flights"].items()},
                              result.total_price or hotel.total_price, payer=payer)
        await self.store.set_state(trip.id, S.CONFIRMED, itinerary={**itin, "split": split})
        await self.say(trip.group_id, fmt.confirmation_message(
            option.destination, flight_refs, result.confirmation_number, split))
        # TODO(P4, hour 12-15): trigger the ElevenLabs hotel call here and post its result.

    # ------------------------------------------------------------------ outbound
    async def say(self, group_id: str, text: str, buttons=None) -> None:
        await self.messenger.send(group_id, text, buttons)
        trip = await self.store.get_active_trip(group_id)
        await self.store.save_message(Message(
            group_id=group_id, trip_id=trip.id if trip else None, sender_id="bot",
            sender_name=settings.bot_name, text=text, is_bot=True))
