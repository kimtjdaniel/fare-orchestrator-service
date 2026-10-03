// Package orchestrator is the brain. Every incoming group message lands in Brain.Handle().
//
// Flow (code moves the state, Claude only reads/writes text). Each WhatsApp group has a SINGLETON
// trip document (no history of past trips) that gets reset and reused for every new cycle:
//
//	COLLECTING --@mention--> extract prefs + propose options --> AWAITING_CHOICE
//	AWAITING_CHOICE --"2"--> search flights + hotel --> AWAITING_APPROVAL
//	AWAITING_APPROVAL --✅--> book flights + hotel (Skyvern) --> BOOKED
//	                  --❌--> back to AWAITING_CHOICE
//	BOOKED/CANCELLED --@mention--> reset the same doc --> COLLECTING
package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"yate-brain/config"
	"yate-brain/formatting"
	"yate-brain/llm"
	"yate-brain/messaging"
	"yate-brain/models"
	"yate-brain/prompts"
	"yate-brain/state"
	"yate-brain/store"
	"yate-brain/tools"
)

const defaultLeadEmail = "demo@yate.travel"

// Fast paths: obvious replies are handled without an LLM call (faster + free).
var (
	choiceOnlyRe  = regexp.MustCompile(`(?i)^\s*(?:option\s*)?([1-3])\s*[.!]?\s*$`)
	approveOnlyRe = regexp.MustCompile(`(?i)^\s*(✅|👍|yes|yep|book it|approve)\s*!*\s*$`)
	rejectOnlyRe  = regexp.MustCompile(`(?i)^\s*(❌|👎|no|nope)\s*!*\s*$`)
)

type Brain struct {
	Store     store.Store
	LLM       llm.LLM
	Messenger messaging.Messenger
	Config    *config.Settings

	mentionRe *regexp.Regexp
	locksMu   sync.Mutex
	locks     map[string]*sync.Mutex
}

func NewBrain(cfg *config.Settings, st store.Store, llmClient llm.LLM, messenger messaging.Messenger) *Brain {
	return &Brain{
		Store:     st,
		LLM:       llmClient,
		Messenger: messenger,
		Config:    cfg,
		mentionRe: regexp.MustCompile(`(?i)@` + regexp.QuoteMeta(cfg.BotName) + `\b`),
		locks:     map[string]*sync.Mutex{},
	}
}

// today is today's date in the team's timezone. Railway runs in UTC, which is already "tomorrow"
// after 5pm in Vancouver, and that would shift "this weekend" by a day.
func (b *Brain) today() time.Time {
	loc, err := time.LoadLocation(b.Config.Timezone)
	if err != nil {
		loc = time.UTC
	}
	return time.Now().In(loc)
}

// validateOptions checks each option against every participant's stated availability. Returns
// {option.destination: [violation, ...]} for options that don't fit someone. Claude is already
// told these rules (see PROPOSE_SYSTEM); this just catches when it slips.
func validateOptions(options []models.Option, people []models.Participant) map[string][]string {
	violations := map[string][]string{}
	for _, o := range options {
		var issues []string
		for _, p := range people {
			if len(p.GeneralPreferences.Availability) > 0 && !fitsAvailability(o, p.GeneralPreferences.Availability) {
				issues = append(issues, fmt.Sprintf("%s–%s doesn't fit %s's available dates",
					o.EmbarkingDate, o.ReturningDate, p.WhatsAppName))
			}
		}
		if len(issues) > 0 {
			violations[o.Destination] = issues
		}
	}
	return violations
}

// fitsAvailability checks that every day of the option's window is in the participant's
// availability list (a flat list of exact "YYYY-MM-DD" dates, not ranges).
func fitsAvailability(o models.Option, availability []string) bool {
	start, err := models.ParseDate(o.EmbarkingDate)
	if err != nil {
		return false
	}
	end, err := models.ParseDate(o.ReturningDate)
	if err != nil {
		return false
	}
	set := make(map[string]bool, len(availability))
	for _, d := range availability {
		set[d] = true
	}
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		if !set[d.Format("2006-01-02")] {
			return false
		}
	}
	return true
}

