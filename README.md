# Fare orchestrator

The Go backend owns group planning and broadcasts progress to the frontend.

## Dashboard contract

- `GET /dashboard/events`: dashboard-wide WebSocket, with an immediate `dashboard.snapshot` and live events from all groups.
- `GET /dashboard/sessions`: planning session snapshots across all groups, newest first.
- `GET /groups/{groupId}/events`: group-scoped WebSocket, with an immediate `group.snapshot`.
- `GET /groups/{groupId}/sessions`: latest 20 planning session snapshots for the group.
- `GET /groups/{groupId}/sessions/{sessionId}`: one group-scoped session snapshot.

The existing WhatsApp `POST /webhook` and Telegram webhook continue to drive the
workflow. A planning trigger creates a UUID session and emits `session.started`;
the frontend shows a popup linking to `/dashboard/{groupId}/{sessionId}`. The
backend still waits for the group's destination choice before starting searches.
Frontend connections and reconnects do not start searches.

Gemini routes conversational messages using the active trip, recent chat, and any
quoted message. Explicit separate-trip requests such as `@Fare let's also plan a
Paris trip` or `@Fare create a new session` create a fresh UUID session, even while
the current trip awaits choice or approval. Clear standalone commands such as
`@Fare plan a 7-night Tokyo trip ...` start a new session directly, including
repeated requests with identical dates and destination. Commands referring to
the current trip or its itinerary still use conversational routing.
Follow-ups such as `yes`, `1`, searches,
budget changes, and date revisions keep the same session through completion and
retries. Starting searches does not create another session.

For ambiguous requests such as `What about Paris?`, Fare asks whether to change
the current trip or start a separate one. The pending request is stored in the
trip's optional `pending_trip_request` field (`text`, `sender_name`, `sent_at`,
`question`), and survives restarts with Mongo. Reply `continue this trip` or
`new trip` to apply the original request; a bare `yes` repeats the routing question
instead of approving the old trip. If classification fails, Fare asks for
clarification before changing the trip.

Repeating a standalone trip request starts another session. Webhook retries
with the same `message_id` remain deduplicated. The latest request becomes the
group's active trip; earlier dashboard snapshots remain available in its latest
20 sessions. Only the latest trip is active for chat replies. Starting a new trip
does not approve or cancel any previous booking.

Every WebSocket event has `version: 1`, `type`, `groupId`, and `revision`.
The dashboard snapshot uses an empty `groupId`; subsequent events retain their
source group ID. Dashboard event revisions cover all groups, while group event
revisions are scoped to the group.
Session events also have `sessionId`, `session`, and `timestamp`. Normal progress
events include the authoritative `snapshot`; browser events carry `agentType`
and the travel service's original `event` payload.

Browser previews include `website` and `origin`. Flight preview keys use
`{website}:{origin}` (for example, `google_flights:YVR` and `kayak:YVR`), so
simultaneous source browsers never replace each other. Hotel preview keys are
`booking_com` and `airbnb`. Both sources stream independently for each agent.

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
carry `agentType`, `searchId`, `status`, legacy `recordingUrl` / `replayUrl`,
`recordingError`, `deliveryError`, and `sources`. They mean recording processing
has finished; a successful search can still have failed recording uploads.
`sources` contains one entry for each Lambda `origins` record:

```json
{
  "website": "airbnb",
  "origin": null,
  "status": "complete",
  "error": null,
  "recordingUrl": "https://example.com/airbnb.mp4",
  "replayUrl": "https://example.com/airbnb-replay",
  "recordingError": null,
  "browserSessionId": "browser-session-id",
  "recordings": [{"url": "https://example.com/airbnb.mp4", "filename": "airbnb.mp4"}]
}
```

Hotel sources use `website` (`booking_com` or `airbnb`); flight entries may use
`origin` instead. Optional absent metadata is null. `recordings` preserves the
Lambda's full recording list; inspect each source's `status`, `error`, and
`recordingError` independently. Legacy single-source records have an empty
`sources` array and still expose the original top-level links. The hotel Lambda
chooses Booking.com's top-level legacy link when available, falling back to
Airbnb. The dashboard saves the entire event payload under `recordings.flight`
and `recordings.hotel` in every REST / WebSocket session snapshot, including
partial or failed searches. Mongo stores those links for reconnects and process
restarts; in-memory storage loses them on restart.

Once both searches provide offers, Gemini compares all returned flights and
hotels against group preferences, timing, ratings, and per-person costs. The
hotel Lambda combines its cheapest eight Booking.com and cheapest eight Airbnb
stays into `hotels`, with a separately reported status and recording per source.
The orchestrator compares all supplied rows, sorted by total-stay price; a
`partially_complete` result still provides offers from the successful source.
Offers retain `source`, `property_type`, `original_rating`,
`original_rating_scale`, and `price_note`. Real offer ratings are normalized to
10; Airbnb originals are on a 5-point scale. Gemini sees source and property type
along with the rating and price caveats, and receives no recording links. Its
selected IDs are validated against those actual offers. The saved itinerary
includes `selection_reason`; the dashboard plan includes the chosen IDs and
`selectionReason`. The chosen offer and the accommodation stored on the trip
retain source, property type, original rating and scale, and price note. Dashboard
hotel rows expose `source`, `propertyType`, `originalRating`,
`originalRatingScale`, and `priceNote`; final plans expose `hotelSource`,
`hotelPropertyType`, `hotelOriginalRating`, `hotelOriginalRatingScale`, and
`hotelPriceNote`. Gemini then generates activities around the selected travel
plan. Selection or activity-generation errors fail the dashboard session and
return the trip to destination choice for a retry. Plans still require group
approval before any booking.

