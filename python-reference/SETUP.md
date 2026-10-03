# Setup guide (verified against current docs, Oct 2 2026)

Each section ends with a one-line smoke test so you know it works before moving on.
Do sections 1–3 tonight if you can; they're the ones that block you tomorrow.

---

## 1. Local setup (5 min)

Requires **Python 3.11–3.13** (Skyvern's SDK refuses other versions). Check with `python --version`.

```bash
cd python-reference
python -m venv .venv && source .venv/bin/activate      # Windows: .venv\Scripts\activate
pip install -r requirements.txt
cp .env.example .env
pytest -q                                              # 26 offline tests (29 with TEST_DATABASE_URL)
python scripts/simulate.py                             # plays the demo chat; type "Sam: 1" then "Jordan: ✅"
```

Everything runs mocked until you flip flags in `.env`, so this costs nothing.

## 2. Claude API (5 min)

1. Create an API key in the Claude Console (platform.claude.com) and add credits.
2. In `.env`: `ANTHROPIC_API_KEY=...`, keep `MOCK_LLM=true` for now.
3. Smoke test: `python scripts/smoke.py claude` (one tiny call, a fraction of a cent).
4. Then `python scripts/smoke.py schemas` (~2 cents). It sends all three brain schemas with the demo
   transcript and proves the API accepts them. Paste its output into `app/llm/mock_data.py`
   to make your mocks match real Claude behaviour.

**Gotchas already handled in the code:**
- **Sonnet 5.5 and Opus 5.5 don't support forced tool use** (`tool_choice: {"type": "tool"}`),
  the common way older tutorials get JSON out of Claude. The brain uses **structured outputs**
  (`output_config` with a JSON schema) instead, which Sonnet 5.5 supports. If you follow an older tutorial,
  don't copy its `tool_choice` trick.
- Structured-output schemas can't use `format`, `default`, `minItems`/`maxItems`, or
  `["string", "null"]` types, and every object needs `"additionalProperties": false`.
  The schemas in `app/prompts.py` follow these rules. Keep them that way when you edit.
- The first call with a new schema is slower (grammar compilation, cached ~24h). Run
  `smoke.py schemas` before the demo so the cache is warm.

**Model choice:** `claude-sonnet-5-5` ($2 / $10 per million input/output tokens) is the default.
If replies feel slow on stage, try `ANTHROPIC_MODEL=claude-haiku-4-5-20251001` ($1 / $5), which is fastest.

**Saving credits:** with `LLM_CACHE_DIR=.llm_cache`, identical requests are answered from disk.
Replaying the demo transcript 20 times costs one set of calls. Delete the folder to force fresh answers.

## 3. Supabase / Postgres (10 min)

1. Create a project at supabase.com (free tier is fine). Save the database password.
2. Click **Connect** at the top of the dashboard and copy a connection string:
   - **Session pooler** (`aws-…pooler.supabase.com:5432`): **use this one.** Works over IPv4 everywhere.
   - Direct (`db.<ref>.supabase.co:5432`): IPv6-only on the free plan, so it may fail from Railway or your laptop.
   - Transaction pooler (`:6543`): for serverless. It also works here, because the code disables
     prepared statements (`statement_cache_size=0`), which this mode requires.
3. Put it in `.env` as `DATABASE_URL=postgresql://postgres.<ref>:<password>@aws-…pooler.supabase.com:5432/postgres`.
   URL-encode special characters in the password (`@` → `%40`, `#` → `%23`).
4. Smoke test: `python scripts/smoke.py db`. It applies `db/schema.sql` and lists the tables.
   (Or paste `db/schema.sql` into the Supabase SQL editor.)
5. For the live dashboard, run this once in the SQL editor:
   ```sql
   alter publication supabase_realtime add table trips, options, bookings, participants;
   ```
   The dashboard can then subscribe to row changes instead of polling.

**No database yet?** Leave `DATABASE_URL` empty and the brain uses an in-memory store with identical behaviour
(data is lost on restart). Teammates can work against that while you set up Supabase.

## 4. Telegram bot, the guaranteed channel (10 min)

1. In Telegram, message **@BotFather** → `/newbot` → pick a name and username → copy the token.
2. `/setprivacy` → choose your bot → **Disable**. Otherwise the bot only sees commands and replies,
   not the group's conversation.
3. **Add the bot to the test group after step 2.** Privacy changes only apply when the bot is
   (re-)added, so if it's already in the group, remove it and add it again.
4. `.env`: `TELEGRAM_BOT_TOKEN=...`, `TELEGRAM_BOT_USERNAME=<username without @>`,
   `TELEGRAM_WEBHOOK_SECRET=<any random string>`, `MESSAGING_BACKEND=telegram`.
5. Smoke test: `python scripts/smoke.py telegram`. Check `can_read_all_group_messages: True`.
6. Point Telegram at the brain. This needs a public **HTTPS** URL on port 443/80/88/8443:
   Railway (section 5), or `ngrok http 8000` for local testing.
   ```bash
   curl "https://api.telegram.org/bot$TELEGRAM_BOT_TOKEN/setWebhook" \
     -d "url=https://YOUR-APP.up.railway.app/telegram/webhook" \
     -d "secret_token=$TELEGRAM_WEBHOOK_SECRET" \
     -d 'allowed_updates=["message","callback_query"]'
   ```
   Re-run `smoke.py telegram` to confirm the webhook URL and that `last_error` is empty.

Telegram retries any non-2xx response. The brain answers 200 immediately and ignores duplicates by
`message_id`, so retries are harmless.

## 5. Railway deploy, a stable public URL (10 min)

`railway.json` in the repo already sets the start command
(`uvicorn app.main:app --host 0.0.0.0 --port $PORT`) and a `/health` check. Railway detects Python from
`requirements.txt`.

1. Push the repo to GitHub. In Railway: **New Project → Deploy from GitHub repo** (or run `railway login`,
   `railway init`, then `railway up` from the folder).
2. Service → **Variables**: paste everything from your `.env`.
3. Service → **Settings → Networking → Generate Domain**.
4. Smoke test: open `https://<domain>/health`. It shows which pieces are mocked vs real.

Use this URL, not ngrok, for the demo: ngrok URLs change on restart.

## 6. Skyvern, hotel booking (P4 owns; 10 min to verify)

1. Sign up at app.skyvern.com → **Settings** → copy the API key → `SKYVERN_API_KEY=...` in `.env`.
2. The SDK is in `requirements.txt` (verified: `skyvern` 1.0.55; `run_task` / `get_run` are async with
   `max_steps`, `wait_for_completion`, `timeout`).
3. Smoke test: `python scripts/smoke.py skyvern` runs 3 steps on example.com.
   Open the printed `app_url` in a **private window**. If it asks you to log in, judges can't open
   it from the chat, so show it on the demo laptop's screen instead.

**Budget:** the free plan is 5,000 one-time credits (~200 browser actions), 1 run at a time.
A checkout is ~10–25 actions, so expect ~10 full test runs. Keep `SKYVERN_MAX_STEPS=25` as the
per-run cap. If you'll rehearse a lot, the $29 Hobby plan gives ~1,200 actions/month.

**Skyvern Cloud cannot reach `localhost`.** `HOTEL_CHECKOUT_URL` must be P4's deployed page.

## 7. Turning the real pieces on, one at a time

Flip one flag, run `python scripts/simulate.py`, confirm the flow still completes, then move on.

| Order | Flag | Owner | Check |
|---|---|---|---|
| 1 | `DATABASE_URL=…` | you | `smoke.py db`, then simulate; rows appear in Supabase |
| 2 | `MOCK_LLM=false` | you | simulate; options now come from real Claude |
| 3 | `MESSAGING_BACKEND=telegram` | P2 | @mention the bot in the real group |
| 4 | `MOCK_TRAVEL=false` | P3 | real Duffel offers in the summary |
| 5 | `MOCK_BROWSER=false` | P4 | ✅ in chat → Skyvern run → confirmation |

If anything breaks on stage, flip that one flag back to `true`. The rest keeps working.

## 8. Friction to plan around (not fixable in code)

- **The brain must be able to reach the WhatsApp robot.** Replies go out via `POST {ROBOT_URL}/send`.
  If the brain runs on Railway and the robot runs on a laptop, Railway can't reach `localhost:3000`.
  Either run robot + brain on the same machine for the demo, or expose the robot with a tunnel and set
  `ROBOT_URL` to it. Telegram doesn't have this problem.
- **One webhook per Telegram bot.** Whoever ran `setWebhook` last receives every message, so a teammate
  testing with ngrok silently steals traffic from Railway. Make a separate dev bot per person and keep one
  demo bot pointed at Railway only.
- **Telegram group IDs can change.** Some settings (making the group public, certain admin changes)
  upgrade a group to a "supergroup" with a new chat ID, and the brain then treats it as a brand-new group
  mid-trip. Finish configuring the demo group *before* seeding it, and don't touch its settings after.
- **Run a single server process.** The per-group lock that stops two messages being processed at once
  lives in memory. Don't add `--workers` to the start command.
- **Dashboard: poll the brain instead of Supabase realtime.** `GET /trips/{id}` every 2s needs no
  Supabase keys in the frontend and can't be blocked by row-level-security settings. If you do use realtime
  and the dashboard receives no events, RLS policies are the usual cause.
- **The Claude cache only helps exact replays.** A live demo chat typed fresh will call Claude for real
  (~5–20s for the first reply; the bot posts "Reading the chat…" instantly to cover it). Seed the demo group
  with the exact transcript you rehearsed if you want cached, instant replies on stage.
- **One Skyvern account, one run at a time** on the free plan. Coordinate with P4 so test runs don't queue.

---

Sources:
[Claude models](https://platform.claude.com/docs/en/about-claude/models/overview) ·
[Tool use / tool_choice](https://platform.claude.com/docs/en/agents-and-tools/tool-use/define-tools) ·
[Structured outputs](https://platform.claude.com/docs/en/build-with-claude/structured-outputs) ·
[Supabase connection strings](https://supabase.com/docs/guides/database/connecting-to-postgres) ·
[Telegram setWebhook](https://core.telegram.org/bots/api#setwebhook) ·
[Telegram privacy mode](https://core.telegram.org/bots/features#privacy-mode) ·
[Railway FastAPI guide](https://docs.railway.com/guides/fastapi) ·
[Skyvern quickstart](https://www.skyvern.com/docs/developers/getting-started/quickstart.md) ·
[Skyvern run_task](https://www.skyvern.com/docs/sdk-reference/tasks/run-task.md) ·
[Skyvern billing](https://www.skyvern.com/docs/cloud/account-settings/billing-usage.md)