// matchName matches a chat display name to an extracted participant. Chat names and Claude's
// extracted names often disagree on nicknames or a last name ("Jordan Lee" in chat vs "Jordan"
// extracted, or the reverse), so try exact, then substring, then first-name before giving up.
func matchName(name string, people []models.Participant) *models.Participant {
	needle := strings.ToLower(strings.TrimSpace(name))
	for i := range people {
		if strings.ToLower(strings.TrimSpace(people[i].WhatsAppName)) == needle {
			return &people[i]
		}
	}
	for i := range people {
		hay := strings.ToLower(strings.TrimSpace(people[i].WhatsAppName))
		if strings.Contains(needle, hay) || strings.Contains(hay, needle) {
			return &people[i]
		}
	}
	needleFields := strings.Fields(needle)
	if len(needleFields) == 0 {
		return nil
	}
	for i := range people {
		fields := strings.Fields(strings.ToLower(people[i].WhatsAppName))
		if len(fields) > 0 && fields[0] == needleFields[0] {
			return &people[i]
		}
	}
	return nil
}

// ------------------------------------------------------------------ entry point

func (b *Brain) lockFor(groupID string) *sync.Mutex {
	b.locksMu.Lock()
	defer b.locksMu.Unlock()
	l, ok := b.locks[groupID]
	if !ok {
		l = &sync.Mutex{}
		b.locks[groupID] = l
	}
	return l
}

// Handle is the entry point for every incoming message: one message at a time per group.
func (b *Brain) Handle(ctx context.Context, m models.IncomingMessage) {
	lock := b.lockFor(m.GroupID)
	lock.Lock()
	defer lock.Unlock()

	if err := b.handle(ctx, m); err != nil {
		slog.Error("failed handling message", "group_id", m.GroupID, "err", err)
		if sayErr := b.say(ctx, m.GroupID, "Oops, something broke on my end 🛠️ Give me a sec and try again.", nil); sayErr != nil {
			slog.Error("failed to send oops message", "group_id", m.GroupID, "err", sayErr)
		}
	}
}

func (b *Brain) handle(ctx context.Context, m models.IncomingMessage) error {
	// Fallback mention detection: WhatsApp uses several ID formats for one account, so the
	// robot's mentionedIds check can miss. Typed "@Yate" in the text always counts.
	if !m.Tagged && b.mentionRe.MatchString(m.Text) {
		m.Tagged = true
	}
	// Trip is a singleton per group: the document's own ID is the group ID.
	trip, err := b.Store.GetTrip(ctx, m.GroupID)
	if err != nil {
		return err
	}
	sentAt := models.Now()
	if m.Timestamp != 0 {
		sentAt = time.Unix(m.Timestamp, 0).UTC()
	}
	tripID := ""
	if trip != nil {
		tripID = trip.ID
	}
	isNew, err := b.Store.SaveMessage(ctx, &models.Message{
		GroupID: m.GroupID, TripID: tripID, ExternalID: m.MessageID, SenderID: m.SenderID,
		SenderName: m.SenderName, Text: m.Text, Tagged: m.Tagged, SentAt: sentAt,
	})
	if err != nil {
		return err
	}
	if !isNew {
		return nil // duplicate delivery (webhook retry)
	}

	if trip == nil {
		if m.Tagged {
			trip, err = b.Store.CreateTrip(ctx, m.GroupID, m.GroupName)
			if err != nil {
				return err
			}
			return b.plan(ctx, trip, "")
		}
		return nil
	}

	if !trip.State.Active() {
		// Terminal trip, same group: reuse the same document for a new cycle rather than keeping
		// history of past trips.
		if m.Tagged {
			trip, err = b.Store.ResetTrip(ctx, trip.ID, m.GroupName)
			if err != nil {
				return err
			}
			return b.plan(ctx, trip, "")
		}
		return nil
	}

	switch trip.State {
	case models.Collecting:
		if m.Tagged {
			return b.plan(ctx, trip, "")
		}
	case models.AwaitingChoice, models.AwaitingApproval:
		return b.onReply(ctx, trip, m)
	case models.Searching, models.BookingState:
		if m.Tagged {
			return b.say(ctx, m.GroupID, "On it, hang tight ⏳", nil)
		}
	}
	return nil
}

