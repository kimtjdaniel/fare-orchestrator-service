---
paths:
  - "app/prompts.py"
  - "app/llm/**/*.py"
  - "app/orchestrator.py"
---

# LLM, prompts and schemas

- Each schema in `prompts.py` is `{"name": ..., "schema": {...}}` and is passed as `schema=` to
  `llm.structured()`. `MockLLM` dispatches on `name`, so a new schema needs a canned answer in
  `app/llm/mock.py` / `mock_data.py`, or every offline test that reaches it will fail.
- Structured-output schema limits (Anthropic docs): `"additionalProperties": false` on every object;
  no `format`, `default`, `minItems`, `maxItems`, `minLength`, `maxLength`, `minimum`, `maximum`,
  `pattern`, or nullable type arrays. Keep total optional fields under 24. Put constraints in
  `description` and validate in code.
- Never switch back to `tool_choice={"type": "tool", ...}` for JSON: Sonnet 5.5 and Opus 5.5 reject it.
  `tests/test_claude_llm.py` asserts the request has `output_config` and no `tool_choice`.
- Claude output is untrusted: build Pydantic models from it inside `try/except ValidationError`,
  drop bad items, and tell the group something useful rather than crashing (see `Brain.plan`).
- Before adding an LLM call, check whether a regex or code can decide it (see `CHOICE_ONLY`,
  `APPROVE_ONLY`, `REJECT_ONLY`). Every extra call adds 2–10s of chat latency during the demo.
- Prompts get `{today}` from `orchestrator.today()` (team timezone), never `date.today()`.
- `ClaudeLLM._create` caches on the full request. Changing a prompt or schema invalidates the cache,
  which is expected.
- Smoke-test a schema change against the real API with `python scripts/smoke.py schemas` (~2 cents),
  but only when asked.
