"""LLM access. Two calls cover everything:

  structured(system, messages, schema) -> dict   # structured outputs = guaranteed JSON shape
  agent(system, messages, tools, handlers) -> str  # the tool-use loop for free-form questions

MOCK_LLM=true  -> MockLLM (canned answers from mock_data.py, zero spend)
MOCK_LLM=false -> ClaudeLLM, with an on-disk cache so replaying the demo doesn't re-spend.
"""
from ..config import settings
from .base import LLM
from .claude import ClaudeLLM
from .mock import MockLLM


def make_llm() -> LLM:
    if settings.mock_llm:
        return MockLLM()
    if not settings.anthropic_api_key:
        raise RuntimeError("MOCK_LLM=false but ANTHROPIC_API_KEY is empty")
    return ClaudeLLM(settings.anthropic_api_key, settings.anthropic_model, settings.llm_cache_dir)


__all__ = ["LLM", "ClaudeLLM", "MockLLM", "make_llm"]
