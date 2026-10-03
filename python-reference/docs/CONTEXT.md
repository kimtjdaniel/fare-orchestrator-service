# Project context (read on demand)

Background for the Fare brain: the hackathon plan, who does what, why the code is shaped the way
it is, and what was verified in the docs. `CLAUDE.md` has the working rules; this file has the why.

---

## 1. The product

Inspired by Unicorn Mafia's winning hackathon project (yate.travel): an AI agent that joins a group
chat, reconciles conflicting preferences (destination, dates, budget), searches and books flights
and hotels, and asks for approval in the chat before buying anything.

**Scope.** Keep: group chat intake → preference reconciliation → flight/hotel search → ✅ approval in
chat → booking → cost split. One "hero" feature beyond that (visible browser booking or a phone call).
Fake or defer: real payments (Stripe test mode only), phone-call intake, multi-currency, price
monitoring, accounts/auth, post-booking changes, more than one active trip per group.

## 2. Team

| Role | Owner | Owns |
|---|---|---|
| P1 Agent brain | **Daniel (me)** | this repo: orchestrator, prompts, schemas, state machine, Postgres |
| P2 Messaging + infra | teammate | WhatsApp robot (whatsapp-web.js) + Telegram bot, deploys |
| P3 Travel tools | teammate | Duffel/Amadeus search + booking, implementing `app/tools/flights.py`, `hotels.py` |
| P4 Demo + hero | teammate | fake hotel checkout page, Skyvern booking, live page, ElevenLabs call, pitch |

Three team rules: mock first, real later (nobody blocks on anyone); hour 6 and hour 12 checkpoints
are hard stops (everyone fixes the critical path if red); the demo path beats feature count.

## 3. Timeline (24h)

| Hours | P1 (me) | Checkpoint |
|---|---|---|
| 0–1 | Agree demo script + exact friend messages; walk team through `CONTRACTS.md` | |
| 1–3 | Supabase + real Claude on; schema smoke test | |
| 3–6 | Preference extraction tuned on the seeded transcript | **6: @mention → preference reply** |
| 6–9 | Reconciliation prompt; option validation | |
| 9–12 | Summary + approval gate with real tools wired in | **12: full happy path once** |
| 12–15 | Sleep | |
| 15–18 | Edge cases, revision loop | |
| 18–22 | Run the demo ×5, fix every stumble, polish wording | **22: code freeze** |
| 22–24 | Rehearse pitch | |

## 4. Demo script (4 minutes)

1. Group chat already open: Maya (Vancouver, wants beach), Jordan (Toronto, wants a food city,
   two date windows), Sam (Calgary, $800 max, busy one weekend).
2. Jordan: "@Fare figure this out".
3. Agent replies within ~10s: overlapping dates, 2–3 destinations with trade-offs, cost per person,
   dashboard link. Dashboard updates live.
4. Someone replies "1" → summary with flights per person, hotel, per-person totals, ✅/❌ buttons.
5. Organizer replies ✅ → booking runs visibly (Skyvern live view or screen share).
6. Optional: agent calls the hotel (ElevenLabs) to confirm early check-in.
7. Confirmation + "who owes whom" posted to the group. Say "sandbox" once, early.

The exact transcript lives in `tests/fixtures/demo_transcript.json`; canned Claude outputs that match
it are in `app/llm/mock_data.py`. Only Nov 20–23, 2026 works for all three.

## 5. Decisions and why

- **Telegram as the guaranteed channel, WhatsApp puppet as the hero.** Meta's official WhatsApp
  APIs can't put a bot in a friends' group without weeks of business verification. A puppet
  account via whatsapp-web.js works but is against ToS and may be banned. Telegram bots in groups
  are official and support inline ✅/❌ buttons. Both sit behind `send()` and the same webhook payload.
- **Stage-driven orchestration, not one free-roaming agent.** Each stage asks Claude for a specific
  JSON shape; code decides what happens next. More reliable on stage and makes the approval gate
  impossible to bypass. The free-form tool loop is only used for questions ("@Fare is it warm there?").
- **Structured outputs instead of forced tool calls.** Forced `tool_choice` isn't supported on
  Sonnet 5.5 / Opus 5.5 (found while reading docs on Oct 2).
- **Chat messages built from templates.** Options, summary and confirmation text come from
  `formatting.py`: instant, free, never malformed. Claude writes the content (intro, trade-offs).
- **Async replies.** The webhook acknowledges instantly and replies through `send()`, so a
  multi-minute Skyvern run can't time out WhatsApp or Telegram.
