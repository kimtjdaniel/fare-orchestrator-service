-- Fare fare-orchestrator-service schema. Paste into Supabase SQL editor, or: psql "$DATABASE_URL" -f db/schema.sql
-- Safe to re-run: everything is IF NOT EXISTS.

-- gen_random_uuid() is built into Postgres 13+ (Supabase included), so no extension needed.

-- One row per trip. A group has at most one *active* trip (state not CONFIRMED/CANCELLED).
create table if not exists trips (
    id                uuid primary key default gen_random_uuid(),
    group_id          text not null,
    group_name        text,
    state             text not null default 'COLLECTING',
    chosen_option_id  uuid,
    itinerary         jsonb,          -- {flights: {name: offer}, hotel: offer, totals: {...}}
    approved_by       text,
    approved_at       timestamptz,
    history_start     timestamptz,    -- only messages after this feed extraction
    created_at        timestamptz not null default now(),
    updated_at        timestamptz not null default now(),
    constraint trips_state_check check (state in (
        'COLLECTING','AWAITING_CHOICE','SEARCHING','AWAITING_APPROVAL',
        'BOOKING','CONFIRMED','CANCELLED'))
);
create index if not exists trips_group_idx on trips (group_id, created_at desc);

-- Every group message, human or bot. Saved even before a trip exists ("scraping is just listening").
create table if not exists messages (
    id           bigserial primary key,
    group_id     text not null,
    trip_id      uuid references trips(id) on delete set null,
    external_id  text,                -- channel's message id, used to drop webhook retries
    sender_id    text not null,
    sender_name  text not null,
    text         text not null,
    tagged       boolean not null default false,
    is_bot       boolean not null default false,
    sent_at      timestamptz not null default now()
);
create index if not exists messages_group_idx on messages (group_id, sent_at);
create unique index if not exists messages_external_uidx on messages (group_id, external_id)
    where external_id is not null;

-- One row per person per trip, written by preference extraction.
create table if not exists participants (
    id               uuid primary key default gen_random_uuid(),
    trip_id          uuid not null references trips(id) on delete cascade,
    name             text not null,
    origin_city      text,
    origin_airport   text,            -- IATA, e.g. YVR
    budget_max       numeric,         -- per person, all-in
    currency         text default 'CAD',
    available_dates  jsonb,           -- [{start: 'YYYY-MM-DD', end: 'YYYY-MM-DD'}]
    vibe             jsonb,           -- ["beach", "food"]
    dealbreakers     jsonb,
    notes            text,
    unique (trip_id, name)
);

-- Destination options proposed by reconciliation (2-3 per round).
create table if not exists options (
    id                   uuid primary key default gen_random_uuid(),
    trip_id              uuid not null references trips(id) on delete cascade,
    position             int not null,        -- 1, 2, 3 as shown in chat
    destination          text not null,
    destination_airport  text not null,
    start_date           date not null,
    end_date             date not null,
    est_cost_per_person  numeric,
    why_it_works         text,
    tradeoffs            text,
    chosen               boolean not null default false,
    created_at           timestamptz not null default now()
);

-- Flight and hotel bookings. Hotel rows carry the Skyvern run so the dashboard can show progress.
create table if not exists bookings (
    id               uuid primary key default gen_random_uuid(),
    trip_id          uuid not null references trips(id) on delete cascade,
    kind             text not null check (kind in ('flight','hotel')),
    participant      text,                -- flights are per person; null for the shared hotel
    status           text not null default 'pending'
                     check (status in ('pending','running','confirmed','failed')),
    provider_ref     text,                -- PNR / confirmation number
    cost             numeric,
    currency         text default 'CAD',
    details          jsonb,
    skyvern_run_id   text,
    live_url         text,
    recording_url    text,
    screenshot_urls  jsonb,
    failure_reason   text,
    created_at       timestamptz not null default now(),
    updated_at       timestamptz not null default now()
);
create index if not exists bookings_trip_idx on bookings (trip_id);

-- Supabase realtime: lets the Next.js dashboard subscribe to changes.
-- Uncomment when running on Supabase (the publication doesn't exist on plain Postgres).
-- alter publication supabase_realtime add table trips, options, bookings, participants;
