# Team contracts

These are the seams between the four of us. Agree on them in hour 0 and nobody blocks anybody:
each side can build against a mock until the real thing lands. Change a contract only by telling
the person on the other side (and updating this file).

---

## 1. Messaging → Brain (P2 → P1)

`POST /webhook` with JSON:

```json
{
  "group_id": "120363025@g.us",
  "group_name": "Fall trip",
  "sender_id": "14165551234@c.us",
  "sender_name": "Jordan",
  "text": "@Fare figure this out",
  "tagged": true,
  "timestamp": 1790000000,
  "message_id": "3EB0C767D26A1D8A5B21"
}
```

- Send **every** group message, not just mentions. The brain stores them all; that's how it "reads the chat".
- `tagged` = the bot was @mentioned (or the message replies to the bot).
- `message_id` is optional but recommended: duplicates with the same id are ignored, so retries are safe.
- The response is always `{"reply": null, "accepted": true}` and comes back instantly.
  **Replies are delivered asynchronously via `send()`**, never in the HTTP response.

Telegram is already handled: point the bot's webhook at `POST /telegram/webhook` and the brain
converts updates itself (`app/main.py → telegram_to_incoming`). Button presses arrive as text
(`"1"`, `"2"`, `"3"`, `"✅"`, `"❌"`) with `tagged: true`.

## 2. Brain → Messaging (P1 → P2)

`send(group_id, text, buttons=None)` in `app/messaging.py`, chosen by `MESSAGING_BACKEND`:

| Backend | What the brain does | What P2 must provide |
|---|---|---|
| `robot` | `POST {ROBOT_URL}/send` `{"group_id", "text"}` | the whatsapp-web.js mailbox from the plan, §7 |
| `telegram` | calls Bot API `sendMessage` with an inline keyboard | bot token + username in `.env` |
| `console` | prints to terminal | nothing (dev/tests) |

`buttons` is a list of `(label, payload)`, e.g. `[("✅ Book it", "✅"), ("❌ Back", "❌")]`.
WhatsApp gets them as a text hint ("✅ Book it reply ✅").

## 3. Travel tools (P3 ↔ P1)

Signatures are fixed; P3 replaces the bodies (search for `TODO(P3)`). Return types are Pydantic
models in `app/models.py`.

```python
# app/tools/flights.py
async def search_flights(origin: str, destination: str, depart_date: date, return_date: date,
                         adults: int = 1) -> list[FlightOffer]      # cheapest first, per-person price
async def book_flight(offer: FlightOffer, passenger_name: str) -> FlightBooking   # .pnr

# app/tools/hotels.py
async def search_hotels(city: str, check_in: date, check_out: date, guests: int,
                        max_price_per_night: float | None = None) -> list[HotelOffer]  # cheapest first
```

- `origin`/`destination` are IATA codes (`YVR`, `SAN`). Prices are CAD numbers.
- `HotelOffer.checkout_url` must point at **P4's fake checkout page** for that hotel: that's where Skyvern books.
- Raise an exception on failure; the brain catches it and tells the group to pick another option.
- `split.py` (`compute_split`) is already done and tested.

## 4. Hotel booking via Skyvern (P4 ↔ P1)

```python
# app/tools/browser.py
async def start_hotel_booking(hotel: HotelOffer, guests: int, lead_name: str,
                              lead_email: str = ..., title: str = ...) -> tuple[str, str | None]
    # -> (run_id, live_url). Returns immediately; the run continues in Skyvern's cloud.
async def wait_for_booking(run_id: str) -> HotelBookingResult
    # .status == "completed" and .confirmation_number set  -> success
```

What P4's fake checkout page must have (so Skyvern succeeds reliably):
- Deployed at a **public URL** (Skyvern Cloud can't reach `localhost`), set as `HOTEL_CHECKOUT_URL`.
- Reads `?hotel=&check_in=&check_out=&guests=` from the query string and shows them.
- Plain HTML inputs: guest name, email, card number, expiry, CVC, postal code. Avoid Stripe's embedded
  card iframe (hard for any browser agent); process Stripe test mode on the backend if you want it.
- One obvious **"Confirm booking"** button.
- A confirmation page that shows the total and a **confirmation number** in plain text.

The prompt Skyvern receives is `build_prompt()` in `browser.py`. P4 owns tuning it.

## 5. Brain → Dashboard (P1 → P4)

- `GET /trips/{trip_id}` → `{trip, participants, options, bookings}` (one JSON blob).
- `GET /groups/{group_id}/trip` → same, for the group's active trip.
- Chat messages link to `{DASHBOARD_URL}/trip/{trip_id}`.
- For live updates, the dashboard can subscribe to Supabase realtime on `trips`, `options`,
  `bookings` (see SETUP.md), or simply poll `GET /trips/{id}` every 2 seconds.

Trip states, in order: `COLLECTING → AWAITING_CHOICE → SEARCHING → AWAITING_APPROVAL → BOOKING → CONFIRMED`
(plus `CANCELLED`). A hotel booking row moves `pending → running → confirmed | failed`; its
`live_url`, `recording_url`, and `screenshot_urls` fill in as Skyvern reports them.
