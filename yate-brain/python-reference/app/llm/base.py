from typing import Any, Awaitable, Callable

Handler = Callable[..., Awaitable[Any]]


class LLM:
    async def structured(self, *, system: str, messages: list[dict], schema: dict) -> dict:
        """Ask for JSON matching schema["schema"] (structured outputs); return it as a dict.
        `schema` is one of the dicts in prompts.py: {"name": ..., "schema": {...}}."""
        raise NotImplementedError

    async def agent(self, *, system: str, messages: list[dict], tools: list[dict],
                    handlers: dict[str, Handler], max_turns: int = 6) -> str:
        """Tool-use loop: Claude asks for tools, we run them, repeat until it answers in text."""
        raise NotImplementedError
