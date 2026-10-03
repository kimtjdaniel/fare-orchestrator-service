# Fare brain

The FastAPI "brain" of the group-travel agent: reads the group chat, reconciles everyone's
preferences with Claude, searches flights and hotels, waits for ✅, then books.

```bash
pip install -r requirements.txt && cp .env.example .env
pytest -q                       # 26 offline tests (29 with TEST_DATABASE_URL)
python scripts/simulate.py      # play the group chat in your terminal
uvicorn app.main:app --reload   # run the server on :8000
```

Everything is **mocked by default**: no keys, no credits. Real services switch on one flag at a time
(see `SETUP.md` §7). Team interfaces are in `CONTRACTS.md`.
Claude Code picks up `CLAUDE.md` (rules), `.claude/rules/` (per-folder rules) and `docs/CONTEXT.md` (background).

## Layout

```
app/
  main.py          FastAPI: /webhook, /telegram/webhook, /trips/{id}, /health
  orchestrator.py  Brain: routes each message by trip state; plan → choose → approve → book
  state.py         Trip state machine + the approval gate (assert_can_book)
  store.py         MemoryStore / PostgresStore behind one interface
  prompts.py       System prompts + JSON schemas Claude fills
  formatting.py    Chat message templates (options, summary, confirmation)
  messaging.py     send(): console | WhatsApp robot | Telegram
  llm/             ClaudeLLM (structured outputs, tool loop, disk cache) and MockLLM
  tools/           flights, hotels (P3), browser/Skyvern (P4), split (done)
db/schema.sql      Postgres / Supabase schema
scripts/           simulate.py (play the chat), smoke.py (check each real service)
tests/             state machine, split, full flow (memory + Postgres), API, Claude client
```

## How a message flows

```
group msg → /webhook → 200 instantly → Brain.handle() in background
  COLLECTING        + @mention  → Claude: extract prefs → Claude: 2-3 options   → AWAITING_CHOICE
  AWAITING_CHOICE   + "2"       → search flights per person + hotel → summary   → AWAITING_APPROVAL
  AWAITING_APPROVAL + ✅        → book flights → Skyvern hotel → cost split      → CONFIRMED
                    + ❌        → options again                                  → AWAITING_CHOICE
```

Design choices worth knowing:
- **Code moves the state, never Claude.** Claude only produces JSON (preferences, options, intent)
  and free-form answers. Booking functions aren't exposed to Claude at all, and
  `assert_can_book()` refuses unless the trip is in `BOOKING` with an approver recorded.
- **Obvious replies skip Claude.** `"1"`, `"✅"`, `"❌"` are matched by regex; ordinary chatter is
  saved but never sent to Claude. Only @mentions that need understanding cost an LLM call.
  The whole happy path is **2 Claude calls**.
- **Replies are async.** The webhook returns immediately; messages go out through `send()`.
  Long steps (Skyvern takes minutes) can't time out the chat channel.
- **Retry-safe booking.** If the hotel fails, flights stay booked, and ✅ again retries only the hotel.

## Tomorrow's work, by plan hour

| Hours | Task | Where |
|---|---|---|
| 0–1 | Lock the demo script; update the transcript + mock data to match | `tests/fixtures/demo_transcript.json`, `app/llm/mock_data.py` |
| 0–1 | Walk teammates through `CONTRACTS.md` | — |
| 1–3 | Supabase + real Claude on (`smoke.py db`, `smoke.py schemas`) | `.env` |
| 3–6 | Tune extraction prompt on the real transcript; **hour-6 checkpoint** | `prompts.EXTRACT_SYSTEM` |
| 6–9 | ✅ Done: validate options against dates/budgets; re-ask Claude once if every option is invalid | `orchestrator.propose` |
| 9–12 | Wire P3/P4's real tools (flip flags); **hour-12 checkpoint** | `SETUP.md` §7 |
| 15–18 | ✅ Done: revision loop ("make it cheaper") re-proposes without re-extracting; missing-info flags block proposing; chat-name/participant-name mismatches resolve the right payer | `orchestrator.replan`, `orchestrator.plan`, `orchestrator.match_name` |
| 18–22 | Polish message wording during rehearsals | `formatting.py` |

Search `TODO(` to find every stub left for a teammate.
