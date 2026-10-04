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

## Local curl walkthrough: Tokyo trip

This walkthrough sends chat messages directly to the webhook and follows the
result in the frontend. Use `MESSAGING_BACKEND=console` so bot replies and
destination options appear in the Go terminal without needing WhatsApp.

Assume these repositories are siblings inside your `hackathons/` directory:

```text
hackathons/
├── fare-orchestrator-service/
├── fare-frontend/
└── travel-search-services/
```

Each terminal's startup commands below begin from that parent directory.

### 1. Configure the backend and frontend

If the orchestrator has no `.env` yet, copy `fare-orchestrator-service/.env.example`
to `fare-orchestrator-service/.env`. Keep existing credentials and set these values
in that file for real Gemini planning and local travel searches:

```dotenv
PORT=8000
MESSAGING_BACKEND=console
MOCK_LLM=false
GEMINI_API_KEY=your-gemini-key
MOCK_TRAVEL=false
DASHBOARD_URL=http://localhost:3000
FLIGHT_SERVICE_WS_URL=ws://127.0.0.1:8765
HOTEL_SERVICE_WS_URL=ws://127.0.0.1:8766
SEARCH_FRONTEND_ORIGIN=http://localhost:3000
```

For a local run without Mongo, set `MONGODB_URI=` in the orchestrator's `.env`;
trip/session state then lasts only until the Go process stops. Otherwise, keep a
working Mongo connection. `MOCK_LLM=true` uses canned demo answers rather than
interpreting this Tokyo request. `MOCK_TRAVEL=true` uses sample travel offers and
does not run the real search browsers.

In `fare-frontend/web/.env.local`, set:

```dotenv
NEXT_PUBLIC_ORCHESTRATOR_URL=http://localhost:8000
NEXT_PUBLIC_ORCHESTRATOR_WS_URL=ws://localhost:8000
```

