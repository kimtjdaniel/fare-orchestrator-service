"""FastAPI entry point.   Run:  uvicorn app.main:app --reload --port 8000

Endpoints
  POST /webhook             WhatsApp robot contract (see CONTRACTS.md). Replies go out via send().
  POST /telegram/webhook    Raw Telegram updates, converted to the same IncomingMessage.
  GET  /trips/{id}          Full trip JSON for the dashboard.
  GET  /groups/{gid}/trip   Active trip for a group (dashboard / debugging).
  GET  /health
"""
from __future__ import annotations

import logging
from contextlib import asynccontextmanager

import httpx
from fastapi import BackgroundTasks, FastAPI, Header, HTTPException, Request
from fastapi.middleware.cors import CORSMiddleware

from .config import settings
from .llm import make_llm
from .messaging import make_messenger
from .models import IncomingMessage
from .orchestrator import Brain
from .store import make_store

logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(name)s: %(message)s")
log = logging.getLogger("yate")


@asynccontextmanager
async def lifespan(app: FastAPI):
    store = make_store(settings.database_url)
    await store.connect()
    app.state.brain = Brain(store, make_llm(), make_messenger(settings.messaging_backend))
    log.info("brain up | store=%s llm=%s travel=%s browser=%s messaging=%s",
             type(store).__name__, "mock" if settings.mock_llm else settings.anthropic_model,
             "mock" if settings.mock_travel else "real", "mock" if settings.mock_browser else "skyvern",
             settings.messaging_backend)
    yield
    await store.close()


app = FastAPI(title="Yate brain", lifespan=lifespan)
app.add_middleware(CORSMiddleware, allow_origins=["*"], allow_methods=["*"], allow_headers=["*"])


def brain(request: Request) -> Brain:
    return request.app.state.brain


@app.get("/health")
async def health():
    return {"ok": True, "mock_llm": settings.mock_llm, "mock_travel": settings.mock_travel,
            "mock_browser": settings.mock_browser, "messaging": settings.messaging_backend,
            "store": "postgres" if settings.database_url else "memory"}


@app.post("/webhook")
async def webhook(m: IncomingMessage, background: BackgroundTasks, request: Request):
    # Respond instantly; the work (Claude, searches, Skyvern) happens after the response,
    # and replies are delivered through send(). Keeps the robot/Telegram from timing out.
    background.add_task(brain(request).handle, m)
    return {"reply": None, "accepted": True}


# ------------------------------------------------------------------ Telegram adapter (P2 to verify)
def telegram_to_incoming(update: dict) -> IncomingMessage | None:
    username = settings.telegram_bot_username.lstrip("@").lower()

    if cq := update.get("callback_query"):            # inline ✅/❌/1/2/3 button press
        chat = cq["message"]["chat"]
        user = cq["from"]
        return IncomingMessage(
            group_id=str(chat["id"]), group_name=chat.get("title", ""),
            sender_id=str(user["id"]), sender_name=user.get("first_name") or user.get("username", "?"),
            text=cq.get("data", ""), tagged=True, message_id=f"cb_{cq['id']}")

    msg = update.get("message")
    if not msg or "text" not in msg or msg["chat"]["type"] not in ("group", "supergroup"):
        return None
    text, user = msg["text"], msg["from"]
    mentions = [text[e["offset"]:e["offset"] + e["length"]].lstrip("@").lower()
                for e in msg.get("entities", []) if e["type"] == "mention"]
    replying_to_bot = (msg.get("reply_to_message", {}).get("from", {}).get("username", "").lower()
                       == username) if username else False
    return IncomingMessage(
        group_id=str(msg["chat"]["id"]), group_name=msg["chat"].get("title", ""),
        sender_id=str(user["id"]), sender_name=user.get("first_name") or user.get("username", "?"),
        text=text, tagged=(username in mentions) or replying_to_bot,
        timestamp=msg.get("date", 0), message_id=str(msg["message_id"]))


async def _ack_callback(callback_id: str) -> None:
    url = f"https://api.telegram.org/bot{settings.telegram_bot_token}/answerCallbackQuery"
    async with httpx.AsyncClient(timeout=10) as client:
        await client.post(url, json={"callback_query_id": callback_id})


@app.post("/telegram/webhook")
async def telegram_webhook(request: Request, background: BackgroundTasks,
                           x_telegram_bot_api_secret_token: str | None = Header(default=None)):
    if settings.telegram_webhook_secret and x_telegram_bot_api_secret_token != settings.telegram_webhook_secret:
        raise HTTPException(status_code=401)
    update = await request.json()
    m = telegram_to_incoming(update)
    if m:
        background.add_task(brain(request).handle, m)
    if "callback_query" in update and settings.telegram_bot_token:
        background.add_task(_ack_callback, update["callback_query"]["id"])
    return {"ok": True}


# ------------------------------------------------------------------ dashboard reads
@app.get("/trips/{trip_id}")
async def get_trip(trip_id: str, request: Request):
    view = await brain(request).store.trip_view(trip_id)
    if not view:
        raise HTTPException(status_code=404)
    return view


@app.get("/groups/{group_id}/trip")
async def get_group_trip(group_id: str, request: Request):
    trip = await brain(request).store.get_active_trip(group_id)
    if not trip:
        raise HTTPException(status_code=404, detail="no active trip")
    return await brain(request).store.trip_view(trip.id)
