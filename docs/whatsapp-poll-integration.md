# WhatsApp polls — brain-side integration

> For whoever's picking up P1 work on polls. Written by Claude working with Paul (P2) after
> reconciling the robot's contract against this repo's actual `CONTRACTS.md` and code — the
> plain-message contract was already a mismatch (nested vs. flat fields) and has been fixed
> robot-side, no action needed here for that part. Polls are the one genuinely new piece, and
> they don't exist on either side yet.

## Status check — what's already compatible, no action needed

The `whatsapp-service` repo (the "robot") now emits `IncomingMessage`-shaped JSON exactly matching
this repo's `python-reference/CONTRACTS.md` §1: flat `group_id`, `group_name`, `sender_id`,
`sender_name`, `text`, `tagged`, `timestamp`, `message_id`. A few extra fields ride along
(`mentioned_ids`, `quoted`, `media`, `agent_id`, `is_group`, `participant_count`) — safe to ignore,
Go's `json.Decode` drops fields `models.IncomingMessage` doesn't declare.

`RobotMessenger.Send()` in `messaging/messaging.go` already POSTs the right shape
(`{"group_id", "text"}`) to `{ROBOT_URL}/send` for plain text — that part works today against the
robot's `POST /send` endpoint unchanged.

Wiring, for reference:
```bash
MESSAGING_BACKEND=robot
ROBOT_URL=http://localhost:3000        # already the default in config.go
```
On the robot's side, point `BRAIN_WEBHOOK_URL` at `http://localhost:8000/webhook` (**not**
`/webhook/whatsapp` — that path is only `whatsapp-service`'s own local `fake-brain.js` test stub).
Leave `BRAIN_SHARED_SECRET` and `SERVICE_TOKEN` unset on both sides for now — this repo's webhook
doesn't verify a signature and `RobotMessenger` doesn't send a bearer token, and the robot's
defaults already tolerate that (open mode, with a startup warning log). Add both later if you want
real auth between the two processes; neither side requires it to function.

## What's new: native WhatsApp polls

The ask (from Paul): drive the group's trip-planning feedback loop — picking a destination option,
approving the final itinerary — with real WhatsApp polls instead of "reply 1, 2, or 3" /
✅❌-as-text-hint. The robot side is done. This repo needs three things.

### 1. Outbound: let `Messenger.Send` (or a new method) send a poll

