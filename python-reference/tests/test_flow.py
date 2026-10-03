"""End-to-end flow with mocks. Runs against the in-memory store, and also against Postgres when
TEST_DATABASE_URL is set (e.g. postgresql://yate:yate@localhost:5432/yate)."""
import asyncio
import os

import pytest

from app import orchestrator
from app.llm.mock import MockLLM
from app.messaging import ConsoleMessenger
from app.models import HotelBookingResult, IncomingMessage, TripState as S
from app.orchestrator import Brain
from app.store import MemoryStore, PostgresStore

PG_URL = os.getenv("TEST_DATABASE_URL")
STORES = ["memory"] + (["postgres"] if PG_URL else [])


async def make_brain(kind: str) -> Brain:
    if kind == "memory":
        store = MemoryStore()
    else:
        store = PostgresStore(PG_URL)
        await store.connect()
        await store.pool.execute("truncate trips, messages, participants, options, bookings cascade")
    return Brain(store, MockLLM(), ConsoleMessenger())


def msg(t: dict, group="demo-group", **kw) -> IncomingMessage:
    return IncomingMessage(group_id=group, group_name="Fall trip", sender_id=t["sender_id"],
                           sender_name=t["sender_name"], text=t["text"],
                           tagged=t.get("tagged", False), **kw)


def run(coro):
    return asyncio.run(coro)


@pytest.mark.parametrize("kind", STORES)
def test_happy_path(kind, transcript):
    async def go():
        b = await make_brain(kind)
        for t in transcript["messages"][:-1]:
            await b.handle(msg(t))
        assert b.llm.calls == [], "chatter before the @mention must not spend LLM calls"
        assert b.messenger.outbox == []

        await b.handle(msg(transcript["messages"][-1]))           # "@Fare figure this out"
        trip = await b.store.get_active_trip("demo-group")
        assert trip.state == S.AWAITING_CHOICE
        assert b.llm.calls == ["record_preferences", "propose_options"]
        assert len(await b.store.get_participants(trip.id)) == 3
        assert "San Diego" in b.messenger.outbox[-1]["text"]

        await b.handle(msg({"sender_id": "u_maya", "sender_name": "Maya", "text": "lol cute"}))
        assert len(b.llm.calls) == 2, "untagged chatter while waiting must not hit the LLM"

        await b.handle(msg(transcript["followups"][0]))          # "1"
        trip = await b.store.get_trip(trip.id)
        assert trip.state == S.AWAITING_APPROVAL
        assert set(trip.itinerary["flights"]) == {"Maya", "Jordan", "Sam"}
        assert "Reply ✅" in b.messenger.outbox[-1]["text"]

        await b.handle(msg(transcript["followups"][1]))          # "✅" from Jordan
        trip = await b.store.get_trip(trip.id)
        assert trip.state == S.CONFIRMED and trip.approved_by == "Jordan"
        bookings = await b.store.get_bookings(trip.id)
        assert sorted(x.kind for x in bookings) == ["flight", "flight", "flight", "hotel"]
        assert all(x.status == "confirmed" and x.provider_ref for x in bookings)
        assert all(o["to"] == "Jordan" for o in trip.itinerary["split"]["owes"])
        assert "Booked" in b.messenger.outbox[-1]["text"]
        assert len(b.llm.calls) == 2, "choice + approval go through the regex fast path"
        await b.store.close()
    run(go())


@pytest.mark.parametrize("kind", STORES)
def test_duplicate_delivery_ignored(kind, transcript):
    async def go():
        b = await make_brain(kind)
        m = msg(transcript["messages"][-1], message_id="tg-42")
        await b.handle(m)
        await b.handle(m)
        assert b.llm.calls == ["record_preferences", "propose_options"]
        await b.store.close()
    run(go())


def test_cannot_approve_before_choosing(transcript):
    async def go():
        b = await make_brain("memory")
        await b.handle(msg(transcript["messages"][-1]))
        await b.handle(msg({"sender_id": "u_j", "sender_name": "Jordan", "text": "@Fare yes book it",
                            "tagged": True}))
        trip = await b.store.get_active_trip("demo-group")
        assert trip.state == S.AWAITING_CHOICE
        assert await b.store.get_bookings(trip.id) == []
    run(go())