Deploy the updated flight and hotel Lambda code in `travel-search-services` to
accept `callback_url`. This is still a synchronous Lambda invocation with a
completion callback, not an asynchronous job-submission endpoint.

The default hotel Function URL in `config/config.go` and `.env.example` is
`https://qhz6talpesw4nfnbipkxnxxsq40ivfah.lambda-url.us-west-2.on.aws/`.
A local `.env` with `MOCK_TRAVEL=true` uses sample stays and never invokes that
Lambda. Set `MOCK_TRAVEL=false` and leave `HOTEL_SERVICE_WS_URL` empty to invoke
the deployed hotel service over HTTP. Its deployed code must include the
multi-source hotel search for Airbnb offers and recordings to appear.

## Itinerary activity edits

The active trip's activities can be edited from WhatsApp or the dashboard. For
example, `@Fare move dinner on day 2 to 7:30pm` updates the matching activity;
ambiguous requests ask for a more specific activity. `@Fare undo itinerary edit`
restores the schedule before the last edit. Activities remain suggestions, not
confirmed reservations or verified opening hours.

Dashboard activity actions use the existing `POST /dashboard/trips/{groupId}`:

```json
{
  "action": "update_activity",
  "activity_id": "saved-activity-id",
  "expected_revision": "3",
  "time": "19:30",
  "session_id": "active-session-id"
}
```

`title` and `description` are also editable. `undo_activity_edit` uses the same
revision and session fields. Times use local `HH:MM`. A stale revision or session
returns a conflict rather than overwriting another edit. The saved advisor and
dashboard schedule stay in sync; undo restores only the previous schedule.
Flight and hotel searches are not rerun for activity edits.

## Flight sources

Current flight searches use Google Flights only and return up to 15 cheapest
fares across the requested origins in the existing `flights` list. The
orchestrator accepts the entire returned list; it does not impose an eight-offer
cap. `FlightOffer.source` identifies the provider, and legacy rows without a
source use `google_flights`.

Saved historical `trip_com` and `kayak` offers retain their original source and
optional `booking_url`, `return_duration`, `return_stops`,
`return_departure_time`, and `return_arrival_time` fields. These details are not
guaranteed on new Google Flights offers. The selected itinerary and stored flight
legs retain source/link provenance; the dashboard's chosen plan exposes
`flightSource`. Gemini selects from the supplied unique offer IDs.

Recording events retain all airport/source pairs under `sources`, including
their separate archive/replay links, errors, partial status, `warning`, and
`resultsComplete`. The legacy single recording link remains available.
The Google-only, 15-fare search requires deploying the updated flight Lambda;
existing service URLs and callback endpoints stay the same.

## Deployed live browser previews

Set `ORCHESTRATOR_PUBLIC_URL` to this backend's public HTTPS origin so the flight
and hotel Lambdas can POST live updates back while their normal HTTP search runs.
The orchestrator registers an opaque per-search callback ID and supplies both
`callback_url` for the final result and `progress_callback_url` for live events:

```text
POST /travel-search/events/{requestID}
POST /travel-search/results/{requestID}
```

Live events use the existing version-1 browser/search event envelope. The receiver
checks the registered session and search IDs, limits JSON bodies to 2 MiB and
base64 JPEG frames to 1 MiB, and forwards only recognized event fields. Final
results end live delivery; returning or timing out the search also releases the
listener. Final-result retries remain acknowledged for the existing 15-minute
correlation window. Both callbacks must reach the same backend process; routing
across replicas and callback recovery after a process restart are unsupported.

Browser previews use `website:origin` keys when both fields differ, or the website
(or origin) alone otherwise, and preserve both metadata fields. This keeps
Google Flights previews separate by departure airport and Booking.com and Airbnb
separate for the same stay; saved historical providers keep their own keys. Frontend support for
these source keys and Lambda support for progress callbacks must also be deployed.

A backend Git push alone does not configure its public URL. Without
`ORCHESTRATOR_PUBLIC_URL`, HTTP searches still return final results and recordings
but do not register live callbacks. Local `*_SERVICE_WS_URL` bridges continue to
forward the same browser events directly. Real searches require `MOCK_TRAVEL=false`.

## Travel recommendation reasons

The existing travel-selection Gemini call returns separate `flight_reason` and
`hotel_reason` justifications, bound to the selected offer IDs. They appear below
the respective prices in WhatsApp and as `flightReason` / `hotelReason` on the
final dashboard plan. Selected rows in the live trip view expose `reason`.
Recommendations use supplied prices, preferences and offer details; they do not
verify hotel neighborhoods, amenities, layover durations or return itineraries.
Selecting a different flight or stay clears both reasons and the combined
selection explanation, since either recommendation may depend on the total
budget. Selecting the same offer retains them. Activity edits and undo preserve
only reasons whose offer IDs still match. Existing plans without reasons remain
viewable; no additional model calls or travel-service changes are required.