// ------------------------------------------------------------------ stage: plan

func (b *Brain) plan(ctx context.Context, trip *models.Trip, feedback string) error {
	history, err := b.Store.GetMessages(ctx, trip.GroupID, trip.HistoryStart, 1000, false)
	if err != nil {
		return err
	}
	lines := make([]string, len(history))
	for i, msg := range history {
		lines[i] = fmt.Sprintf("%s: %s", msg.SenderName, msg.Text)
	}
	transcript := strings.Join(lines, "\n")
	day := b.today().Format("2006-01-02")

	// Two Claude calls take 5-20s; an instant ack keeps the group chat from going silent.
	if err := b.say(ctx, trip.GroupID, "Reading the chat 🧠 give me a few seconds…", nil); err != nil {
		return err
	}

	extracted, err := b.LLM.Structured(ctx, prompts.ExtractSystem(day),
		[]llm.Message{{Role: "user", Content: fmt.Sprintf("Group chat:\n%s", transcript)}},
		toSchema(prompts.RecordPreferences))
	if err != nil {
		return err
	}

	var people []models.Participant
	if err := decodeInto(extracted["participants"], &people); err != nil {
		return err
	}
	if len(people) == 0 {
		return b.say(ctx, trip.GroupID, "I need a bit more to go on. Where's everyone flying from, "+
			"which dates work, and what's your budget?", nil)
	}
	for i := range people {
		if people[i].PID == "" {
			people[i].PID = fmt.Sprintf("p_%d_%d", time.Now().UnixNano(), i)
		}
	}
	fields := map[string]any{"participants": people}
	if origin := people[0].OriginCity; origin != "" {
		fields["origin"] = origin
	} else if people[0].OriginAirport != "" {
		fields["origin"] = people[0].OriginAirport
	}
	trip, err = b.Store.UpdateTrip(ctx, trip.ID, fields)
	if err != nil {
		return err
	}

	// Can't search flights without an origin: ask now, re-plan on the next @mention. Also
	// surface whatever else Claude flagged as missing (budget, dates, ...) rather than silently
	// proposing a plan built on guesses.
	var noOrigin []string
	for _, p := range people {
		if p.OriginAirport == "" {
			noOrigin = append(noOrigin, p.WhatsAppName)
		}
	}
	var missingInfo []string
	if err := decodeInto(extracted["missing_info"], &missingInfo); err != nil {
		return err
	}
	if len(noOrigin) > 0 || len(missingInfo) > 0 {
		var asks []string
		if len(noOrigin) > 0 {
			asks = append(asks, fmt.Sprintf("Where are %s flying from?", strings.Join(noOrigin, ", ")))
		}
		asks = append(asks, missingInfo...)
		return b.say(ctx, trip.GroupID, "Almost there! "+strings.Join(asks, " ")+
			fmt.Sprintf(" Tell me and @%s again.", b.Config.BotName), nil)
	}

	trip, intro, options, ok, err := b.propose(ctx, trip, people, day, feedback, false)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}

	if trip.State != models.AwaitingChoice {
		trip, err = store.SetState(ctx, b.Store, trip.ID, models.AwaitingChoice, nil)
		if err != nil {
			return err
		}
	}
	return b.say(ctx, trip.GroupID, formatting.OptionsMessage(intro, options, trip.ID, b.Config.DashboardURL), optionButtons(options))
}

