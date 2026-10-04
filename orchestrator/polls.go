package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"fare-brain/formatting"
	"fare-brain/messaging"
	"fare-brain/models"
)

var (
	dashboardAskRe = regexp.MustCompile(`(?i)(dashboard|session link|live session|trip page|details page|the link again|send (me |us )?(the )?(url|link)|localhost|unique (link|url|session)|planning (page|session)|create a new session)`)
	stuckRe        = regexp.MustCompile(`(?i)(can'?t decide|cannot decide|undecided|we'?re stuck|help us pick|make a poll|start a poll|put it (in )?a poll|flip a coin)`)
	wantBudgetRe   = regexp.MustCompile(`(?i)\bbudget\b`)
	wantDatesRe    = regexp.MustCompile(`(?i)\b(dates?|weekend|when (do|should) we|calendar)\b`)
	wantDestRe     = regexp.MustCompile(`(?i)\b(destination|where (should|do) we|which city|which trip|where to go)\b`)
	wantFlightRe   = regexp.MustCompile(`(?i)\b(flights?|direct|layover|non[- ]?stop)\b`)
	cancelBookRe   = regexp.MustCompile(`(?i)\b(cancel|cancelled|call (it )?off|scrap (it|the)|undo the booking|void)\b`)
	replanRe       = regexp.MustCompile(`(?i)(change (?:of |the |if )?plan|change (?:the )?(?:destination|city)|switch (?:the )?(?:destination|city|trip)|instead|different (?:city|destination|trip)|help us plan|pivot)`)
	planRecapRe    = regexp.MustCompile(`(?i)\b(?:run|show|walk|replay|repeat|recap)(?:\s+\w+){0,4}\s+plan\b|\bplan again\b|\bthrough the plan\b`)
	dontCancelRe   = regexp.MustCompile(`(?i)\b(?:don'?t|do not|never|not)\b.{0,24}\bcancel`)
)

func looksLikeDashboardAsk(text string) bool {
	return dashboardAskRe.MatchString(text)
}

func looksLikeStuck(text string) bool {
	return stuckRe.MatchString(text)
}

func looksLikeCancelBooking(text string) bool {
	if dontCancelRe.MatchString(text) {
		return false
	}
	return cancelBookRe.MatchString(text)
}

func looksLikePlanRecap(text string) bool {
	return planRecapRe.MatchString(text)
}

func looksLikeReplan(text string) bool {
	if looksLikePlanRecap(text) || looksLikeItineraryAsk(text) || looksLikeStatusAsk(text) || looksLikeRestaurantAsk(text) || looksLikeFoodMoneyAsk(text) {
		return false
	}
	if looksLikeCancelBooking(text) {
		return true
	}
	if looksLikeBookAsk(text) || looksLikeDashboardAsk(text) {
		return false
	}
	return replanRe.MatchString(text)
}

func (b *Brain) tripPageURL(ctx context.Context, trip *models.Trip, sessionID string) string {
	base := ""
	if b.Config != nil {
		base = strings.TrimSpace(b.Config.DashboardURL)
	}
	gid := strings.TrimSpace(trip.GroupID)
	if gid == "" {
		gid = strings.TrimSpace(trip.ID)
	}
	gid = models.CanonicalGroupID(gid)
	sid := strings.TrimSpace(sessionID)
	if sid == "" && b.Dashboard != nil {
		sid, _ = b.Dashboard.CurrentID(ctx, gid)
	}
	return formatting.SessionLink(base, gid, sid)
}

func (b *Brain) shareLiveSearch(ctx context.Context, trip *models.Trip, sessionID string) error {
	link := b.tripPageURL(ctx, trip, sessionID)
	if link == "" {
		return b.say(ctx, trip.GroupID, "I don't have a trip page set up to send.", nil)
	}
	text := "Watch the flight and hotel search live:\n" + link
	if err := b.say(ctx, trip.GroupID, text, nil); err != nil {
		return err
	}
	_, err := b.Store.UpdateTrip(ctx, trip.ID, map[string]any{"shared_dashboard": true})
	return err
}