- **Booking strategy (hybrid).** Flights via Duffel test orders (real PNR, no money). Hotel via
  Skyvern driving P4's own fake checkout page (the visual "watch it book" moment). Never automate
  a real site with a real card.
- **Approver pays.** The split assumes everything is charged to whoever sent ✅; everyone else owes
  them their own flight plus an equal hotel share.
- **"Watch it book" options.** B (baseline): Skyvern's live view link / a live page. A (hero, brittle):
  the puppet account video-calls the group and screen-shares the browser. C (fallback): screenshots
  in chat. Skyvern returns `screenshot_urls` and `recording_url`, which cover C and the backup video.

## 6. Research findings (verified Oct 2, 2026)

**Claude API.** Models: `claude-sonnet-5-5` ($2/$10 per MTok), `claude-haiku-4-5-20251001`
($1/$5, fastest), `claude-opus-5-5`. Structured outputs via `output_config.format` (no beta header),
supported on Sonnet 5.5 and Haiku 4.5. First request with a new schema has grammar-compilation
latency (cached ~24h). `anthropic` Python SDK 1.11 has `output_config` on `messages.create`.

**Skyvern.** `pip install skyvern` (cloud SDK, Python 3.11–3.13). API key from app.skyvern.com →
Settings. `run_task` returns immediately with a `run_id` unless `wait_for_completion=True`; poll with
`get_run`. `max_steps` caps cost (run ends `timed_out`). Every run is recorded; live view at
app.skyvern.com, which may require login (check in a private window before relying on it for
judges). Free plan: 5,000 one-time credits ≈ 200 actions, 1 concurrent run; Hobby $29/mo ≈ 1,200
actions. Cloud browsers can't reach localhost. Keep the fake checkout form plain HTML (no Stripe
card iframe).

**Supabase.** Connect button shows the strings. Direct `db.<ref>.supabase.co:5432` is IPv6-only on
free; session pooler `aws-…pooler.supabase.com:5432` is IPv4 (use this); transaction pooler `:6543`
needs prepared statements off.

**Telegram.** BotFather `/newbot`, then `/setprivacy` → Disable, then (re-)add the bot to the group,
because privacy changes only apply on re-add. `setWebhook` needs HTTPS on 443/80/88/8443;
`secret_token` arrives as header `X-Telegram-Bot-Api-Secret-Token`; non-2xx responses are retried.
Group → supergroup upgrades change the chat ID.

**Railway.** Detects Python from `requirements.txt`; version from `.python-version` (default 3.13);
`railway.json` sets the start command; Settings → Networking → Generate Domain for a public URL.

**WhatsApp (whatsapp-web.js).** Accounts can appear under several ID formats, so mention detection
by ID can miss; the brain also treats a typed "@Fare" as a mention.

## 7. Risks and fallbacks

| Risk | Fallback |
|---|---|
| Webhook URL changes (ngrok) | Deploy to Railway for the demo |
| WhatsApp burner banned | Spare SIM; switch `MESSAGING_BACKEND=telegram` |
| Skyvern slow/fails on stage | Mock flag + cached screenshots + backup video |
| Flight API rate limits | Cache the demo route |
| Claude slow on stage | "Reading the chat…" ack; switch to Haiku; seed exact transcript for cache hits |
| Brain can't reach robot's `/send` | Run robot + brain on the same machine, or tunnel the robot |

## Sources

- Claude models: https://platform.claude.com/docs/en/about-claude/models/overview
- Tool use / tool_choice: https://platform.claude.com/docs/en/agents-and-tools/tool-use/define-tools
- Structured outputs: https://platform.claude.com/docs/en/build-with-claude/structured-outputs
- Skyvern quickstart: https://www.skyvern.com/docs/developers/getting-started/quickstart.md
- Skyvern run_task: https://www.skyvern.com/docs/sdk-reference/tasks/run-task.md
- Skyvern billing: https://www.skyvern.com/docs/cloud/account-settings/billing-usage.md
- Supabase connections: https://supabase.com/docs/guides/database/connecting-to-postgres
- Telegram setWebhook: https://core.telegram.org/bots/api#setwebhook
- Telegram privacy mode: https://core.telegram.org/bots/features#privacy-mode
- Railway FastAPI: https://docs.railway.com/guides/fastapi
- Railpack Python: https://railpack.com/languages/python.md
- Claude Code memory (CLAUDE.md, rules): https://code.claude.com/docs/en/memory