// propose asks Claude for 2-3 options, persists them onto the trip, and returns the updated trip.
// If every option breaks someone's dates, it re-asks once with the specifics instead of showing
// the group a plan nobody can actually take. ok=false means a message was already sent and
// there's nothing more to do.
func (b *Brain) propose(ctx context.Context, trip *models.Trip, people []models.Participant, day, feedback string, retried bool) (*models.Trip, string, []models.Option, bool, error) {
	fb := ""
	if feedback != "" {
		fb = "\nThe group asked for changes: " + feedback
	}
	lines := make([]string, len(people))
	for i, p := range people {
		pj, err := json.Marshal(p)
		if err != nil {
			return trip, "", nil, false, err
		}
		lines[i] = string(pj)
	}
	content := "Participants:\n" + strings.Join(lines, "\n")

	proposal, err := b.LLM.Structured(ctx, prompts.ProposeSystem(b.Config.BotName, day, fb),
		[]llm.Message{{Role: "user", Content: content}}, toSchema(prompts.ProposeOptions))
	if err != nil {
		return trip, "", nil, false, err
	}

	rawOptions, _ := proposal["options"].([]any)
	var options []models.Option
	for _, raw := range rawOptions {
		if len(options) >= 3 {
			break
		}
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		// structured outputs can't enforce date formats, so a bad option is skipped, not fatal
		opt, err := optionFromMap(len(options)+1, m)
		if err != nil {
			slog.Warn("dropping invalid option", "option", m, "err", err)
			continue
		}
		options = append(options, opt)
	}
	if len(options) == 0 {
		return trip, "", nil, false, b.say(ctx, trip.GroupID, "I couldn't come up with a plan that fits everyone. "+
			"Can you loosen the dates a little?", nil)
	}

	violations := validateOptions(options, people)
	if len(violations) == len(options) && !retried {
		var detailParts []string
		for dest, issues := range violations {
			detailParts = append(detailParts, fmt.Sprintf("%s: %s.", dest, strings.Join(issues, "; ")))
		}
		detail := strings.Join(detailParts, " ")
		slog.Info("trip: every option violates someone's dates, re-asking once", "trip", trip.ID)
		return b.propose(ctx, trip, people, day, fmt.Sprintf(
			"Every option broke someone's dates. %s Try again, fitting every participant's availability exactly.", detail), true)
	}

	trip, err = b.Store.UpdateTrip(ctx, trip.ID, map[string]any{"options": options})
	if err != nil {
		return trip, "", nil, false, err
	}
	intro, _ := proposal["intro"].(string)
	return trip, intro, options, true, nil
}

func optionFromMap(position int, m map[string]any) (models.Option, error) {
	var o models.Option
	o.Position = position
	var err error
	if o.Destination, err = requiredString(m, "destination"); err != nil {
		return o, err
	}
	if o.DestinationAirport, err = requiredString(m, "destination_airport"); err != nil {
		return o, err
	}
	if o.ActivityDescription, err = requiredString(m, "activity_description"); err != nil {
		return o, err
	}
	if o.CulinaryDescription, err = requiredString(m, "culinary_description"); err != nil {
		return o, err
	}
	if o.EmbarkingDate, err = requiredString(m, "embarking_date"); err != nil {
		return o, err
	}
	if o.ReturningDate, err = requiredString(m, "returning_date"); err != nil {
		return o, err
	}
	if o.WhyItWorks, err = requiredString(m, "why_it_works"); err != nil {
		return o, err
	}
	if o.Tradeoffs, err = requiredString(m, "tradeoffs"); err != nil {
		return o, err
	}
	if v, ok := m["duration_nights"].(float64); ok {
		o.DurationNights = int(v)
	}
	if v, ok := m["cost_per_person"].(float64); ok {
		o.CostPerPerson = &v
	}
	if _, err := models.ParseDate(o.EmbarkingDate); err != nil {
		return o, fmt.Errorf("bad embarking_date: %w", err)
	}
	if _, err := models.ParseDate(o.ReturningDate); err != nil {
		return o, fmt.Errorf("bad returning_date: %w", err)
	}
	return o, nil
}

func requiredString(m map[string]any, key string) (string, error) {
	v, ok := m[key].(string)
	if !ok || v == "" {
		return "", fmt.Errorf("missing %s", key)
	}
	return v, nil
}

// replan re-proposes options for already-known participants ("make it cheaper", "swap to the
// beach one"). No re-extraction: the group is reacting to the options, not restating
// preferences, so re-reading the whole chat through Claude again would just add latency.
func (b *Brain) replan(ctx context.Context, trip *models.Trip, feedback string) error {
	trip, intro, options, ok, err := b.propose(ctx, trip, trip.Participants, b.today().Format("2006-01-02"), feedback, false)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	return b.say(ctx, trip.GroupID, formatting.OptionsMessage(intro, options, trip.ID, b.Config.DashboardURL), optionButtons(options))
}