func (b *Brain) shareDashboard(ctx context.Context, trip *models.Trip) error {
	if trip.State != models.Searching && trip.State != models.BookingState && trip.State != models.AwaitingApproval && trip.State != models.Booked {
		return b.say(ctx, trip.GroupID, "I'll send the live search page after the group finalizes the plan.", nil)
	}
	return b.shareLiveSearch(ctx, trip, "")
}

func (b *Brain) postOptions(ctx context.Context, trip *models.Trip, intro string, options []models.Option) error {
	if err := b.say(ctx, trip.GroupID, formatting.OptionsMessage(intro, options, trip.ID, ""), optionButtons(options)); err != nil {
		return err
	}
	return b.pollOptions(ctx, trip, options)
}

func (b *Brain) postSummary(ctx context.Context, trip *models.Trip, chosen models.Option, itin map[string]any, people []models.Participant) error {
	updated, err := b.Store.UpdateTrip(ctx, trip.ID, map[string]any{"last_poll": "", "pending_approver": ""})
	if err != nil {
		return err
	}
	if updated != nil {
		trip = updated
	}
	return b.shareItinerary(ctx, trip)
}

func (b *Brain) shareItinerary(ctx context.Context, trip *models.Trip) error {
	url := b.tripPageURL(ctx, trip, "")
	return b.say(ctx, trip.GroupID, formatting.SummaryMessage(models.Option{}, nil, nil, trip.ID, url), nil)
}

func (b *Brain) askWhoPays(ctx context.Context, trip *models.Trip, people []models.Participant) error {
	if hasDesignatedPayer(people) {
		return nil
	}
	names := make([]string, len(people))
	for i, p := range people {
		names[i] = p.WhatsAppName
	}
	return b.sendPoll(ctx, trip.GroupID, "Who's paying?", names, "payer", trip.ID)
}

func (b *Brain) maybeAskPayer(ctx context.Context, trip *models.Trip, force bool) error {
	if hasDesignatedPayer(trip.Participants) {
		return nil
	}
	if trip.AskedPayer && !force {
		return nil
	}
	if err := b.askWhoPays(ctx, trip, trip.Participants); err != nil {
		return err
	}
	updated, err := b.Store.UpdateTrip(ctx, trip.ID, map[string]any{"asked_payer": true})
	if err != nil {
		return err
	}
	if updated != nil {
		*trip = *updated
	}
	return nil
}

// Legacy booking replies return the completed itinerary.
func (b *Brain) requestBooking(ctx context.Context, trip *models.Trip, approver string) error {
	return b.shareItinerary(ctx, trip)
}

// resumeBookingIfPending finishes a booking that was approved before a payer was designated,
// once PendingApprover is set — see requestBooking.
func (b *Brain) resumeBookingIfPending(ctx context.Context, trip *models.Trip, pendingApprover string) error {
	if pendingApprover == "" {
		return nil
	}
	return b.book(ctx, trip, pendingApprover)
}

func (b *Brain) capturePayer(ctx context.Context, trip *models.Trip, m models.IncomingMessage) (*models.Trip, error) {
	if !looksLikeIPay(m.Text) || hasDesignatedPayer(trip.Participants) {
		return trip, nil
	}
	match := matchName(m.SenderName, trip.Participants)
	if match == nil {
		return trip, nil
	}
	people := trip.Participants
	for i := range people {
		people[i].Payer = people[i].WhatsAppName == match.WhatsAppName
	}
	pendingApprover := strings.TrimSpace(trip.PendingApprover)
	updated, err := b.Store.UpdateTrip(ctx, trip.ID, map[string]any{
		"participants":     people,
		"payer_name":       match.WhatsAppName,
		"asked_payer":      true,
		"pending_approver": "",
	})
	if err != nil {
		return trip, err
	}
	if updated != nil {
		trip = updated
	}
	if pendingApprover != "" {
		if err := b.say(ctx, trip.GroupID, match.WhatsAppName+" is on the hook for this one. 💳", nil); err != nil {
			return trip, err
		}
		if err := b.resumeBookingIfPending(ctx, trip, pendingApprover); err != nil {
			return trip, err
		}
	}
	return trip, nil
}

