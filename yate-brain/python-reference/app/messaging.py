"""Outbound messages: send(group_id, text, buttons=None).

The brain never talks to WhatsApp/Telegram directly — only through send(). P2 owns the channels;
switching channels is one env var (MESSAGING_BACKEND), no agent code changes.

  console  — prints and keeps an outbox list (dev + tests)
  robot    — POSTs to the whatsapp-web.js robot's /send mailbox
  telegram — calls the Bot API directly; buttons become inline ✅/❌ keyboard
"""
from __future__ import annotations

import logging
from typing import Optional

import httpx

from .config import settings

log = logging.getLogger("yate.messaging")

# Button = (label, payload). Payload comes back to /telegram/webhook as if the user typed it.
Buttons = Optional[list[tuple[str, str]]]


class Messenger:
    async def send(self, group_id: str, text: str, buttons: Buttons = None) -> None:
        raise NotImplementedError


class ConsoleMessenger(Messenger):
    def __init__(self) -> None:
        self.outbox: list[dict] = []

    async def send(self, group_id, text, buttons=None):
        self.outbox.append({"group_id": group_id, "text": text, "buttons": buttons})
        suffix = f"  [buttons: {' | '.join(b[0] for b in buttons)}]" if buttons else ""
        print(f"\n🤖 → {group_id}:\n{text}{suffix}\n", flush=True)


class RobotMessenger(Messenger):
    """whatsapp-web.js robot. WhatsApp has no inline buttons, so they're rendered as a text hint."""

    async def send(self, group_id, text, buttons=None):
        if buttons:
            text += "\n\n" + "  ".join(f"{label} reply {payload}" for label, payload in buttons)
        async with httpx.AsyncClient(timeout=15) as client:
            r = await client.post(f"{settings.robot_url}/send", json={"group_id": group_id, "text": text})
            r.raise_for_status()


class TelegramMessenger(Messenger):
    async def send(self, group_id, text, buttons=None):
        body: dict = {"chat_id": group_id, "text": text}
        if buttons:
            body["reply_markup"] = {
                "inline_keyboard": [[{"text": label, "callback_data": payload} for label, payload in buttons]]
            }
        url = f"https://api.telegram.org/bot{settings.telegram_bot_token}/sendMessage"
        async with httpx.AsyncClient(timeout=15) as client:
            r = await client.post(url, json=body)
            if r.status_code != 200:
                log.error("telegram sendMessage failed: %s %s", r.status_code, r.text)
            r.raise_for_status()


def make_messenger(backend: str) -> Messenger:
    return {"console": ConsoleMessenger, "robot": RobotMessenger,
            "telegram": TelegramMessenger}[backend]()
