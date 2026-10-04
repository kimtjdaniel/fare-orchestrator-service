# Fare

## Inspiration

Planning a group trip sounds fun until everyone has different dates, budgets, and preferences. The planning usually starts in a group chat, and then one person quietly becomes the travel agent. They search flights, compare hotels, keep track of who can travel when, and paste links back into the chat.

We wanted that person to get their weekend back.

There are trip planners and booking sites, but they all ask the group to leave the conversation and start over in another app. We asked a simpler question: what if the travel planner was already in the group chat?

Fare sits in WhatsApp with your friends. People keep talking the way they already do. When someone needs help, they mention @Fare. The rest of the chat stays context, not a command line.

## Links

- Frontend: https://github.com/Mark-Vu/fare-frontend
- Backend: https://github.com/kimtjdaniel/fare-orchestrator-service
- Flight and hotel search: https://github.com/bnquon/travel-search-services
- WhatsApp client: https://github.com/PaulP1406/travel-agent-whatsapp-service

## What it does

Friends talk normally in the group:

"I'm free December 17 to 24."
"Keep it under $1,500."
"Somewhere with good nightlife."
"I'd rather stay somewhere walkable."

Then someone asks for help in everyday language:

"@Fare can you plan a trip based on what we've discussed?"

There is no command list to memorize. A mention, or a reply to one of Fare's own messages, is enough. Messages that do not mention Fare are saved as context. Fare does not answer every line in the chat.

From that conversation, Fare collects destinations, dates, departure cities, budgets, and who wants what. If something important is missing, it asks. If the group has not picked a place, it can suggest a few.

Once a destination is chosen, Fare searches for real options:

- Google Flights for fares
- Booking.com and Airbnb for places to stay

Flight and stay searches run at the same time. Fare sends the group a live dashboard link. Everyone can follow the progress, watch the browser previews, and compare the options that come back. When a recording is captured, it can be played back later.

Gemini then picks a flight and a stay from those actual results, using the group's preferences, and explains each choice in plain language.

The conversation keeps going:

"@Fare can we find a cheaper flight?"
"@Fare send us the hotel link."
"@Fare replace that museum with something outdoors."
"@Fare remove the activity on Tuesday afternoon."

Fare can also build a day-by-day itinerary, suggest restaurants, and track shared expenses.

"@Fare I paid $84.50 for dinner, split with everyone."
"@Fare show our expenses."

In WhatsApp, Fare repeats the payer, the amount, and the equal split, then waits for a yes before saving. The dashboard can add or remove expenses and show who owes what. Each trip has its own ledger. Tracking is CAD and equal splits. Flight quotes and hotel prices do not automatically become expenses, and repayments are not tracked.

Selected flights and stays keep their source links, so Fare can send those links back to the group and the dashboard can show them.

Fare plans and researches the trip. It does not buy anything. Hotel links open the listing. A captured Google Flights link selects an outbound flight. The group still checks availability, picks the return flight, and books it themselves.

## How we built it

**Core stack**

- Next.js, React, and TypeScript for the dashboard
- Go for the brain: conversation, trip state, search coordination, itinerary edits, and expenses
- Python on AWS Lambda for the flight and hotel workers
- Skyvern cloud browsers to open Google Flights, Booking.com, and Airbnb and pull offers into one format
- Google Gemini to read the chat, extract preferences, choose among real offers, and write the itinerary
- MongoDB for trips, messages, sessions, itineraries, and expense ledgers
- Amazon S3 for browser recordings
- A Node WhatsApp client that only relays messages. It logs in as a normal WhatsApp user, forwards the group chat to the brain, and sends replies back. It does not plan trips.

The split that mattered: Gemini decides what the group is asking for. Go checks that decision against the current trip and then does the work. Prices and links come from saved search results. The model does not invent them.

```text
WhatsApp group
"@Fare help us plan"
        |
        v
WhatsApp client
        |
        v
Go brain  ---- Gemini
   |  |
   |  +---- MongoDB
   |
   +---- Next.js dashboard
        |
        +---- Flight Lambda ---- Skyvern ---- Google Flights
        |
        +---- Hotel Lambda ---- Skyvern ---- Booking.com
                                    |
                                    +------- Airbnb

Results, progress, and browser frames come back to Go.
Go updates WhatsApp and the dashboard.
Recordings go to S3, then show up on the dashboard.
```

## Challenges we ran into

Flight search and hotel search finish at different times, and each one can fail on its own. We had to keep every browser session, progress update, result, and recording attached to the right trip, and still show a useful plan when only one side came back.

Travel sites do not sit still. Layouts change, pages load slowly, and some searches get blocked. We stopped trying to cover every airline site and kept the flight search on Google Flights, with Booking.com and Airbnb for stays.

A chat message that says "I'm searching" does not show the group anything. The dashboard had to stream live browser frames while the search was running, without letting the page itself start another search.

People do not talk like an API. Someone changes their dates in the middle of the chat, mentions a budget once, or replies "yes" to the wrong question. Fare has to tell background chatter apart from a message aimed at it, and figure out what a short reply is actually answering.

A recommendation is useless if it loses the price, the reason, and the link it came from. We had to keep every chosen flight and stay tied to the offer that was actually returned.

Money someone already paid is not the same as a flight quote. Estimated trip costs stay separate from the expense ledger, and WhatsApp will not save an expense until the group confirms it.

## Accomplishments that we're proud of

Getting four separate codebases to behave like one product: a WhatsApp relay, a Go brain, two Python search workers, and a Next.js dashboard.

Watching a real Google Flights or Airbnb session appear in the group dashboard while the search is still running.

Letting people plan in normal sentences, with no command list, while the backend still refuses to book, overwrite the wrong trip, or treat a random "yes" as approval.

Keeping source links on the flight and stay Fare recommends, then sending those same links back into the group chat.

An expense ledger that stores amounts in cents, splits them evenly, and asks before it writes anything from WhatsApp.

## What we learned

An AI agent is mostly orchestration. The model is one step. The rest is state, concurrent searches, failed browsers, saved results, and two clients that have to show the same trip.

Gemini is good at reading a messy group chat and explaining a choice. It should not be the thing that moves the trip forward, calculates who owes what, or invents a URL. That belongs in Go, checked against data we actually stored.

Browser automation is how the conversation reaches live travel sites. The dashboard is how the group can see that work instead of waiting on a single reply.

## What's next for Fare

A real booking step, after the group approves a plan. Right now Fare stops at research and links.

Authentication, so a dashboard is tied to the group that owns the trip.

More than CAD and equal splits: shared repayments, uneven shares, and other currencies.

A sturdier WhatsApp connection than a logged-in browser session, and search progress that survives a backend restart.
