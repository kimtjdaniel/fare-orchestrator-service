---
paths:
  - "app/tools/**/*.py"
  - "app/messaging.py"
  - "app/main.py"
---

# Tools, booking and messaging

- Function signatures in `app/tools/` are contracts with teammates (`CONTRACTS.md` §3–4). Keep them
  stable; replace bodies behind the `settings.mock_*` checks.
- Every tool keeps a working mock branch. Real implementations go after the mock check and raise on
  failure (the orchestrator catches it and tells the group).
- `AGENT_TOOLS` / `AGENT_HANDLERS` in `app/tools/__init__.py` are read-only (search). Booking
  functions must never be added there.
- Skyvern (`browser.py`): SDK `skyvern` 1.0.55 (Python 3.11–3.13), async `run_task(prompt, url,
  data_extraction_schema, max_steps, title, ...)` and `get_run(run_id)`. Terminal statuses:
  completed, failed, terminated, timed_out, canceled. Response fields used: `run_id`, `status`,
  `output`, `app_url`, `recording_url`, `screenshot_urls`, `failure_reason`. Skyvern Cloud cannot
  reach localhost; `HOTEL_CHECKOUT_URL` must be a deployed page.
- Duffel (`flights.py`, `hotels.py`): test mode only. Cache the demo route's responses to disk.
- `send()` is the only way to reach the chat. `RobotMessenger` POSTs to `{ROBOT_URL}/send`, which must
  be reachable from wherever the brain runs (same machine or a tunnel; not a laptop's localhost from Railway).
- Telegram: privacy mode must be disabled and the bot re-added to the group; one webhook per bot;
  button presses arrive as `callback_query` and are converted to text with `tagged=True`.
  `X-Telegram-Bot-Api-Secret-Token` is checked against `TELEGRAM_WEBHOOK_SECRET`.
- The webhook must keep returning 200 immediately. Telegram retries non-2xx responses; duplicates are
  dropped by `message_id`.