// ------------------------------------------------------------------ replies

func (b *Brain) onReply(ctx context.Context, trip *models.Trip, m models.IncomingMessage) error {
	intent, err := b.interpret(ctx, trip, m)
	if err != nil {
		return err
	}
	if intent == nil {
		return nil
	}
	kind, _ := intent["intent"].(string)
	slog.Info("trip intent", "trip", trip.ID, "state", trip.State, "intent", kind)

	switch {
	case kind == "choose" && trip.State == models.AwaitingChoice:
		num := 0
		if n, ok := intent["option_number"].(float64); ok {
			num = int(n)
		}
		return b.selectOption(ctx, trip, num)
	case kind == "approve" && trip.State == models.AwaitingApproval:
		return b.book(ctx, trip, m.SenderName)
	case kind == "reject" && trip.State == models.AwaitingApproval:
		trip, err = store.SetState(ctx, b.Store, trip.ID, models.AwaitingChoice, nil)
		if err != nil {
			return err
		}
		return b.say(ctx, trip.GroupID, formatting.OptionsMessage("No problem. Here are the options again:",
			trip.Options, trip.ID, b.Config.DashboardURL), optionButtons(trip.Options))
	case kind == "revise":
		if trip.State == models.AwaitingApproval {
			trip, err = store.SetState(ctx, b.Store, trip.ID, models.AwaitingChoice, nil)
			if err != nil {
				return err
			}
		}
		revision, _ := intent["revision_request"].(string)
		if revision == "" {
			revision = m.Text
		}
		return b.replan(ctx, trip, revision)
	case kind == "cancel":
		if _, err := store.SetState(ctx, b.Store, trip.ID, models.Cancelled, nil); err != nil {
			return err
		}
		return b.say(ctx, trip.GroupID, "Trip planning cancelled. @ me whenever you want to start again 👋", nil)
	case kind == "question":
		return b.answerQuestion(ctx, trip, m)
	case kind == "approve" && trip.State == models.AwaitingChoice:
		return b.say(ctx, trip.GroupID, "Pick an option first. Reply 1, 2 or 3 👆", nil)
	}
	return nil
}

// interpret uses regex for the obvious replies; Claude only for @mentions that need understanding.
func (b *Brain) interpret(ctx context.Context, trip *models.Trip, m models.IncomingMessage) (map[string]any, error) {
	if trip.State == models.AwaitingChoice {
		if hit := choiceOnlyRe.FindStringSubmatch(m.Text); hit != nil {
			n, _ := strconv.Atoi(hit[1])
			return map[string]any{"intent": "choose", "option_number": float64(n)}, nil
		}
	}
	if trip.State == models.AwaitingApproval {
		if approveOnlyRe.MatchString(m.Text) {
			return map[string]any{"intent": "approve"}, nil
		}
		if rejectOnlyRe.MatchString(m.Text) {
			return map[string]any{"intent": "reject"}, nil
		}
	}
	if !m.Tagged {
		return nil, nil // ordinary chatter: saved, but no reply and no LLM spend
	}
	optsDesc := "none"
	if len(trip.Options) > 0 {
		parts := make([]string, len(trip.Options))
		for i, o := range trip.Options {
			parts[i] = fmt.Sprintf("%d. %s", o.Position, o.Destination)
		}
		optsDesc = strings.Join(parts, "; ")
	}
	return b.LLM.Structured(ctx, prompts.InterpretSystem(string(trip.State), optsDesc),
		[]llm.Message{{Role: "user", Content: m.Text}}, toSchema(prompts.InterpretReply))
}

func (b *Brain) answerQuestion(ctx context.Context, trip *models.Trip, m models.IncomingMessage) error {
	ctxBlob, err := json.Marshal(map[string]any{
		"state": trip.State, "options": trip.Options, "itinerary": trip.Itinerary,
	})
	if err != nil {
		return err
	}
	answer, err := b.LLM.Agent(ctx, prompts.AgentSystem(b.Config.BotName, string(ctxBlob)),
		[]llm.Message{{Role: "user", Content: fmt.Sprintf("%s: %s", m.SenderName, m.Text)}},
		tools.AgentTools, tools.AgentHandlers(b.Config))
	if err != nil {
		return err
	}
	return b.say(ctx, trip.GroupID, answer, nil)
}