Set up the travel services' shared Python environment and credential files using
their [local startup instructions](../travel-search-services/README.md#run-both-services-locally).
Flight credentials belong in `travel-search-services/.env`; hotel credentials
belong in `travel-search-services/hotel-service/.env`.

### 2. Start both travel search bridges

In one terminal, start flights:

```bash
cd travel-search-services/flight-service
set -a
source ../.env
set +a
../.venv/bin/python websocket_test_server.py
```

In a second terminal, start hotels:

```bash
cd travel-search-services/hotel-service
set -a
source .env
set +a
WS_PORT=8766 ../.venv/bin/python websocket_test_server.py
```

Leave both running. Flights listen on `ws://127.0.0.1:8765`; hotels listen on
`ws://127.0.0.1:8766`.

### 3. Start the Go orchestrator from its repository

In a third terminal:

```bash
cd fare-orchestrator-service
go run .
```

Run this inside `fare-orchestrator-service/`, where `go.mod` lives. Running
`go run .` from `hackathons/` produces `cannot find main module`; change directory
instead of creating a new module. Leave this terminal open to read bot replies
and backend errors. Restart the process after editing its `.env`.

### 4. Start the frontend and open the test group's page

In a fourth terminal:

```bash
cd fare-frontend/web
npm install
npm run dev
```

You can skip `npm install` if dependencies are already installed. Open:

**http://localhost:3000/dashboard/test-group**

Use this exact group URL. `/dashboard` redirects to `group123`, which will not
show messages sent with `group_id: "test-group"`. Before sending the first request,
the page should connect to the group and have no active session for a fresh group.

### 5. Send the planning request

From another terminal, send:

```bash
curl -X POST http://localhost:8000/webhook \
  -H 'Content-Type: application/json' \
  -d '{
    "group_id": "test-group",
    "group_name": "Tokyo crew",
    "sender_id": "mark",
    "sender_name": "Mark",
    "tagged": true,
    "text": "@Fare plan a 7-night Tokyo trip from Vancouver YVR, December 17–24, 2026. Budget CAD 1800 per person. We like food and walkable neighborhoods."
  }'
```

Expected immediate response:

```json
{"accepted":true,"reply":null}
```

This means the webhook accepted the message for background processing. It does
not mean planning has finished or succeeded. Bot replies appear in the Go terminal
because this walkthrough uses console messaging; they are not returned by curl.

The frontend should show a new planning session and a popup with **View live plan**.
Open it to reach `http://localhost:3000/dashboard/test-group/<sessionId>`.
Initially, Fare gathers preferences and then waits for a destination choice.
The Go terminal should print the proposed options. Gemini may suggest a Tokyo
neighborhood such as Shimokitazawa, so the displayed name can vary.

### 6. Choose a destination option

Wait until options have been generated before sending the choice. You can read
them in the Go terminal or inspect the trip:

```bash
curl http://localhost:8000/groups/test-group/trip
```

Look for `state: "AWAITING_CHOICE"` and a nonempty `options` list. Then send
`"1"` to select the first proposed option; use `"2"` or `"3"` if you prefer another
available option:

```bash
curl -X POST http://localhost:8000/webhook \
  -H 'Content-Type: application/json' \
  -d '{"group_id":"test-group","sender_id":"mark","sender_name":"Mark","text":"1","tagged":true}'
```

Expect the same `{"accepted":true,"reply":null}` acknowledgement. The Go terminal
should then say that Fare is looking up flights and a place to stay.

### 7. Follow the search and finished plan

Keep the session page open. Flight and hotel searches should start concurrently,
with progress and browser previews as frames arrive. Once both searches return
usable offers, Fare selects flights and a stay and builds the daily itinerary.
On success, the frontend shows **Your trip is ready** with the saved plan;
the trip state becomes `AWAITING_APPROVAL`. This completes the planning walkthrough.

To inspect the dashboard's saved progress or error details from the terminal:

```bash
curl http://localhost:8000/groups/test-group/sessions
```

If the page says **Waiting for your group to choose a destination**, check that
options exist and send the choice using the same `group_id`. If it says
**Planning interrupted**, inspect the Go terminal's error and the session response;
the generic error text does not identify the underlying cause. Check that both
search bridges are running and that the configured Gemini, Skyvern, and Mongo
credentials work. Browser previews require the WebSocket search configuration;
the Lambda HTTP path returns final results without live frames.

For a fresh rerun, use a new group ID such as `test-group-2` in both POST bodies,
both inspection URLs, and the frontend URL. Reusing `test-group` continues its
existing trip state, especially when Mongo persists it across restarts.

## Lambda recording callbacks and plan selection

Real search mode POSTs flight and hotel requests concurrently to the configured
Lambda Function URLs. Set `MOCK_TRAVEL=false`, `MOCK_LLM=false`, and leave
`FLIGHT_SERVICE_WS_URL` / `HOTEL_SERVICE_WS_URL` empty to use this path.
Gemini requires `GEMINI_API_KEY` (or `GOOGLE_API_KEY`).

Set `ORCHESTRATOR_PUBLIC_URL` to the externally reachable Go backend origin,
for example `https://your-orchestrator.example.com`. The orchestrator includes
an individual `callback_url` in each Lambda request pointing to:

```text
POST /travel-search/results/{requestID}
```

Each Lambda saves its result after browser cleanup and recording upload, then
POSTs that full record to the supplied URL. The receiver validates the request
correlation, session, service-specific results list, and terminal status. It
acknowledges duplicate deliveries for 15 minutes. No authentication is added.
Callback correlation is in memory: searches and their callbacks must reach the
same Go process; restart recovery and routing across replicas are not supported.

The first final result from either the callback or the synchronous Lambda
response resumes that search exactly once. A broken HTTP connection can still
wait for a callback within the existing 12-minute search deadline. A recording
failure does not discard valid travel results. If no public URL is configured,
the direct Lambda response still works; per-request callbacks are disabled.

`flight_search.recording.completed` and `hotel_search.recording.completed` events
carry `searchId`, `recordingUrl`, `recordingError`, and `deliveryError`. They mean
recording processing has finished; check `recordingUrl` / `recordingError` for
upload success. The dashboard persists this metadata under `recordings.flight`
and `recordings.hotel` for reconnects, including failed searches.

Once both searches provide offers, Gemini compares all returned flights and
hotels against group preferences, timing, ratings, and per-person costs. Its
selected IDs are validated against those actual offers. The saved itinerary
includes `selection_reason`; the dashboard plan includes the chosen IDs and
`selectionReason`. Gemini then generates activities around the selected travel
plan. Selection or activity-generation errors fail the dashboard session and
return the trip to destination choice for a retry. Plans still require group
approval before any booking.

Deploy the updated flight and hotel Lambda code in `travel-search-services` to
accept `callback_url`. This is still a synchronous Lambda invocation with a
completion callback, not an asynchronous job-submission endpoint.
