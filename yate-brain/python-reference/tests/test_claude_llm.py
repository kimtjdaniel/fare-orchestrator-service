"""Checks ClaudeLLM's request shape, JSON parsing, tool loop and cache — offline, with a fake client.
Validates our code against the real SDK types without spending a cent."""
import asyncio
import json

from anthropic.types import Message

from app import prompts
from app.llm.claude import ClaudeLLM


def _msg(content, stop_reason="end_turn"):
    return Message.model_validate({
        "id": "msg_test", "type": "message", "role": "assistant", "model": "test",
        "content": content, "stop_reason": stop_reason, "stop_sequence": None,
        "usage": {"input_tokens": 10, "output_tokens": 5}})


class FakeMessages:
    def __init__(self, responses):
        self.responses, self.requests = list(responses), []

    async def create(self, **kwargs):
        self.requests.append(kwargs)
        return self.responses.pop(0)


def make(responses, cache_dir=""):
    llm = ClaudeLLM(api_key="test", model="claude-sonnet-5-5", cache_dir=cache_dir)
    llm.client.messages = FakeMessages(responses)
    return llm


def test_structured_uses_output_config_and_parses_json():
    llm = make([_msg([{"type": "text", "text": json.dumps({"intent": "approve"})}])])
    out = asyncio.run(llm.structured(system="s", messages=[{"role": "user", "content": "yes"}],
                                     schema=prompts.INTERPRET_REPLY))
    assert out == {"intent": "approve"}
    req = llm.client.messages.requests[0]
    assert req["output_config"]["format"]["type"] == "json_schema"
    assert "tool_choice" not in req, "forced tool_choice isn't supported on Sonnet/Opus 5.5"


def test_agent_loop_runs_tools_then_answers():
    llm = make([
        _msg([{"type": "tool_use", "id": "toolu_1", "name": "search_flights",
               "input": {"origin": "YVR", "destination": "SAN",
                         "depart_date": "2026-11-20", "return_date": "2026-11-23"}}],
             stop_reason="tool_use"),
        _msg([{"type": "text", "text": "Cheapest is about $280."}]),
    ])
    seen = {}

    async def search_flights(**kw):
        seen.update(kw)
        return [{"price": 280}]

    answer = asyncio.run(llm.agent(system="s", messages=[{"role": "user", "content": "cheapest?"}],
                                   tools=[prompts.SEARCH_FLIGHTS_TOOL],
                                   handlers={"search_flights": search_flights}))
    assert answer == "Cheapest is about $280." and seen["origin"] == "YVR"
    second = llm.client.messages.requests[1]["messages"]
    assert second[-2]["role"] == "assistant" and second[-2]["content"][0]["type"] == "tool_use"
    assert second[-1]["content"][0] == {"type": "tool_result", "tool_use_id": "toolu_1",
                                        "content": json.dumps([{"price": 280}])}


def test_cache_prevents_second_api_call(tmp_path):
    resp = _msg([{"type": "text", "text": "{\"intent\": \"other\"}"}])
    llm = make([resp], cache_dir=str(tmp_path))
    kw = dict(system="s", messages=[{"role": "user", "content": "lol"}], schema=prompts.INTERPRET_REPLY)
    asyncio.run(llm.structured(**kw))
    asyncio.run(llm.structured(**kw))   # FakeMessages would raise IndexError if called again
    assert len(llm.client.messages.requests) == 1