`messaging.Messenger` (`messaging/messaging.go`) is currently:
```go
type Messenger interface {
	Send(ctx context.Context, groupID, text string, buttons []Button) error
}
```
There's no poll concept. Recommend adding a second method so console/telegram aren't forced to
fake it:
```go
type Poll struct {
	Name                string
	Options             []string
	AllowMultipleAnswers bool
}

type Messenger interface {
	Send(ctx context.Context, groupID, text string, buttons []Button) error
	SendPoll(ctx context.Context, groupID string, poll Poll) error
}
```
- **`RobotMessenger.SendPoll`**: `POST {ROBOT_URL}/send` with
  `{"group_id": groupID, "poll": {"name": poll.Name, "options": poll.Options, "allow_multiple_answers": poll.AllowMultipleAnswers}}`
  (mirrors `RobotMessenger.Send`'s existing pattern — see `messaging.go:79-105`). Robot's `/send`
  validates exactly one of `text`/`poll` is present; sending both is a 400.
- **`TelegramMessenger.SendPoll`**: Telegram has native polls too (`sendPoll` Bot API method) if you
  want parity — otherwise falling back to the existing inline-keyboard `Send()` rendering is fine,
  this is a bonus not a requirement.
- **`ConsoleMessenger.SendPoll`**: just print it (same spirit as its existing `Send`).

### 2. Inbound: handle the new `poll_vote` webhook event

Today `webhookHandler` (`main.go:137-147`) unconditionally decodes every `POST /webhook` body as
`models.IncomingMessage`. A poll vote is shaped differently — no `text`, no `sender_id` — so it
needs to be discriminated before decoding:

```jsonc
// POST /webhook body when someone votes on a poll
{
  "event": "poll_vote",
  "group_id": "1203…@g.us",
  "group_name": "Fall trip",
  "voter_id": "14165551234@c.us",
  "voter_name": "Jordan",
  "poll_message_id": "true_1203…@g.us_3EB0…",
  "poll_name": "Pick your favourite option",
  "selected_options": ["Lisbon — chill beach vibes"],
  "timestamp": 1790000000
}
```
(A regular chat message also carries `"event": "message"` now, so you can switch on that field
first — decode into a small struct with just `Event string` to peek, then decode fully into either
`models.IncomingMessage` or a new `models.PollVoteEvent`.)

`selected_options` is `[]` if the voter deselected everything (WhatsApp allows un-voting) — treat
that as a no-op, not a choice.

### 3. Wire votes into the state machine

The existing text-reply path for picking a destination (`AwaitingChoice`) and approving the final
plan (`AwaitingApproval`) already lives in `orchestrator/brain.go`:

- `interpret()` (`brain.go:522-527`) regex-matches `"1"`/`"2"`/`"3"` during `AwaitingChoice` into
  `{"intent": "choose", "option_number": N}`.
- `onReply()` (`brain.go:469-519`) routes that intent to `selectOption(ctx, trip, num)`
  (`brain.go:573`), or for `AwaitingApproval`, `"approve"`/`"reject"` intents to `book()`
  (`brain.go:681`) or back to `AwaitingChoice`.
- Options are sent with `optionButtons(options)` (`brain.go:857-864`) — numbered buttons, which
  WhatsApp renders as a text hint today.
- The approval buttons are inlined at the call site, `brain.go:676`:
  `[]messaging.Button{{Label: "✅ Book it", Payload: "✅"}, {Label: "❌ Back", Payload: "❌"}}`.

A poll vote is **unambiguous** — unlike free text, there's no need to run it through `interpret()`'s
regex/Claude guessing. Recommended shape: add a `Brain.HandlePollVote(ctx, event)` entry point
(parallel to `Handle`) that:

1. Loads the trip for `event.GroupID` (same as `handle()` does today via `store`).
2. If `trip.State == models.AwaitingChoice`: match `event.SelectedOptions[0]` against
   `trip.Options[].Destination` (so when you send the poll, make each poll option's label equal to
   the option's `Destination` string — simplest way to round-trip the match without parsing option
   numbers out of poll text) and call `selectOption(ctx, trip, matchedOption.Position)` directly.
3. If `trip.State == models.AwaitingApproval`: match against `"✅ Book it"` / `"❌ Back"` (or
   whatever labels you send the poll with) and call `book(ctx, trip, event.VoterName)` or the same
   reject path `onReply` uses (`brain.go:489-495`).
4. Anything else (wrong state, unrecognized option): no-op, same as `onReply`'s `intent == nil`
   case.

Then switch the sends at `brain.go:331`, `brain.go:464`, `brain.go:495` (options) and `brain.go:676`
(approval) to call `Messenger.SendPoll` instead of `Messenger.Send` + buttons — but only when the
active messenger supports it meaningfully. Simplest: keep calling `say()`/`Send()` with buttons as
the default (keeps console/telegram untouched), and have `RobotMessenger` specifically prefer a
poll by having `Brain` type-assert `if p, ok := b.Messenger.(interface{ SendPoll(...) error }); ok`
— or just always call `SendPoll` on every messenger per the interface change in §1 and let
`ConsoleMessenger`/`TelegramMessenger` do something reasonable with it. Either is fine; pick
whichever fits the rest of this codebase's style better than I can guess from the outside.

## Non-goals

- Don't touch `models.IncomingMessage` or `RobotMessenger.Send()` — those already match the robot's
  current output for plain messages, verified against a real WhatsApp group.
- Don't add HMAC signature verification or bearer-token checks unless you specifically want that
  hardening — neither side requires it to work today.
- Don't break the `console` or `telegram` backends — `MESSAGING_BACKEND` switching is the whole
  point of the `Messenger` abstraction (see `messaging.go`'s package doc comment).

## Done when

- [ ] `Messenger` has a way to send a poll; `RobotMessenger`'s implementation POSTs
      `{"group_id", "poll": {...}}` to `{ROBOT_URL}/send`.
- [ ] `POST /webhook` correctly discriminates `event: "poll_vote"` from `event: "message"` (or
      absent `event`, for any caller that doesn't send it) before decoding.
- [ ] A vote during `AwaitingChoice` calls `selectOption` with the right position, same as typing
      "1"/"2"/"3" does today.
- [ ] A vote during `AwaitingApproval` triggers `book()` or the reject path, same as ✅/❌ does
      today.
- [ ] `MESSAGING_BACKEND=console` and `MESSAGING_BACKEND=telegram` still build and run (polls are
      additive, not a breaking interface change for them).