def test_reject_goes_back_to_options(transcript):
    async def go():
        b = await make_brain("memory")
        await b.handle(msg(transcript["messages"][-1]))
        await b.handle(msg(transcript["followups"][0]))
        await b.handle(msg({"sender_id": "u_s", "sender_name": "Sam", "text": "❌"}))
        trip = await b.store.get_active_trip("demo-group")
        assert trip.state == S.AWAITING_CHOICE
        assert "options again" in b.messenger.outbox[-1]["text"]
    run(go())


@pytest.mark.parametrize("kind", STORES)
def test_hotel_failure_then_retry_does_not_rebook_flights(kind, transcript, monkeypatch):
    async def go():
        b = await make_brain(kind)
        await b.handle(msg(transcript["messages"][-1]))
        await b.handle(msg(transcript["followups"][0]))

        async def fail(run_id, **kw):
            return HotelBookingResult(status="failed", failure_reason="checkout page timed out")
        monkeypatch.setattr(orchestrator, "wait_for_booking", fail)
        await b.handle(msg(transcript["followups"][1]))
        trip = await b.store.get_active_trip("demo-group")
        assert trip.state == S.AWAITING_APPROVAL
        assert "retry" in b.messenger.outbox[-1]["text"]

        monkeypatch.undo()
        await b.handle(msg(transcript["followups"][1]))
        trip = await b.store.get_trip(trip.id)
        assert trip.state == S.CONFIRMED
        flights = [x for x in await b.store.get_bookings(trip.id) if x.kind == "flight"]
        assert len(flights) == 3, "flights from the first attempt must not be booked twice"
        await b.store.close()
    run(go())


def test_revise_reproposes_options(transcript):
    """'can we find something cheaper' while choosing must re-propose options, not get stuck
    or crash. This path (Brain.on_reply's "revise" branch) previously had no coverage."""
    async def go():
        b = await make_brain("memory")
        await b.handle(msg(transcript["messages"][-1]))
        trip = await b.store.get_active_trip("demo-group")
        await b.handle(msg({"sender_id": "u_sam", "sender_name": "Sam",
                            "text": "@Fare can we find something cheaper?", "tagged": True}))
        trip = await b.store.get_trip(trip.id)
        assert trip.state == S.AWAITING_CHOICE
        assert b.llm.calls == ["record_preferences", "propose_options", "interpret_reply", "propose_options"]
        assert len(await b.store.get_options(trip.id)) == 3
    run(go())


def test_revise_during_approval_goes_back_to_choice_then_reproposes(transcript):
    """Asking to revise after seeing the booking summary (AWAITING_APPROVAL) must drop back to
    AWAITING_CHOICE with fresh options, not try to book the old plan."""
    async def go():
        b = await make_brain("memory")
        await b.handle(msg(transcript["messages"][-1]))
        trip = await b.store.get_active_trip("demo-group")
        await b.handle(msg(transcript["followups"][0]))     # "1" -> AWAITING_APPROVAL
        await b.handle(msg({"sender_id": "u_sam", "sender_name": "Sam",
                            "text": "@Fare actually something cheaper please", "tagged": True}))
        trip = await b.store.get_trip(trip.id)
        assert trip.state == S.AWAITING_CHOICE
        assert await b.store.get_bookings(trip.id) == []
    run(go())


def test_cancel_then_new_trip_ignores_old_messages(transcript):
    async def go():
        b = await make_brain("memory")
        await b.handle(msg(transcript["messages"][-1]))
        first = await b.store.get_active_trip("demo-group")
        await b.handle(msg({"sender_id": "u_j", "sender_name": "Jordan", "text": "@Fare cancel this",
                            "tagged": True}))
        assert (await b.store.get_trip(first.id)).state == S.CANCELLED
        await b.handle(msg({"sender_id": "u_j", "sender_name": "Jordan", "text": "@Fare new trip!",
                            "tagged": True}))
        second = await b.store.get_active_trip("demo-group")
        assert second.id != first.id and second.history_start is not None
    run(go())