// ------------------------------------------------------------------ stage: search

// selectOption flattens the chosen Option onto the trip's own fields (the singleton document's
// "plan" shape), then searches one shared flight itinerary + hotel for the whole group — everyone
// currently flies from the same origin (see Participant.OriginAirport).
func (b *Brain) selectOption(ctx context.Context, trip *models.Trip, number int) error {
	var option *models.Option
	for i := range trip.Options {
		if trip.Options[i].Position == number {
			option = &trip.Options[i]
			break
		}
	}
	if option == nil {
		return b.say(ctx, trip.GroupID, fmt.Sprintf("I only have options 1–%d. Which one?", len(trip.Options)), nil)
	}

	fields := map[string]any{
		"chosen_option_position": option.Position,
		"destination":            option.Destination, "destination_airport": option.DestinationAirport,
		"activity_description": option.ActivityDescription, "culinary_description": option.CulinaryDescription,
		"duration_nights": option.DurationNights,
		"embarking_date":  option.EmbarkingDate, "returning_date": option.ReturningDate,
	}
	if option.CostPerPerson != nil {
		fields["cost_per_person"] = *option.CostPerPerson
	}
	trip, err := b.Store.UpdateTrip(ctx, trip.ID, fields)
	if err != nil {
		return err
	}
	trip, err = store.SetState(ctx, b.Store, trip.ID, models.Searching, nil)
	if err != nil {
		return err
	}
	if err := b.say(ctx, trip.GroupID, fmt.Sprintf("%s it is! Finding flights and a hotel 🔎", option.Destination), nil); err != nil {
		return err
	}

	people := trip.Participants
	originAirport := trip.Origin
	if len(people) > 0 && people[0].OriginAirport != "" {
		originAirport = people[0].OriginAirport
	}

	offers, err := tools.SearchFlights(ctx, b.Config, originAirport, option.DestinationAirport, option.EmbarkingDate, option.ReturningDate)
	if err != nil || len(offers) == 0 {
		if _, sErr := store.SetState(ctx, b.Store, trip.ID, models.AwaitingChoice, nil); sErr != nil {
			return sErr
		}
		return b.say(ctx, trip.GroupID, fmt.Sprintf("Couldn't find availability for %s (no flights %s->%s). Pick another option?",
			option.Destination, originAirport, option.DestinationAirport), nil)
	}
	offer := offers[0]

	hotels, err := tools.SearchHotels(ctx, b.Config, option.Destination, option.EmbarkingDate, option.ReturningDate, len(people), nil)
	if err != nil || len(hotels) == 0 {
		if _, sErr := store.SetState(ctx, b.Store, trip.ID, models.AwaitingChoice, nil); sErr != nil {
			return sErr
		}
		return b.say(ctx, trip.GroupID, fmt.Sprintf("Couldn't find availability for %s (no hotels). Pick another option?", option.Destination), nil)
	}
	hotel := hotels[0]

	names := make([]string, len(people))
	payer := "the group"
	for i, p := range people {
		names[i] = p.WhatsAppName
		if i == 0 {
			payer = p.WhatsAppName
		}
	}
	preview := tools.ComputeSplit(names, offer.Price, hotel.TotalPrice, payer)

	embarkOffer := offer
	returnOffer := offer
	returnOffer.Origin, returnOffer.Destination = offer.Destination, offer.Origin

	itinMap, err := structToMap(map[string]any{
		"flights":     map[string]any{"embarking": embarkOffer, "returning": returnOffer},
		"hotel":       hotel,
		"per_person":  preview.PerPerson,
		"group_total": preview.GroupTotal,
	})
	if err != nil {
		return err
	}

	ed, _ := models.ParseDate(option.EmbarkingDate)
	rd, _ := models.ParseDate(option.ReturningDate)
	half := offer.Price / 2
	flights := []models.Flight{
		{Direction: models.Embarking, BookingStatus: models.StatusIncomplete, DepartingDate: ed, ArrivalDate: ed, Costs: &half},
		{Direction: models.Returning, BookingStatus: models.StatusIncomplete, DepartingDate: rd, ArrivalDate: rd, Costs: &half},
	}
	accommodations := []models.Accommodation{{
		BookingStatus: models.StatusIncomplete, CheckInDate: ed, CheckOutDate: rd,
		Rating: hotel.Rating, Costs: &hotel.TotalPrice, BookingURL: hotel.CheckoutURL,
	}}

	trip, err = store.SetState(ctx, b.Store, trip.ID, models.AwaitingApproval, map[string]any{
		"itinerary": itinMap, "flights": flights, "accommodations": accommodations,
	})
	if err != nil {
		return err
	}
	chosen := trip.ChosenOption()
	return b.say(ctx, trip.GroupID, formatting.SummaryMessage(*chosen, itinMap, people, trip.ID, b.Config.DashboardURL),
		[]messaging.Button{{Label: "✅ Book it", Payload: "✅"}, {Label: "❌ Back", Payload: "❌"}})
}

