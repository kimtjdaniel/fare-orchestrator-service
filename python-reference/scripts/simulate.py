"""Play the group chat yourself — no WhatsApp, Telegram, or server needed.

  python scripts/simulate.py              # replay the demo transcript, then chat interactively
  python scripts/simulate.py --no-replay  # start from an empty chat
  python scripts/simulate.py --url http://localhost:8000   # send to a running server instead

Interactive input format:   Name: message      (mention the bot with @Yate)
Uses your .env, so with MOCK_LLM=false it calls real Claude (cached in .llm_cache/).
"""
import argparse
import asyncio
import json
import sys
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ROOT))

from app.config import settings  # noqa: E402

FIXTURE = ROOT / "tests" / "fixtures" / "demo_transcript.json"


def parse(line: str, group_id: str) -> dict | None:
    if ":" not in line:
        print("  format is  Name: message")
        return None
    name, text = (s.strip() for s in line.split(":", 1))
    return {"group_id": group_id, "group_name": "Simulator", "sender_id": f"u_{name.lower()}",
            "sender_name": name, "text": text,
            "tagged": f"@{settings.bot_name.lower()}" in text.lower(), "timestamp": int(time.time())}


async def main(args):
    data = json.loads(FIXTURE.read_text())
    group_id = data["group_id"] + f"-{int(time.time())}"
    script = [] if args.no_replay else data["messages"]

    if args.url:
        import httpx
        async def deliver(body):
            async with httpx.AsyncClient(timeout=10) as c:
                (await c.post(f"{args.url}/webhook", json=body)).raise_for_status()
    else:
        settings.messaging_backend = "console"
        from app.llm import make_llm
        from app.messaging import make_messenger
        from app.models import IncomingMessage
        from app.orchestrator import Brain
        from app.store import make_store
        store = make_store(settings.database_url)
        await store.connect()
        brain = Brain(store, make_llm(), make_messenger("console"))
        async def deliver(body):
            await brain.handle(IncomingMessage(**body))

    print(f"group: {group_id} | llm={'mock' if settings.mock_llm else settings.anthropic_model}")
    for t in script:
        print(f"{t['sender_name']}: {t['text']}")
        await deliver({"group_id": group_id, "group_name": data["group_name"], "tagged": False,
                       "timestamp": int(time.time()), **t})

    print("\nYour turn (Ctrl+C to quit). Try:  Sam: 1   then   Jordan: ✅")
    loop = asyncio.get_running_loop()
    while True:
        line = (await loop.run_in_executor(None, input, "> ")).strip()
        if line and (body := parse(line, group_id)):
            await deliver(body)


if __name__ == "__main__":
    p = argparse.ArgumentParser()
    p.add_argument("--no-replay", action="store_true")
    p.add_argument("--url", help="POST to a running server's /webhook instead of in-process")
    try:
        asyncio.run(main(p.parse_args()))
    except (KeyboardInterrupt, EOFError):
        print()