func (b *Brain) applyPayerVote(ctx context.Context, trip *models.Trip, vote models.PollVote, selected string) error {
	if selected == "" {
		return nil
	}
	match := matchName(selected, trip.Participants)
	if match == nil {
		return b.say(ctx, trip.GroupID, "Didn't catch who that was — tap the poll option again.", nil)
	}
	people := trip.Participants
	for i := range people {
		people[i].Payer = people[i].WhatsAppName == match.WhatsAppName
	}
	pendingApprover := strings.TrimSpace(trip.PendingApprover)
	updated, err := b.Store.UpdateTrip(ctx, trip.ID, map[string]any{
		"participants":     people,
		"payer_name":       match.WhatsAppName,
		"asked_payer":      true,
		"pending_approver": "",
	})
	if err != nil {
		return err
	}
	if updated != nil {
		trip = updated
	}
	if err := b.say(ctx, trip.GroupID, match.WhatsAppName+" is on the hook for this one. 💳", nil); err != nil {
		return err
	}
	return b.resumeBookingIfPending(ctx, trip, pendingApprover)
}

func hasDesignatedPayer(people []models.Participant) bool {
	for _, p := range people {
		if p.Payer {
			return true
		}
	}
	return false
}

func (b *Brain) pollOptions(ctx context.Context, trip *models.Trip, options []models.Option) error {
	choices := formatting.OptionPollChoices(options)
	if len(choices) < 2 {
		return nil
	}
	return b.sendPoll(ctx, trip.GroupID, "Which trip?", choices, "options", trip.ID)
}

func (b *Brain) askOrigin(ctx context.Context, trip *models.Trip) error {
	return b.say(ctx, trip.GroupID, "Ask for the missing departure city for each traveler. Use existing preferences; do not suggest arbitrary origins.", nil)
}

func (b *Brain) askDates(ctx context.Context, trip *models.Trip) error {
	return b.say(ctx, trip.GroupID, "Ask which travel dates work for the group. Keep any dates and duration already supplied; do not invent a weekend.", nil)
}

func (b *Brain) askBudget(ctx context.Context, trip *models.Trip) error {
	if err := b.say(ctx, trip.GroupID, "What's a comfortable spend per person? All CAD.", nil); err != nil {
		return err
	}
	return b.sendPoll(ctx, trip.GroupID, "Budget per person (CAD)", []string{
		"Under C$800",
		"Around C$1,200",
		"Around C$2,000",
		"Flexible if the trip's good",
	}, "budget", trip.ID)
}

func (b *Brain) askVibe(ctx context.Context, trip *models.Trip) error {
	if err := b.say(ctx, trip.GroupID, "If the destination is the hard part, pick a vibe and I'll work from that.", nil); err != nil {
		return err
	}
	return b.sendPoll(ctx, trip.GroupID, "What kind of trip?", []string{
		"Beach",
		"City",
		"Somewhere quiet",
		"Surprise us",
	}, "vibe", trip.ID)
}

func (b *Brain) helpDecide(ctx context.Context, trip *models.Trip, m models.IncomingMessage) error {
	text := m.Text
	switch {
	case wantBudgetRe.MatchString(text) && trip.BudgetNote != "":
		return b.say(ctx, trip.GroupID, "If you want to change the budget, say the new number and I'll put up a yes/no for the group.", nil)
	case wantBudgetRe.MatchString(text):
		return b.askBudget(ctx, trip)
	case wantFlightRe.MatchString(text) && trip.State == models.AwaitingApproval:
		return b.sendPoll(ctx, trip.GroupID, "Flights — what matters more?", []string{
			"Direct if we can",
			"Stops are fine if it's cheaper",
			"Morning departures",
			"Whatever's cheapest",
		}, "flights", trip.ID)
	case wantDestRe.MatchString(text) && destIsSet(trip):
		return b.say(ctx, trip.GroupID, "If you want a different destination, say the city and I'll ask everyone yes or no.", nil)
	case wantDestRe.MatchString(text) && len(trip.Options) >= 2:
		return b.pollOptions(ctx, trip, trip.Options)
	case wantDestRe.MatchString(text):
		return b.askVibe(ctx, trip)
	case wantDatesRe.MatchString(text) && hasAnyDates(trip.Participants):
		return b.say(ctx, trip.GroupID, "If you want to move dates, say the new ones and I'll ask the group yes or no.", nil)
	case wantDatesRe.MatchString(text) || !hasAnyDates(trip.Participants):
		return b.askDates(ctx, trip)
	case !hasSharedOrigin(trip.Participants):
		return b.askOrigin(ctx, trip)
	case trip.State == models.AwaitingChoice && len(trip.Options) >= 2:
		return b.pollOptions(ctx, trip, trip.Options)
	case trip.State == models.AwaitingApproval:
		return b.shareItinerary(ctx, trip)
	default:
		return b.askBudget(ctx, trip)
	}
}

