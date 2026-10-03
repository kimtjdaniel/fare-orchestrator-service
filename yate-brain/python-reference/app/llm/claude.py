import hashlib
import json
import logging
import time
from pathlib import Path
from typing import Any

from anthropic import AsyncAnthropic
from anthropic.types import Message

from .base import LLM, Handler

log = logging.getLogger("yate.llm")


class ClaudeLLM(LLM):
    def __init__(self, api_key: str, model: str, cache_dir: str = "") -> None:
        self.client = AsyncAnthropic(api_key=api_key)
        self.model = model
        self.cache = Path(cache_dir) if cache_dir else None
        if self.cache:
            self.cache.mkdir(parents=True, exist_ok=True)

    async def _create(self, **kwargs: Any) -> Message:
        """messages.create with a disk cache keyed on the exact request.
        Same transcript in -> same response out, no API call. Delete .llm_cache/ to force fresh calls."""
        kwargs = {"model": self.model, "max_tokens": 2048, **kwargs}
        key = hashlib.sha256(json.dumps(kwargs, sort_keys=True, default=str).encode()).hexdigest()[:32]
        path = self.cache / f"{key}.json" if self.cache else None
        if path and path.exists():
            log.info("llm cache hit %s", key)
            return Message.model_validate_json(path.read_text())

        t0 = time.monotonic()
        resp = await self.client.messages.create(**kwargs)
        log.info("llm %s: %.1fs, in=%s out=%s tokens", self.model, time.monotonic() - t0,
                 resp.usage.input_tokens, resp.usage.output_tokens)
        if path:
            path.write_text(resp.model_dump_json())
        return resp

    async def structured(self, *, system, messages, schema):
        resp = await self._create(
            system=system, messages=messages,
            output_config={"format": {"type": "json_schema", "schema": schema["schema"]}})
        text = "".join(b.text for b in resp.content if b.type == "text")
        try:
            return json.loads(text)
        except json.JSONDecodeError as e:
            raise RuntimeError(f"{schema['name']}: Claude returned non-JSON "
                               f"(stop_reason={resp.stop_reason}): {text[:200]}") from e

    async def agent(self, *, system, messages, tools, handlers: dict[str, Handler], max_turns=6):
        messages = list(messages)
        for _ in range(max_turns):
            resp = await self._create(system=system, messages=messages, tools=tools)
            if resp.stop_reason != "tool_use":
                return "".join(b.text for b in resp.content if b.type == "text").strip()

            messages.append({"role": "assistant",
                             "content": [b.model_dump(exclude_none=True) for b in resp.content]})
            results = []
            for block in resp.content:
                if block.type != "tool_use":
                    continue
                try:
                    output = await handlers[block.name](**block.input)
                    results.append({"type": "tool_result", "tool_use_id": block.id,
                                    "content": json.dumps(output, default=str)})
                except Exception as e:  # let Claude see the error and recover
                    log.exception("tool %s failed", block.name)
                    results.append({"type": "tool_result", "tool_use_id": block.id,
                                    "content": f"Error: {e}", "is_error": True})
            messages.append({"role": "user", "content": results})
        return "Sorry, I got stuck working that out. Can you rephrase?"