// ------------------------------------------------------------------ stage: book

func (b *Brain) book(ctx context.Context, trip *models.Trip, approver string) error {
	trip, err := store.SetState(ctx, b.Store, trip.ID, models.BookingState, map[string]any{
		"approved_by": approver, "approved_at": models.Now(),
	})
	if err != nil {
		return err
	}
	if err := state.AssertCanBook(trip); err != nil {
		return err
	}

	people := trip.Participants
	option := trip.ChosenOption()
	if option == nil {
		return fmt.Errorf("trip %s has no chosen option", trip.ID)
	}
	itin := trip.Itinerary
	if err := b.say(ctx, trip.GroupID, fmt.Sprintf("Approved by %s! Booking now 🚀", approver), nil); err != nil {
		return err
	}

	// Flights: skip legs already booked (makes ✅-to-retry safe after a hotel failure).
	flights := append([]models.Flight(nil), trip.Flights...)
	flightsRaw, _ := itin["flights"].(map[string]any)
	for i := range flights {
		if flights[i].BookingStatus == models.StatusBooked {
			continue
		}
		raw, ok := flightsRaw[string(flights[i].Direction)]
		if !ok {
			continue
		}
		var offer models.FlightOffer
		if err := decodeInto(raw, &offer); err != nil {
			return err
		}
		fb, err := tools.BookFlight(ctx, b.Config, offer, approver)
		if err != nil {
			return err
		}
		flights[i].BookingStatus = models.StatusBooked
		flights[i].ProviderRef = fb.PNR
		// fb.Price is the round-trip offer's per-person price (the same offer backs both legs);
		// halve it so Costs summed across both legs still equals one round-trip price per person.
		cost := fb.Price / 2
		flights[i].Costs = &cost
	}
	if trip, err = b.Store.UpdateTrip(ctx, trip.ID, map[string]any{"flights": flights}); err != nil {
		return err
	}

	// Hotel: Skyvern (or mock). Starts instantly, then we wait for the browser to finish.
	var hotel models.HotelOffer
	if err := decodeInto(itin["hotel"], &hotel); err != nil {
		return err
	}
	accommodations := append([]models.Accommodation(nil), trip.Accommodations...)
	if len(accommodations) == 0 {
		ci, _ := models.ParseDate(option.EmbarkingDate)
		co, _ := models.ParseDate(option.ReturningDate)
		accommodations = []models.Accommodation{{
			BookingStatus: models.StatusIncomplete, CheckInDate: ci, CheckOutDate: co,
			Rating: hotel.Rating, Costs: &hotel.TotalPrice, BookingURL: hotel.CheckoutURL,
		}}
	}
	acc := &accommodations[0]

	if acc.BookingStatus != models.StatusBooked {
		runID, liveURL, err := tools.StartHotelBooking(ctx, b.Config, hotel, len(people), approver, defaultLeadEmail,
			fmt.Sprintf("Trip %s hotel", trip.ID))
		if err != nil {
			return err
		}
		if liveURL != "" {
			if err := b.say(ctx, trip.GroupID, fmt.Sprintf("Watch me book the hotel live 👀 %s", liveURL), nil); err != nil {
				return err
			}
		}

		result, err := tools.WaitForBooking(ctx, b.Config, runID, 3*time.Second, 600*time.Second)
		if err != nil {
			return err
		}
		if result.Status != "completed" || result.ConfirmationNumber == "" {
			if _, err := b.Store.UpdateTrip(ctx, trip.ID, map[string]any{"accommodations": accommodations}); err != nil {
				return err
			}
			if _, err := store.SetState(ctx, b.Store, trip.ID, models.AwaitingApproval, nil); err != nil {
				return err
			}
			return b.say(ctx, trip.GroupID, "Flights are booked ✈️ but the hotel booking hit a snag. "+
				"Reply ✅ to retry the hotel.", nil)
		}

		cost := hotel.TotalPrice
		if result.TotalPrice != nil {
			cost = *result.TotalPrice
		}
		acc.BookingStatus = models.StatusBooked
		acc.ProviderRef = result.ConfirmationNumber
		acc.Costs = &cost
	}
	if trip, err = b.Store.UpdateTrip(ctx, trip.ID, map[string]any{"accommodations": accommodations}); err != nil {
		return err
	}

	payer := approver
	if match := matchName(approver, people); match != nil {
		payer = match.WhatsAppName
	} else if len(people) > 0 {
		payer = people[0].WhatsAppName
	}
	names := make([]string, len(people))
	for i, p := range people {
		names[i] = p.WhatsAppName
	}
	var flightPricePerPerson float64
	for _, f := range flights {
		if f.Costs != nil {
			flightPricePerPerson += *f.Costs
		}
	}
	hotelCost := 0.0
	if acc.Costs != nil {
		hotelCost = *acc.Costs
	}
	split := tools.ComputeSplit(names, flightPricePerPerson, hotelCost, payer)
	splitMap, err := structToMap(split)
	if err != nil {
		return err
	}
	newItin := make(map[string]any, len(itin)+1)
	for k, v := range itin {
		newItin[k] = v
	}
	newItin["split"] = splitMap
	if _, err := store.SetState(ctx, b.Store, trip.ID, models.Booked, map[string]any{"itinerary": newItin}); err != nil {
		return err
	}

	var embarkRef, returnRef string
	for _, f := range flights {
		switch f.Direction {
		case models.Embarking:
			embarkRef = f.ProviderRef
		case models.Returning:
			returnRef = f.ProviderRef
		}
	}
	return b.say(ctx, trip.GroupID, formatting.ConfirmationMessage(option.Destination, embarkRef, returnRef, acc.ProviderRef, split), nil)
	// TODO(P4, hour 12-15): trigger the ElevenLabs hotel call here and post its result.
}