func (b *Brain) sendPoll(ctx context.Context, groupID, name string, options []string, kind, tripID string) error {
	opts := clipPollOptions(options)
	if len(opts) < 2 {
		return nil
	}
	pollID, err := b.Messenger.SendPoll(ctx, groupID, messaging.Poll{Name: name, Options: opts})
	if err != nil {
		return err
	}
	if err := b.recordBotMessage(ctx, groupID, "Poll: "+name+"\nOptions: "+strings.Join(opts, " | "), pollID); err != nil {
		return err
	}
	if tripID != "" {
		trip, err := b.Store.UpdateTrip(ctx, tripID, map[string]any{"last_poll": kind})
		if err != nil {
			return err
		}
		if _, err := b.Store.SaveWhatsAppSession(ctx, "poll:"+groupID+":"+pollID, map[string]any{
			"session_id": trip.SessionID, "kind": kind, "options": opts, "plan_key": pollPlanKey(trip),
		}); err != nil {
			return err
		}
	}
	return nil
}

func clipPollPair(options, values []string) ([]string, []string) {
	seen := map[string]bool{}
	opts := make([]string, 0, len(options))
	vals := make([]string, 0, len(options))
	for i, o := range options {
		o = strings.TrimSpace(o)
		if o == "" {
			continue
		}
		if len(o) > 100 {
			o = strings.TrimSpace(o[:97]) + "…"
		}
		key := strings.ToLower(o)
		if seen[key] {
			continue
		}
		seen[key] = true
		opts = append(opts, o)
		v := ""
		if i < len(values) {
			v = values[i]
		}
		vals = append(vals, v)
		if len(opts) >= 12 {
			break
		}
	}
	return opts, vals
}

func clipPollOptions(opts []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(opts))
	for _, o := range opts {
		o = strings.TrimSpace(o)
		if o == "" {
			continue
		}
		if len(o) > 100 {
			o = strings.TrimSpace(o[:97]) + "…"
		}
		key := strings.ToLower(o)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, o)
		if len(out) >= 12 {
			break
		}
	}
	return out
}

func matchOptionFromLabel(label string, options []models.Option) *models.Option {
	lower := strings.ToLower(label)
	for i := range options {
		dest := strings.ToLower(strings.TrimSpace(options[i].Destination))
		if dest != "" && strings.Contains(lower, dest) {
			return &options[i]
		}
	}
	if hit := choiceOnlyRe.FindStringSubmatch(label); hit != nil {
		n, _ := strconv.Atoi(hit[1])
		for i := range options {
			if options[i].Position == n {
				return &options[i]
			}
		}
	}
	return nil
}

func isLocalOrFakeURL(url string) bool {
	u := strings.ToLower(url)
	return strings.Contains(u, "localhost") || strings.Contains(u, "127.0.0.1") || strings.Contains(u, "example-fake")
}

func comingWeekend(today time.Time) (time.Time, time.Time) {
	wd := today.Weekday()
	if wd == time.Saturday {
		return today, today.AddDate(0, 0, 1)
	}
	if wd == time.Sunday {
		fri := today.AddDate(0, 0, 5)
		return fri, fri.AddDate(0, 0, 2)
	}
	untilFri := (int(time.Friday) - int(wd) + 7) % 7
	fri := today.AddDate(0, 0, untilFri)
	return fri, fri.AddDate(0, 0, 2)
}

func dateList(start, end time.Time) []string {
	var out []string
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		out = append(out, d.Format("2006-01-02"))
	}
	return out
}

func (b *Brain) HandlePollVote(ctx context.Context, vote models.PollVote) {
	if vote.GroupID == "" {
		return
	}
	lock := b.lockFor(vote.GroupID)
	lock.Lock()
	defer lock.Unlock()
	ctx = withReplyTurn(ctx)
	err := b.handlePollVote(ctx, vote)
	if err == nil {
		err = b.flushReply(ctx, vote.GroupID)
	}
	if err != nil {
		_ = b.replyUnavailable(ctx, vote.GroupID)
	}
}