def test_typed_mention_counts_even_if_channel_missed_it(transcript):
    """WhatsApp ID formats vary, so the robot may send tagged=false for a real @mention."""
    async def go():
        b = await make_brain("memory")
        await b.handle(msg({"sender_id": "u_j", "sender_name": "Jordan", "text": "@fare figure this out"}))
        assert (await b.store.get_active_trip("demo-group")).state == S.AWAITING_CHOICE
    run(go())


def test_missing_origin_asks_instead_of_failing(transcript, monkeypatch):
    from app.llm import mock_data
    prefs = {**mock_data.PREFERENCES, "participants": [
        {**p, "origin_airport": None} if p["name"] == "Sam" else p
        for p in mock_data.PREFERENCES["participants"]]}
    monkeypatch.setattr(mock_data, "PREFERENCES", prefs)

    async def go():
        b = await make_brain("memory")
        await b.handle(msg(transcript["messages"][-1]))
        trip = await b.store.get_active_trip("demo-group")
        assert trip.state == S.COLLECTING
        assert "Sam" in b.messenger.outbox[-1]["text"] and "flying from" in b.messenger.outbox[-1]["text"]
        assert "propose_options" not in b.llm.calls
    run(go())


def test_missing_info_asks_instead_of_proposing(transcript, monkeypatch):
    """Claude's own missing_info flags (budget, dates, ...) must block proposing, not just a
    missing origin airport."""
    from app.llm import mock_data
    prefs = {**mock_data.PREFERENCES, "missing_info": ["Sam's exact budget"]}
    monkeypatch.setattr(mock_data, "PREFERENCES", prefs)

    async def go():
        b = await make_brain("memory")
        await b.handle(msg(transcript["messages"][-1]))
        trip = await b.store.get_active_trip("demo-group")
        assert trip.state == S.COLLECTING
        assert "Sam's exact budget" in b.messenger.outbox[-1]["text"]
        assert "propose_options" not in b.llm.calls
    run(go())


def test_approver_name_mismatch_still_resolves_payer(transcript):
    """Chat display name ('Jordan Lee') vs extracted participant name ('Jordan') must still
    resolve to the right payer instead of silently falling back to the first participant."""
    async def go():
        b = await make_brain("memory")
        await b.handle(msg(transcript["messages"][-1]))
        trip = await b.store.get_active_trip("demo-group")
        await b.handle(msg(transcript["followups"][0]))
        await b.handle(msg({"sender_id": "u_jordan", "sender_name": "Jordan Lee", "text": "✅"}))
        trip = await b.store.get_trip(trip.id)
        assert trip.state == S.CONFIRMED and trip.approved_by == "Jordan Lee"
        assert all(o["to"] == "Jordan" for o in trip.itinerary["split"]["owes"])
    run(go())


def test_all_options_over_budget_reasks_claude_once(transcript, monkeypatch):
    """If every option breaks someone's dates/budget, re-ask Claude once with the violations
    instead of showing the group a plan nobody can take."""
    from app.llm import mock_data
    over_budget = {**mock_data.OPTIONS, "options": [
        {**o, "est_cost_per_person": 2000} for o in mock_data.OPTIONS["options"]]}
    monkeypatch.setattr(mock_data, "OPTIONS", over_budget)

    async def go():
        b = await make_brain("memory")
        await b.handle(msg(transcript["messages"][-1]))
        trip = await b.store.get_active_trip("demo-group")
        assert trip.state == S.AWAITING_CHOICE
        assert b.llm.calls == ["record_preferences", "propose_options", "propose_options"], \
            "must re-ask exactly once, not loop forever"
        assert len(await b.store.get_options(trip.id)) == 3
    run(go())


def test_invalid_option_is_dropped_not_fatal(transcript, monkeypatch):
    from app.llm import mock_data
    bad = {**mock_data.OPTIONS, "options": [{**mock_data.OPTIONS["options"][0], "start_date": "Nov 20"}]
           + mock_data.OPTIONS["options"][1:]}
    monkeypatch.setattr(mock_data, "OPTIONS", bad)

    async def go():
        b = await make_brain("memory")
        await b.handle(msg(transcript["messages"][-1]))
        trip = await b.store.get_active_trip("demo-group")
        options = await b.store.get_options(trip.id)
        assert [o.destination for o in options] == ["Los Angeles", "Puerto Vallarta"]
        assert [o.position for o in options] == [1, 2]
    run(go())
