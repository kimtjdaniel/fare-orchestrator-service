---
paths:
  - "db/**"
  - "app/store.py"
  - "app/models.py"
---

# Database and store

- Two stores, one interface: `MemoryStore` (no `DATABASE_URL`) and `PostgresStore`. Any new method
  or field goes in **both**, plus `db/schema.sql` and `models.py`. `tests/test_flow.py` runs the flow
  tests against both when `TEST_DATABASE_URL` is set; run them that way after store changes.
- `db/schema.sql` must stay idempotent (`create ... if not exists`); it's re-applied by `smoke.py db`.
  For a column change on an existing table use `alter table ... add column if not exists`.
- asyncpg pool uses `statement_cache_size=0`, required behind Supabase's transaction pooler. Keep it.
- jsonb columns: pass Python dicts/lists that are JSON-safe. Dump Pydantic models with
  `model_dump(mode="json")` first (dates become strings); the codec is plain `json.dumps`.
- UUID columns: convert with `uuid.UUID(...)` going in and `str(...)` coming out; models use `str` ids.
- `PostgresStore.update_trip` / `update_booking` whitelist columns. Add new columns to those lists.
- State changes go through `Store.set_state()` (validates transitions), not `update_trip(state=...)`.
- Supabase: use the **session pooler** connection string (IPv4); direct connections are IPv6-only on
  the free plan. Realtime needs `alter publication supabase_realtime add table ...` (commented at the
  bottom of `schema.sql`); the dashboard is better off polling `GET /trips/{id}`.
