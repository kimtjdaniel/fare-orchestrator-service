import re

from . import mock_data
from .base import LLM

APPROVE = re.compile(r"✅|👍|\b(yes|yep|approve|book it|do it|go)\b", re.I)
REJECT = re.compile(r"❌|👎|\b(no|nope|reject)\b", re.I)
CANCEL = re.compile(r"\b(cancel|start over|never ?mind)\b", re.I)
REVISE = re.compile(r"\b(cheaper|instead|swap|change|different)\b", re.I)
NUMBER = re.compile(r"\b([1-3])\b")


class MockLLM(LLM):
    """Deterministic stand-in for Claude. Good enough to drive the whole flow offline."""

    def __init__(self) -> None:
        self.calls: list[str] = []

    async def structured(self, *, system, messages, schema):
        name = schema["name"]
        self.calls.append(name)
        if name == "record_preferences":
            return mock_data.PREFERENCES
        if name == "propose_options":
            return mock_data.OPTIONS
        if name == "interpret_reply":
            text = messages[-1]["content"] if messages else ""
            if CANCEL.search(text):
                return {"intent": "cancel"}
            if m := NUMBER.search(text):
                return {"intent": "choose", "option_number": int(m.group(1))}
            if REVISE.search(text):
                return {"intent": "revise", "revision_request": text}
            if APPROVE.search(text):
                return {"intent": "approve"}
            if REJECT.search(text):
                return {"intent": "reject"}
            return {"intent": "question" if "?" in text else "other"}
        raise ValueError(f"MockLLM has no canned answer for {name}")

    async def agent(self, *, system, messages, tools, handlers, max_turns=6):
        self.calls.append("agent")
        return "(mock) Good question! I'll have a real answer once MOCK_LLM=false."