// ------------------------------------------------------------------ outbound

func (b *Brain) say(ctx context.Context, groupID, text string, buttons []messaging.Button) error {
	if err := b.Messenger.Send(ctx, groupID, text, buttons); err != nil {
		return err
	}
	trip, err := b.Store.GetTrip(ctx, groupID)
	if err != nil {
		return err
	}
	tripID := ""
	if trip != nil {
		tripID = trip.ID
	}
	_, err = b.Store.SaveMessage(ctx, &models.Message{
		GroupID: groupID, TripID: tripID, SenderID: "bot", SenderName: b.Config.BotName,
		Text: text, IsBot: true, SentAt: models.Now(),
	})
	return err
}

// ------------------------------------------------------------------ helpers

func optionButtons(options []models.Option) []messaging.Button {
	buttons := make([]messaging.Button, len(options))
	for i, o := range options {
		label := strconv.Itoa(o.Position)
		buttons[i] = messaging.Button{Label: label, Payload: label}
	}
	return buttons
}

// toSchema adapts a prompts.go {"name":..., "schema":...} map into llm.Schema.
func toSchema(m map[string]any) llm.Schema {
	name, _ := m["name"].(string)
	schema, _ := m["schema"].(map[string]any)
	return llm.Schema{Name: name, Schema: schema}
}

// decodeInto round-trips through JSON to decode a loosely-typed value (map[string]any / []any,
// from Claude's structured output) into a concrete Go type.
func decodeInto[T any](v any, out *T) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

func structToMap(v any) (map[string]any, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return m, nil
}
