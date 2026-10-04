# Fare orchestrator

The Go backend owns group planning and broadcasts progress to the frontend.

## Dashboard contract

- `GET /groups/{groupId}/events`: group-scoped WebSocket, with an immediate `group.snapshot`.
- `GET /groups/{groupId}/sessions`: latest 20 planning session snapshots for the group.
- `GET /groups/{groupId}/sessions/{sessionId}`: one group-scoped session snapshot.

The existing WhatsApp `POST /webhook` and Telegram webhook continue to drive the
workflow. A planning trigger creates a UUID session and emits `session.started`;
the frontend shows a popup linking to `/dashboard/{groupId}/{sessionId}`. The
backend still waits for the group's destination choice before starting searches.
Frontend connections and reconnects do not start searches.

Every WebSocket event has `version: 1`, `type`, `groupId`, and `revision`.
Session events also have `sessionId`, `session`, and `timestamp`. Normal progress
events include the authoritative `snapshot`; browser events carry `agentType`
and the travel service's original `event` payload.

Events:

```text
group.snapshot
session.started
session.updated
flight_search.started / flight_search.progress / flight_search.completed
hotel_search.started / hotel_search.progress / hotel_search.completed
agent.browser.live_view / agent.browser.stream / agent.browser.frame
planning.started
planning.task.updated
planning.completed
session.completed
session.failed
```

`planning.task.updated` uses `taskId` (`flight-prices`, `hotel-location`,
`group-budget`, `daily-schedule`) and `status` (`running`, `completed`). Progress
comes from actual backend work. Flight and hotel searches run concurrently.
A search completes after its saved result arrives, rather than an earlier service
status message. The backend then generates and saves Gemini's timed itinerary.
Session completion means the plan is ready for approval, not that a booking was made.

Dashboard snapshots/history are stored through the existing store abstraction
under `dashboard:{groupId}` in `whatsapp_sessions`. With Mongo configured they
survive reconnects and restarts; with the memory store they survive only for the
process lifetime. Browser frames remain ephemeral. The existing singleton trip
schema and booking state machine are retained.

## Configuration

Copy `.env.example` to `.env` and configure the existing Gemini, database, and
messaging settings. Set `DASHBOARD_URL=http://localhost:3000` for bot-shared links.

For real searches and live browser frames:

```dotenv
MOCK_LLM=false
MOCK_TRAVEL=false
FLIGHT_SERVICE_WS_URL=ws://127.0.0.1:8765
HOTEL_SERVICE_WS_URL=ws://127.0.0.1:8766
SEARCH_FRONTEND_ORIGIN=http://localhost:3000
```

Run the travel-search services' flight bridge on 8765 and hotel bridge with
`WS_PORT=8766`. Their Skyvern and Mongo credentials remain in those services.
Leaving the WebSocket URLs empty uses `FLIGHT_SERVICE_URL` / `HOTEL_SERVICE_URL`
for final HTTP results; those Lambda calls do not provide live browser frames.
`MOCK_TRAVEL=true` keeps mock travel offers while still streaming real backend
workflow events. Gemini planning requires its configured API key.

The frontend uses `NEXT_PUBLIC_ORCHESTRATOR_URL=http://localhost:8000` and an
optional `NEXT_PUBLIC_ORCHESTRATOR_WS_URL=ws://localhost:8000`. For HTTPS hosting,
use an HTTPS backend and public WSS URL. No authentication is added for this hackathon.
