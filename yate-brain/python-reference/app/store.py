"""Persistence behind one interface.

MemoryStore  — used when DATABASE_URL is empty (solo dev, tests). Lost on restart.
PostgresStore — Supabase or any Postgres. Same methods, same return types.

Every state change goes through set_state(), which enforces the state machine.
"""
from __future__ import annotations

import json
import uuid
from datetime import datetime
from typing import Any, Optional

from .models import (
    ACTIVE_STATES, Booking, Message, Option, Participant, Trip, TripState, now,
)
from .state import check_transition


class Store:
    async def connect(self) -> None: ...
    async def close(self) -> None: ...

    # messages
    async def save_message(self, msg: Message) -> bool:
        """Returns False if this external_id was already saved (duplicate delivery)."""
        raise NotImplementedError
    async def get_messages(self, group_id: str, since: Optional[datetime] = None,
                           limit: int = 100, include_bot: bool = True) -> list[Message]:
        raise NotImplementedError

    # trips
    async def create_trip(self, group_id: str, group_name: str = "") -> Trip: raise NotImplementedError
    async def get_trip(self, trip_id: str) -> Optional[Trip]: raise NotImplementedError
    async def get_active_trip(self, group_id: str) -> Optional[Trip]: raise NotImplementedError
    async def last_closed_at(self, group_id: str) -> Optional[datetime]: raise NotImplementedError
    async def update_trip(self, trip_id: str, **fields: Any) -> Trip: raise NotImplementedError

    async def set_state(self, trip_id: str, new: TripState, **fields: Any) -> Trip:
        trip = await self.get_trip(trip_id)
        check_transition(trip.state, new)
        return await self.update_trip(trip_id, state=new, **fields)

    # participants / options / bookings
    async def save_participants(self, trip_id: str, people: list[Participant]) -> None: raise NotImplementedError
    async def get_participants(self, trip_id: str) -> list[Participant]: raise NotImplementedError
    async def save_options(self, trip_id: str, options: list[Option]) -> list[Option]: raise NotImplementedError
    async def get_options(self, trip_id: str) -> list[Option]: raise NotImplementedError
    async def choose_option(self, trip_id: str, option_id: str) -> None: raise NotImplementedError
    async def save_booking(self, booking: Booking) -> Booking: raise NotImplementedError
    async def update_booking(self, booking_id: str, **fields: Any) -> Booking: raise NotImplementedError
    async def get_bookings(self, trip_id: str) -> list[Booking]: raise NotImplementedError

    async def trip_view(self, trip_id: str) -> Optional[dict]:
        """Everything the dashboard needs in one JSON blob."""
        trip = await self.get_trip(trip_id)
        if not trip:
            return None
        return {
            "trip": trip.model_dump(mode="json"),
            "participants": [p.model_dump(mode="json") for p in await self.get_participants(trip_id)],
            "options": [o.model_dump(mode="json") for o in await self.get_options(trip_id)],
            "bookings": [b.model_dump(mode="json") for b in await self.get_bookings(trip_id)],
        }


# =====================================================================
class MemoryStore(Store):
    def __init__(self) -> None:
        self.messages: list[Message] = []
        self.trips: dict[str, Trip] = {}
        self.participants: dict[str, list[Participant]] = {}
        self.options: dict[str, list[Option]] = {}
        self.bookings: dict[str, Booking] = {}
        self._msg_id = 0

    async def save_message(self, msg: Message) -> bool:
        if msg.external_id and any(
            m.group_id == msg.group_id and m.external_id == msg.external_id for m in self.messages
        ):
            return False
        self._msg_id += 1
        self.messages.append(msg.model_copy(update={"id": self._msg_id}))
        return True

    async def get_messages(self, group_id, since=None, limit=100, include_bot=True):
        rows = [m for m in self.messages if m.group_id == group_id
                and (since is None or m.sent_at > since) and (include_bot or not m.is_bot)]
        return rows[-limit:]

    async def create_trip(self, group_id, group_name=""):
        trip = Trip(id=str(uuid.uuid4()), group_id=group_id, group_name=group_name,
                    history_start=await self.last_closed_at(group_id))
        self.trips[trip.id] = trip
        return trip

    async def get_trip(self, trip_id):
        return self.trips.get(trip_id)

    async def get_active_trip(self, group_id):
        active = [t for t in self.trips.values() if t.group_id == group_id and t.state in ACTIVE_STATES]
        return max(active, key=lambda t: t.created_at) if active else None

    async def last_closed_at(self, group_id):
        closed = [t.updated_at for t in self.trips.values()
                  if t.group_id == group_id and t.state not in ACTIVE_STATES]
        return max(closed) if closed else None

    async def update_trip(self, trip_id, **fields):
        trip = self.trips[trip_id].model_copy(update={**fields, "updated_at": now()})
        self.trips[trip_id] = trip
        return trip

    async def save_participants(self, trip_id, people):
        self.participants[trip_id] = list(people)

    async def get_participants(self, trip_id):
        return self.participants.get(trip_id, [])

    async def save_options(self, trip_id, options):
        saved = [o.model_copy(update={"id": str(uuid.uuid4()), "chosen": False}) for o in options]
        self.options[trip_id] = saved
        return saved

    async def get_options(self, trip_id):
        return self.options.get(trip_id, [])

    async def choose_option(self, trip_id, option_id):
        self.options[trip_id] = [o.model_copy(update={"chosen": o.id == option_id})
                                 for o in self.options.get(trip_id, [])]
        await self.update_trip(trip_id, chosen_option_id=option_id)

    async def save_booking(self, booking):
        b = booking.model_copy(update={"id": booking.id or str(uuid.uuid4())})
        self.bookings[b.id] = b
        return b

    async def update_booking(self, booking_id, **fields):
        b = self.bookings[booking_id].model_copy(update=fields)
        self.bookings[booking_id] = b
        return b

    async def get_bookings(self, trip_id):
        return [b for b in self.bookings.values() if b.trip_id == trip_id]