func (b *Brain) handlePollVote(ctx context.Context, vote models.PollVote) error {
	trip, err := b.Store.GetTrip(ctx, vote.GroupID)
	if err != nil || trip == nil {
		return err
	}
	if err := b.ensureSession(ctx, trip); err != nil {
		return err
	}
	// Only a poll sent for this session can drive its workflow. Never interpret
	// an unrelated group's poll, an old trip's vote, or a poll title as consent.
	poll, err := b.Store.GetWhatsAppSession(ctx, "poll:"+vote.GroupID+":"+vote.PollMessageID)
	if err != nil {
		return err
	}
	legacyIntake := trip.State == models.Collecting && isIntakePoll(trip, vote.PollMessageID)
	if !legacyIntake && (poll == nil || strAny(poll.Data["session_id"]) != trip.SessionID) {
		return nil
	}
	if poll != nil && strAny(poll.Data["kind"]) != trip.LastPoll {
		return nil
	}
	if poll != nil && strAny(poll.Data["plan_key"]) != pollPlanKey(trip) {
		return nil
	}
	var allowed []string
	if poll != nil {
		if err := decodeInto(poll.Data["options"], &allowed); err != nil {
			return err
		}
	} else if p := findIntakePoll(trip, vote.PollMessageID); p != nil {
		allowed = p.Options
	}
	for _, selected := range vote.SelectedOptions {
		valid := false
		for _, option := range allowed {
			if selected == option {
				valid = true
				break
			}
		}
		if !valid {
			return nil
		}
	}
	sentAt := models.Now()
	if vote.Timestamp != 0 {
		sentAt = time.Unix(vote.Timestamp, 0).UTC()
	}
	selection := strings.Join(vote.SelectedOptions, " | ")
	text := "Poll vote: " + vote.PollName + "\nSelected: " + selection
	isNew, err := b.Store.SaveMessage(ctx, &models.Message{
		GroupID: vote.GroupID, TripID: trip.ID, SessionID: trip.SessionID,
		ExternalID: fmt.Sprintf("poll-vote:%s:%s:%d:%s", vote.PollMessageID, vote.VoterID, vote.Timestamp, selection),
		SenderID:   vote.VoterID, SenderName: vote.VoterName, Text: text, Tagged: true, SentAt: sentAt,
	})
	if err != nil || !isNew {
		return err
	}
	if legacyIntake {
		return b.runIntakePollVote(ctx, trip, vote)
	}
	if len(vote.SelectedOptions) == 0 {
		return nil
	}
	selected := vote.SelectedOptions[0]
	switch strAny(poll.Data["kind"]) {
	case "payer":
		return b.applyPayerVote(ctx, trip, vote, selected)
	case "change":
		return b.handleChangeVote(ctx, trip, vote, selected)
	case "options":
		if trip.State != models.AwaitingChoice {
			return nil
		}
		for i, label := range allowed {
			if label == selected && i < len(trip.Options) {
				return b.selectOption(ctx, trip, trip.Options[i].Position)
			}
		}
		return nil
	case "finalize":
		if trip.State != models.AwaitingChoice {
			return nil
		}
		if selected == allowed[0] {
			return b.startTravelSearch(ctx, trip)
		}
		return b.say(ctx, trip.GroupID, "The group chose not to start a search. Ask what they would like to change in the current plan.", nil)
	}
	return b.handleChatTurn(ctx, trip, models.IncomingMessage{
		GroupID: vote.GroupID, GroupName: trip.GroupName, SenderID: vote.VoterID,
		SenderName: vote.VoterName, Text: text, Tagged: true, Timestamp: vote.Timestamp,
	}, sentAt)
}

func pollPlanKey(trip *models.Trip) string {
	data, _ := json.Marshal(map[string]any{
		"options": trip.Options, "chosen": trip.ChosenOption(), "change_summary": pendingChangeSummary(trip),
	})
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func pendingChangeSummary(trip *models.Trip) string {
	if trip.PendingChange == nil {
		return ""
	}
	return trip.PendingChange.Summary
}
