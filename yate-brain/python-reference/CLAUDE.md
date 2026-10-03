# Yate brain — project instructions

24-hour hackathon project (Oct 3, 2026): an AI travel agent that sits in a group chat, reconciles
friends' conflicting trip preferences, proposes options, waits for ✅, then books flights + hotel.
This repo is the **brain** (FastAPI + Postgres + Claude). I own it (role P1) plus the Postgres setup.
The demo is the product: optimize for a 4-minute scripted demo that works every time.

Background, team plan, decision log and research notes: read `docs/CONTEXT.md` when you need them.
Service setup: `SETUP.md`. Interfaces with teammates: `CONTRACTS.md`.

## Commands

```bash
pip install -r requirements.txt && cp .env.example .env   # Python 3.11–3.13 (3.13 pinned)
pytest -q                                                 # 26 offline tests (29 with TEST_DATABASE_URL)
TEST_DATABASE_URL=postgresql://... pytest -q              # also runs flow tests against Postgres
python scripts/simulate.py                                # play the group chat in the terminal
uvicorn app.main:app --reload --port 8000                 # dev server
python scripts/smoke.py db|claude|schemas|telegram|skyvern   # check a real service
```

Run `pytest -q` after every change. All tests must pass with the default mocks.

## Layout

- `app/main.py` — FastAPI: `POST /webhook`, `POST /telegram/webhook`, `GET /trips/{id}`, `GET /groups/{gid}/trip`, `/health`
- `app/orchestrator.py` — `Brain.handle()`: routes each message by trip state
- `app/state.py` — allowed state transitions + `assert_can_book()` approval gate
- `app/store.py` — `MemoryStore` and `PostgresStore` with identical interfaces
- `app/prompts.py` — system prompts + JSON schemas Claude fills
- `app/formatting.py` — chat message templates (built by code, not Claude)
- `app/messaging.py` — `send(group_id, text, buttons)`: console | robot | telegram
- `app/llm/` — `ClaudeLLM` (structured outputs, tool loop, disk cache), `MockLLM`, `mock_data.py`
- `app/tools/` — `flights.py`, `hotels.py` (P3), `browser.py` Skyvern (P4), `split.py` (done)
- `db/schema.sql` — idempotent schema; `tests/fixtures/demo_transcript.json` — the demo chat

## Flow

```
COLLECTING --@mention--> ack, extract prefs, propose 2-3 options --> AWAITING_CHOICE
AWAITING_CHOICE --"2"--> search flights per person + hotel, post summary --> AWAITING_APPROVAL
AWAITING_APPROVAL --✅--> book flights, Skyvern hotel, cost split --> CONFIRMED
                  --❌--> AWAITING_CHOICE      any active state --"cancel"--> CANCELLED
```

## Invariants (do not break these)

1. **Code moves the trip state, never Claude.** Every change goes through `store.set_state()`,
   which enforces `state.ALLOWED`. Claude only returns JSON (prefs, options, intent) or chat text.
2. **Booking is never exposed to Claude.** `book_flight` / `start_hotel_booking` are called only
   from `Brain.book()`, after `assert_can_book()`. Never add them to `AGENT_TOOLS`.
3. **The webhook returns instantly.** Work runs as a background task; replies go out via `send()`,
   never in the HTTP response. Long steps (Skyvern takes minutes) must not block the channel.
4. **Obvious replies skip Claude.** `"1"`, `"✅"`, `"❌"` are matched by regex in `Brain.interpret()`;
   untagged chatter is saved but never sent to Claude. The happy path is exactly 2 Claude calls.
   Tests assert this (`b.llm.calls`); keep it true.
5. **Retry-safe booking.** A hotel failure leaves flights booked and returns to AWAITING_APPROVAL;
   ✅ again must not re-book confirmed flights.
6. **Single process.** The per-group `asyncio.Lock` is in memory. No `--workers`.
7. **Mocks stay the default.** `MOCK_LLM`, `MOCK_TRAVEL`, `MOCK_BROWSER` default to true; tests force
   them on in `tests/conftest.py`. Never make a test depend on a real API or key.

## Claude API rules (verified against docs, Oct 2026)

- Model: `claude-sonnet-5-5` (env `ANTHROPIC_MODEL`). Fallback for speed: `claude-haiku-4-5-20251001`.
- **Sonnet 5.5 / Opus 5.5 do not support forced tool use** (`tool_choice: {"type": "tool"|"any"}`).
  Get JSON via **structured outputs**: `messages.create(..., output_config={"format": {"type":
  "json_schema", "schema": ...}})`, then `json.loads` the text block. This is what `ClaudeLLM.structured` does.
- Structured-output schemas: every object has `"additionalProperties": false`; no `format`, `default`,
  `minItems`/`maxItems`, `minLength`, `pattern`, or `["x", "null"]` types. Optional = leave out of `required`.
  Dates are plain strings with `"description": "YYYY-MM-DD"`, so validate them in code (Pydantic).
- The agent loop (`ClaudeLLM.agent`) uses normal tools with `tool_choice` auto. Those tool schemas may use `format`.
- Disk cache (`LLM_CACHE_DIR`) keys on the exact request; it only helps identical replays.

## Credits and cost

- Don't flip mocks off or run `smoke.py claude|schemas|skyvern` unless I ask. Claude costs money;
  Skyvern free tier is ~200 browser actions total, 1 concurrent run, shared with P4.
- Keep `SKYVERN_MAX_STEPS` (25) as the per-run cap.

## Conventions

- Python 3.13, async everywhere, Pydantic v2 models in `app/models.py`.
- Change a tool signature or the webhook payload only together with `CONTRACTS.md`, and tell me,
  since a teammate depends on it.
- Changing a stored field means updating `db/schema.sql`, `models.py`, and both stores in `store.py`.
- Stubs for teammates are marked `TODO(P3)` / `TODO(P4)`; my remaining work is marked `TODO(P1)`.
- Comments in `.env.example` go on their own line (`KEY=  # note` makes the note the value).
- Chat wording lives in `formatting.py` and prompts in `prompts.py`. Keep chat text short, no markdown headers.
- When the demo script changes, update `tests/fixtures/demo_transcript.json` and `app/llm/mock_data.py` together.

## Current status and next tasks

Done: full happy path on mocks (memory + Postgres), state machine, approval gate, Telegram adapter,
Skyvern wrapper (signatures verified against `skyvern` 1.0.55), cost split, smoke scripts.
Not yet verified live: real Claude calls (need API key), Supabase, Telegram webhook, Skyvern.

Next, in order (plan hours in brackets):
1. [0–1] Lock the demo script with the team; update fixture + mock data.
2. [1–3] Supabase + real Claude on: `smoke.py db`, `smoke.py schemas`; paste real outputs into `mock_data.py`.
3. [3–6] Tune `EXTRACT_SYSTEM` on the real transcript. Hour-6 checkpoint: @mention → preference reply.
4. [6–9] `TODO(P1)` in `Brain.plan`: validate options against every participant's dates and budget;
   re-ask Claude once with the violations if invalid.
5. [9–12] Flip `MOCK_TRAVEL` / `MOCK_BROWSER` as P3/P4 land. Hour-12 checkpoint: full path end to end.
6. [15–18] Revision loop ("make it cheaper"), missing-info follow-ups, name mismatches between
   chat display names and extracted participant names.
7. [18–22] Rehearse; polish `formatting.py`. Code freeze at hour 22.
