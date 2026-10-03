"""One-command checks that each real service is wired up. Run after filling in .env.

  python scripts/smoke.py db        # connect, apply schema, list tables          (free)
  python scripts/smoke.py claude    # one tiny structured-output call             (~$0.001)
  python scripts/smoke.py schemas   # send all 3 brain schemas once with the real transcript
                                    #   -> proves the API accepts them             (~$0.02)
  python scripts/smoke.py telegram  # getMe + getWebhookInfo                      (free)
  python scripts/smoke.py skyvern   # tiny 3-step run on example.com             (~a few credits)
"""
import asyncio
import json
import sys
from datetime import datetime
from zoneinfo import ZoneInfo
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ROOT))

from app import prompts  # noqa: E402
from app.config import settings  # noqa: E402


def ok(msg):
    print(f"✅ {msg}")


def fail(msg):
    print(f"❌ {msg}")
    sys.exit(1)


async def db():
    if not settings.database_url:
        fail("DATABASE_URL is empty")
    import asyncpg
    conn = await asyncpg.connect(settings.database_url, statement_cache_size=0)
    await conn.execute((ROOT / "db" / "schema.sql").read_text())
    tables = [r["tablename"] for r in await conn.fetch(
        "select tablename from pg_tables where schemaname = 'public' order by 1")]
    await conn.close()
    missing = {"trips", "messages", "participants", "options", "bookings"} - set(tables)
    if missing:
        fail(f"schema applied but missing tables: {missing}")
    ok(f"connected, schema applied, tables: {', '.join(tables)}")


def _claude():
    if not settings.anthropic_api_key:
        fail("ANTHROPIC_API_KEY is empty")
    from app.llm.claude import ClaudeLLM
    return ClaudeLLM(settings.anthropic_api_key, settings.anthropic_model, cache_dir="")


async def claude():
    out = await _claude().structured(
        system="Classify the message.", messages=[{"role": "user", "content": "✅ book it!"}],
        schema=prompts.INTERPRET_REPLY)
    ok(f"{settings.anthropic_model} answered: {out}")


async def schemas():
    llm = _claude()
    transcript = json.loads((ROOT / "tests" / "fixtures" / "demo_transcript.json").read_text())
    chat = "\n".join(f"{m['sender_name']}: {m['text']}" for m in transcript["messages"])
    today = datetime.now(ZoneInfo(settings.timezone)).date().isoformat()
    prefs = await llm.structured(system=prompts.EXTRACT_SYSTEM.format(today=today),
                                 messages=[{"role": "user", "content": f"Group chat:\n{chat}"}],
                                 schema=prompts.RECORD_PREFERENCES)
    ok(f"record_preferences: {[p['name'] for p in prefs['participants']]}")
    print(json.dumps(prefs, indent=2))
    opts = await llm.structured(
        system=prompts.PROPOSE_SYSTEM.format(bot_name=settings.bot_name, today=today, feedback=""),
        messages=[{"role": "user", "content": "Participants:\n" + json.dumps(prefs["participants"])}],
        schema=prompts.PROPOSE_OPTIONS)
    ok(f"propose_options: {[o['destination'] for o in opts['options']]}")
    print(json.dumps(opts, indent=2))
    out = await llm.structured(
        system=prompts.INTERPRET_SYSTEM.format(state="AWAITING_CHOICE", options="1. A; 2. B"),
        messages=[{"role": "user", "content": "@Fare let's do the second one"}],
        schema=prompts.INTERPRET_REPLY)
    ok(f"interpret_reply: {out}")
    print("\nTip: paste the record_preferences output into app/llm/mock_data.py to make mocks realistic.")


async def telegram():
    if not settings.telegram_bot_token:
        fail("TELEGRAM_BOT_TOKEN is empty")
    import httpx
    base = f"https://api.telegram.org/bot{settings.telegram_bot_token}"
    async with httpx.AsyncClient(timeout=10) as c:
        me = (await c.get(f"{base}/getMe")).json()
        if not me.get("ok"):
            fail(f"getMe failed: {me}")
        info = (await c.get(f"{base}/getWebhookInfo")).json()["result"]
    ok(f"bot @{me['result']['username']} (set TELEGRAM_BOT_USERNAME={me['result']['username']})")
    print(f"   can_read_all_group_messages: {me['result'].get('can_read_all_group_messages')}"
          "  <- must be True (BotFather /setprivacy -> Disable, then re-add bot to the group)")
    print(f"   webhook: {info.get('url') or '(not set)'}  pending: {info.get('pending_update_count')}"
          f"  last_error: {info.get('last_error_message') or 'none'}")


async def skyvern():
    if not settings.skyvern_api_key:
        fail("SKYVERN_API_KEY is empty")
    from skyvern import Skyvern
    s = Skyvern(api_key=settings.skyvern_api_key)
    run = await s.run_task(prompt="What is the main heading on this page?", url="https://example.com",
                           max_steps=3, wait_for_completion=True, timeout=180, title="smoke test")
    if run.status != "completed":
        fail(f"status={run.status} reason={run.failure_reason}")
    ok(f"output={run.output}")
    print(f"   watch/replay: {run.app_url}  <- open in a private window to see if it's shareable")


if __name__ == "__main__":
    cmds = {"db": db, "claude": claude, "schemas": schemas, "telegram": telegram, "skyvern": skyvern}
    if len(sys.argv) != 2 or sys.argv[1] not in cmds:
        print(__doc__)
        sys.exit(1)
    asyncio.run(cmds[sys.argv[1]]())