# =====================================================================
class PostgresStore(Store):
    """asyncpg-backed. Works with Supabase direct, session pooler, and transaction pooler."""

    TRIP_COLS = "id, group_id, group_name, state, chosen_option_id, itinerary, approved_by, " \
                "approved_at, history_start, created_at, updated_at"

    def __init__(self, dsn: str) -> None:
        self.dsn = dsn
        self.pool = None

    async def connect(self) -> None:
        import asyncpg

        async def _init(conn):
            for t in ("json", "jsonb"):
                await conn.set_type_codec(t, encoder=json.dumps, decoder=json.loads, schema="pg_catalog")

        # statement_cache_size=0: required behind Supabase's transaction pooler (port 6543 /
        # PgBouncer-style), which can't hold prepared statements. Harmless elsewhere.
        self.pool = await asyncpg.create_pool(self.dsn, min_size=1, max_size=5,
                                              statement_cache_size=0, init=_init)

    async def close(self) -> None:
        if self.pool:
            await self.pool.close()

    # ---- helpers ----
    @staticmethod
    def _trip(row) -> Trip:
        d = dict(row)
        d["id"] = str(d["id"])
        d["chosen_option_id"] = str(d["chosen_option_id"]) if d["chosen_option_id"] else None
        return Trip(**d)

    # ---- messages ----
    async def save_message(self, msg):
        row = await self.pool.fetchrow(
            """insert into messages (group_id, trip_id, external_id, sender_id, sender_name,
                                     text, tagged, is_bot, sent_at)
               values ($1, $2, $3, $4, $5, $6, $7, $8, $9)
               on conflict (group_id, external_id) where external_id is not null do nothing
               returning id""",
            msg.group_id, msg.trip_id, msg.external_id, msg.sender_id, msg.sender_name,
            msg.text, msg.tagged, msg.is_bot, msg.sent_at)
        return row is not None

    async def get_messages(self, group_id, since=None, limit=100, include_bot=True):
        rows = await self.pool.fetch(
            """select * from (
                 select * from messages
                 where group_id = $1 and ($2::timestamptz is null or sent_at > $2)
                   and ($3 or not is_bot)
                 order by sent_at desc, id desc limit $4
               ) t order by sent_at, id""",
            group_id, since, include_bot, limit)
        return [Message(**{**dict(r), "trip_id": str(r["trip_id"]) if r["trip_id"] else None})
                for r in rows]

    # ---- trips ----
    async def create_trip(self, group_id, group_name=""):
        row = await self.pool.fetchrow(
            f"insert into trips (group_id, group_name, history_start) values ($1, $2, $3) "
            f"returning {self.TRIP_COLS}",
            group_id, group_name, await self.last_closed_at(group_id))
        return self._trip(row)

    async def get_trip(self, trip_id):
        row = await self.pool.fetchrow(f"select {self.TRIP_COLS} from trips where id = $1", uuid.UUID(trip_id))
        return self._trip(row) if row else None

    async def get_active_trip(self, group_id):
        row = await self.pool.fetchrow(
            f"select {self.TRIP_COLS} from trips where group_id = $1 "
            f"and state not in ('CONFIRMED','CANCELLED') order by created_at desc limit 1", group_id)
        return self._trip(row) if row else None

    async def last_closed_at(self, group_id):
        return await self.pool.fetchval(
            "select max(updated_at) from trips where group_id = $1 and state in ('CONFIRMED','CANCELLED')",
            group_id)

    async def update_trip(self, trip_id, **fields):
        allowed = {"group_name", "state", "chosen_option_id", "itinerary", "approved_by",
                   "approved_at", "history_start"}
        bad = set(fields) - allowed
        if bad:
            raise ValueError(f"unknown trip fields: {bad}")
        fields = {k: (v.value if isinstance(v, TripState) else v) for k, v in fields.items()}
        if "chosen_option_id" in fields and fields["chosen_option_id"]:
            fields["chosen_option_id"] = uuid.UUID(fields["chosen_option_id"])
        sets = ", ".join(f"{k} = ${i}" for i, k in enumerate(fields, start=2))
        sets = f"{sets}, updated_at = now()" if sets else "updated_at = now()"
        row = await self.pool.fetchrow(
            f"update trips set {sets} where id = $1 returning {self.TRIP_COLS}",
            uuid.UUID(trip_id), *fields.values())
        return self._trip(row)

    # ---- participants ----
    async def save_participants(self, trip_id, people):
        async with self.pool.acquire() as conn, conn.transaction():
            await conn.execute("delete from participants where trip_id = $1", uuid.UUID(trip_id))
            for p in people:
                d = p.model_dump(mode="json")
                await conn.execute(
                    """insert into participants (trip_id, name, origin_city, origin_airport, budget_max,
                         currency, available_dates, vibe, dealbreakers, notes)
                       values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)""",
                    uuid.UUID(trip_id), d["name"], d["origin_city"], d["origin_airport"], d["budget_max"],
                    d["currency"], d["available_dates"], d["vibe"], d["dealbreakers"], d["notes"])

    async def get_participants(self, trip_id):
        rows = await self.pool.fetch(
            "select name, origin_city, origin_airport, budget_max, currency, available_dates, vibe, "
            "dealbreakers, notes from participants where trip_id = $1 order by name", uuid.UUID(trip_id))
        out = []
        for r in rows:
            d = dict(r)
            d["budget_max"] = float(d["budget_max"]) if d["budget_max"] is not None else None
            for k in ("available_dates", "vibe", "dealbreakers"):
                d[k] = d[k] or []
            out.append(Participant(**d))
        return out

    # ---- options ----
    async def save_options(self, trip_id, options):
        tid = uuid.UUID(trip_id)
        async with self.pool.acquire() as conn, conn.transaction():
            await conn.execute("delete from options where trip_id = $1", tid)
            saved = []
            for o in options:
                oid = await conn.fetchval(
                    """insert into options (trip_id, position, destination, destination_airport, start_date,
                         end_date, est_cost_per_person, why_it_works, tradeoffs)
                       values ($1,$2,$3,$4,$5,$6,$7,$8,$9) returning id""",
                    tid, o.position, o.destination, o.destination_airport, o.start_date, o.end_date,
                    o.est_cost_per_person, o.why_it_works, o.tradeoffs)
                saved.append(o.model_copy(update={"id": str(oid), "chosen": False}))
        return saved

    async def get_options(self, trip_id):
        rows = await self.pool.fetch(
            "select id, position, destination, destination_airport, start_date, end_date, "
            "est_cost_per_person, why_it_works, tradeoffs, chosen from options "
            "where trip_id = $1 order by position", uuid.UUID(trip_id))
        return [Option(**{**dict(r), "id": str(r["id"]),
                          "est_cost_per_person": float(r["est_cost_per_person"])
                          if r["est_cost_per_person"] is not None else None}) for r in rows]

    async def choose_option(self, trip_id, option_id):
        await self.pool.execute("update options set chosen = (id = $2) where trip_id = $1",
                                uuid.UUID(trip_id), uuid.UUID(option_id))
        await self.update_trip(trip_id, chosen_option_id=option_id)

    # ---- bookings ----
    BOOKING_COLS = ["trip_id", "kind", "participant", "status", "provider_ref", "cost", "currency",
                    "details", "skyvern_run_id", "live_url", "recording_url", "screenshot_urls",
                    "failure_reason"]

    @staticmethod
    def _booking(row) -> Booking:
        d = dict(row)
        d["id"], d["trip_id"] = str(d["id"]), str(d["trip_id"])
        d["cost"] = float(d["cost"]) if d["cost"] is not None else None
        d.pop("created_at", None), d.pop("updated_at", None)
        return Booking(**d)

    async def save_booking(self, booking):
        d = booking.model_dump(mode="json")
        d["trip_id"] = uuid.UUID(d["trip_id"])
        cols = ", ".join(self.BOOKING_COLS)
        params = ", ".join(f"${i}" for i in range(1, len(self.BOOKING_COLS) + 1))
        row = await self.pool.fetchrow(
            f"insert into bookings ({cols}) values ({params}) returning *",
            *[d[c] for c in self.BOOKING_COLS])
        return self._booking(row)

    async def update_booking(self, booking_id, **fields):
        bad = set(fields) - set(self.BOOKING_COLS)
        if bad:
            raise ValueError(f"unknown booking fields: {bad}")
        sets = ", ".join(f"{k} = ${i}" for i, k in enumerate(fields, start=2))
        row = await self.pool.fetchrow(
            f"update bookings set {sets}, updated_at = now() where id = $1 returning *",
            uuid.UUID(booking_id), *fields.values())
        return self._booking(row)

    async def get_bookings(self, trip_id):
        rows = await self.pool.fetch("select * from bookings where trip_id = $1 order by created_at",
                                     uuid.UUID(trip_id))
        return [self._booking(r) for r in rows]


def make_store(database_url: str) -> Store:
    return PostgresStore(database_url) if database_url else MemoryStore()
