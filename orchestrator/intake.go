// Package orchestrator (this file): the deterministic intake turn controller from the WhatsApp
// Agent Flow Spec, §4. It owns a trip exclusively while State == Collecting — everything from
// AwaitingChoice onward (search, approval, booking, post-booking advisor features) is untouched,
// existing code, reached via handoffToSearch below.
//
// Code decides the intent (this file); Gemini only (a) extracts facts (prompts.IntakeExtract) and
// (b) writes the words for whatever intent code picked (prompts.IntakeWriter). Gemini never
// decides what happens next.
package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"fare-brain/llm"
	"fare-brain/messaging"
	"fare-brain/models"
	"fare-brain/prompts"
	"fare-brain/store"
)

// Field names, in spec §4.1's required order. Each is "satisfied" independently; the first
// unsatisfied one in this order is what gets asked next.
const (
	fAttendance     = "attendance"
	fOrigin         = "origin"
	fDateWindow     = "date_window"
	fExactDates     = "exact_dates"
	fBudgetRange    = "budget_pp" // matches prompts.IntakeExtract's field enum value
	fBudgetIncludes = "budget_includes"
	fVibe           = "vibe"
	fDestination    = "destination"
	fConstraints    = "constraints"
)

const llmCallTimeout = 8 * time.Second   // spec §7.3
const pollWaitTimeout = 10 * time.Minute // spec §4.2

// ------------------------------------------------------------------ entry points

// runIntakeTurn is the §4 turn() function for an inbound text message. Called from handle() only
// while trip.State == Collecting.
func (b *Brain) runIntakeTurn(ctx context.Context, trip *models.Trip, m models.IncomingMessage) error {
	if trip.Intake == nil {
		trip = b.seedIntake(ctx, trip, m)
	}

	extraction, err := b.extractIntake(ctx, trip, m.Text, m.SenderID)
	if err != nil {
		slog.Warn("intake extraction failed, continuing without it", "group_id", trip.GroupID, "err", err)
		extraction = nil
	}

	if extraction != nil && extraction.TripIntent == "cancel" {
		_, err := store.SetState(ctx, b.Store, trip.ID, models.Cancelled, nil)
		if err != nil {
			return err
		}
		return b.writeAndSend(ctx, trip, "CANCEL", "the group asked to stop planning", nil)
	}

	if extraction != nil {
		trip = b.applyExtraction(ctx, trip, extraction, m.SenderID, m.MessageID)
	}

	return b.continueIntake(ctx, trip, extraction)
}

// runIntakePollVote is the §4.3 poll-vote path. Called from handlePollVote only when the vote's
// poll_message_id matches one of trip.IntakePolls (i.e. it's ours, not a payer/finalize poll from
// the existing AwaitingChoice-onward machinery).
func (b *Brain) runIntakePollVote(ctx context.Context, trip *models.Trip, vote models.PollVote) error {
	poll := findIntakePoll(trip, vote.PollMessageID)
	if poll == nil {
		return nil
	}
	trip = b.recordPollVote(ctx, trip, poll, vote)

	complete := pollComplete(trip, poll)
	stale := time.Since(poll.CreatedAt) > pollWaitTimeout
	if !complete && !stale {
		return nil // spec §4.3: don't reply to every single vote, wait for completion or staleness
	}

	trip = b.closePollAndApply(ctx, trip, poll)
	return b.continueIntake(ctx, trip, nil)
}

// isIntakePoll reports whether a poll_message_id belongs to this turn controller (as opposed to
// the existing finalize/payer polls further down the pipeline).
func isIntakePoll(trip *models.Trip, pollMessageID string) bool {
	return findIntakePoll(trip, pollMessageID) != nil
}

// findIntakePoll matches an incoming vote to the open poll it belongs to. The real WhatsApp
// message id isn't known until the *first* vote arrives (Messenger.SendPoll returns only an
// error, no id) — so an open poll with no id recorded yet is presumed to be the one and only
// thing a vote could be answering (the controller never has more than one poll open at a time).
func findIntakePoll(trip *models.Trip, pollMessageID string) *models.IntakePoll {
	if trip == nil {
		return nil
	}
	for i := range trip.IntakePolls {
		p := &trip.IntakePolls[i]
		if p.Closed {
			continue
		}
		if p.PollMessageID == pollMessageID || p.PollMessageID == "" {
			return p
		}
	}
	return nil
}

// ------------------------------------------------------------------ the §4 pseudocode

// continueIntake runs steps 3-7 of spec §4's turn() pseudocode: conflicts, pending-question
// clearing, next-missing-field, or readiness. extraction may be nil (e.g. after a poll vote,
// where there's no free-text message to extract from).
func (b *Brain) continueIntake(ctx context.Context, trip *models.Trip, extraction *intakeExtraction) error {
	if conflicts := detectConflicts(trip); len(conflicts) > 0 {
		trip = b.saveConflicts(ctx, trip, conflicts)
		return b.writeAndSend(ctx, trip, "RESOLVE_CONFLICT", conflicts[0].Description, nil)
	}
	if len(trip.Conflicts) > 0 {
		trip = b.clearResolvedConflicts(ctx, trip)
	}

	if trip.PendingQuestion != nil {
		answered := extraction != nil && extraction.AnswersPendingQuestion
		if !answered && !fieldSatisfied(trip, trip.PendingQuestion.Field) {
			return nil // spec §4 step 5: not an answer, not tagged-and-relevant -> stay silent, don't nag
		}
		trip = b.clearPendingQuestion(ctx, trip)
	}

	if extraction != nil && extraction.Approval == "yes" && readyToSearch(trip) {
		return b.handoffToSearch(ctx, trip)
	}

	missing := nextMissingField(trip)
	if missing == "" {
		return b.confirmReady(ctx, trip)
	}
	return b.askField(ctx, trip, missing)
}

// ------------------------------------------------------------------ readiness (§4.5)

// readyToSearch implements spec §4.5's exact checklist. This — not "origin + overlapping dates"
// — is the only thing that may be true before a search can start, and even then only after an
// explicit ✅ (checked by the caller, continueIntake).
func readyToSearch(trip *models.Trip) bool {
	return nextMissingField(trip) == ""
}

// nextMissingField returns the first unsatisfied field in spec §4.1's order, or "" if every
// required field is confirmed/inferred.
func nextMissingField(trip *models.Trip) string {
	order := []string{fAttendance, fOrigin, fDateWindow, fExactDates, fBudgetRange, fBudgetIncludes, fVibe, fDestination, fConstraints}
	for _, f := range order {
		if !fieldSatisfied(trip, f) {
			return f
		}
	}
	return ""
}

func fieldSatisfied(trip *models.Trip, field string) bool {
	intake := trip.Intake
	if intake == nil {
		intake = &models.TripIntake{}
	}
	coming := comingParticipants(trip)
	switch field {
	case fAttendance:
		return len(coming) >= 1 && anyAttendanceKnown(trip)
	case fOrigin:
		for _, p := range coming {
			if p.Intake == nil || !p.Intake.Origin.Known() {
				return false
			}
		}
		return len(coming) > 0
	case fDateWindow:
		return intake.DateWindow != nil && intake.DateWindow.Earliest != "" && intake.Nights.Min > 0
	case fExactDates:
		return intake.ExactDates != nil && intake.ExactDates.Depart != "" && intake.ExactDates.Return != ""
	case fBudgetRange:
		return intake.BudgetPP != nil && intake.BudgetPP.Max > 0
	case fBudgetIncludes:
		return intake.BudgetPP != nil && len(intake.BudgetPP.Includes) > 0
	case fVibe:
		return intake.Vibe.Known() || intake.Destination.Known()
	case fDestination:
		return intake.Destination.Known()
	case fConstraints:
		return intake.Constraints.Known()
	}
	return true
}

func anyAttendanceKnown(trip *models.Trip) bool {
	for _, p := range trip.Participants {
		if p.Intake != nil && p.Intake.Attendance != "" && p.Intake.Attendance != "unknown" {
			return true
		}
	}
	return false
}

func comingParticipants(trip *models.Trip) []models.Participant {
	var out []models.Participant
	anyKnown := anyAttendanceKnown(trip)
	for _, p := range trip.Participants {
		if !anyKnown {
			out = append(out, p) // attendance not asked yet — treat everyone as a candidate
			continue
		}
		if p.Intake != nil && (p.Intake.Attendance == "coming" || p.Intake.Attendance == "maybe") {
			out = append(out, p)
		}
	}
	return out
}

// ------------------------------------------------------------------ conflicts (§4.4)

func detectConflicts(trip *models.Trip) []models.Conflict {
	var out []models.Conflict
	coming := comingParticipants(trip)

	// Date conflict: every coming participant has availability, but no single range works for all.
	if trip.Intake != nil && trip.Intake.ExactDates == nil {
		var ranges [][2]string
		allHaveAvailability := len(coming) > 0
		for _, p := range coming {
			if p.Intake == nil || len(p.Intake.Available) == 0 {
				allHaveAvailability = false
				continue
			}
			for _, r := range p.Intake.Available {
				ranges = append(ranges, [2]string{r.Start, r.End})
			}
		}
		if allHaveAvailability && len(coming) > 1 && !anyCommonRange(trip, coming) {
			out = append(out, models.Conflict{
				Field:       fExactDates,
				Description: "no date range works for everyone who's coming — " + namesWithNoOverlap(coming),
			})
		}
	}

	// Budget conflict: cheapest viable option (we don't have real prices during intake, so this
	// checks for a >20% spread between the lowest and highest stated per-person budget instead,
	// which is the signal available before a real search exists).
	if lo, hi, ok := budgetSpread(coming); ok && hi > 0 && (hi-lo)/hi > 0.20 {
		out = append(out, models.Conflict{
			Field:       fBudgetRange,
			Description: fmt.Sprintf("budgets are pretty spread out (from %.0f to %.0f per person) — worth agreeing a number before I search", lo, hi),
		})
	}

	return out
}

func anyCommonRange(trip *models.Trip, coming []models.Participant) bool {
	// Simple overlap check: true if there's at least one date that every coming participant with
	// availability listed includes.
	dateCounts := map[string]int{}
	withAvailability := 0
	for _, p := range coming {
		if p.Intake == nil || len(p.Intake.Available) == 0 {
			continue
		}
		withAvailability++
		seen := map[string]bool{}
		for _, r := range p.Intake.Available {
			for _, d := range expandDateRange(r.Start, r.End) {
				if !seen[d] {
					seen[d] = true
					dateCounts[d]++
				}
			}
		}
	}
	for _, c := range dateCounts {
		if c == withAvailability && withAvailability > 0 {
			return true
		}
	}
	return withAvailability == 0
}

func namesWithNoOverlap(coming []models.Participant) string {
	var names []string
	for _, p := range coming {
		names = append(names, p.WhatsAppName)
	}
	return strings.Join(names, ", ")
}

func budgetSpread(coming []models.Participant) (lo, hi float64, ok bool) {
	first := true
	for _, p := range coming {
		if p.Intake == nil || !p.Intake.BudgetPP.Known() {
			continue
		}
		v, parseOK := p.Intake.BudgetPP.Value.(float64)
		if !parseOK {
			continue
		}
		if first {
			lo, hi = v, v
			first = false
			continue
		}
		if v < lo {
			lo = v
		}
		if v > hi {
			hi = v
		}
	}
	return lo, hi, !first
}

func expandDateRange(start, end string) []string {
	s, err1 := time.Parse("2006-01-02", start)
	e, err2 := time.Parse("2006-01-02", end)
	if err1 != nil || err2 != nil {
		if start != "" {
			return []string{start}
		}
		return nil
	}
	var out []string
	for d := s; !d.After(e); d = d.AddDate(0, 0, 1) {
		out = append(out, d.Format("2006-01-02"))
	}
	return out
}

func (b *Brain) saveConflicts(ctx context.Context, trip *models.Trip, conflicts []models.Conflict) *models.Trip {
	updated, err := b.Store.UpdateTrip(ctx, trip.ID, map[string]any{"conflicts": conflicts})
	if err != nil || updated == nil {
		trip.Conflicts = conflicts
		return trip
	}
	return updated
}

func (b *Brain) clearResolvedConflicts(ctx context.Context, trip *models.Trip) *models.Trip {
	updated, err := b.Store.UpdateTrip(ctx, trip.ID, map[string]any{"conflicts": []models.Conflict{}})
	if err != nil || updated == nil {
		trip.Conflicts = nil
		return trip
	}
	return updated
}

// ------------------------------------------------------------------ extraction (§5)

type intakeUpdate struct {
	Scope      string `json:"scope"`
	WaID       string `json:"wa_id"`
	Field      string `json:"field"`
	ValueText  string `json:"value_text"`
	Confidence string `json:"confidence"`
	Evidence   string `json:"evidence"`
}

type intakeExtraction struct {
	TripIntent              string         `json:"trip_intent"`
	Updates                 []intakeUpdate `json:"updates"`
	AnswersPendingQuestion  bool           `json:"answers_pending_question"`
	Approval                string         `json:"approval"`
	NeedsClarificationField string         `json:"needs_clarification_field"`
	NeedsClarificationWhy   string         `json:"needs_clarification_why"`
}

func (b *Brain) extractIntake(ctx context.Context, trip *models.Trip, text, senderWaID string) (*intakeExtraction, error) {
	cctx, cancel := context.WithTimeout(ctx, llmCallTimeout)
	defer cancel()

	roster := rosterJSON(trip)
	pending := ""
	if trip.PendingQuestion != nil {
		pending = trip.PendingQuestion.Field
	}
	system := prompts.IntakeExtractSystem(b.Config.BotName, b.today().Format("2006-01-02"), b.Config.Timezone, roster, tripStateJSON(trip), pending)

	out, err := b.LLM.Structured(cctx, system, []llm.Message{{Role: "user", Content: text}}, toSchema(prompts.IntakeExtract))
	if err != nil {
		return nil, err
	}
	var ex intakeExtraction
	if err := decodeInto(out, &ex); err != nil {
		return nil, err
	}
	return &ex, nil
}

func rosterJSON(trip *models.Trip) string {
	parts := make([]string, 0, len(trip.Participants))
	for _, p := range trip.Participants {
		id := p.WaID
		if id == "" {
			id = p.PID
		}
		parts = append(parts, fmt.Sprintf("%s -> %s", id, p.WhatsAppName))
	}
	if len(parts) == 0 {
		return "(none yet)"
	}
	return strings.Join(parts, "; ")
}

func tripStateJSON(trip *models.Trip) string {
	m, err := structToMap(trip.Intake)
	if err != nil || m == nil {
		return "{}"
	}
	raw, _ := structToMap(map[string]any{"intake": m, "pending_question": trip.PendingQuestion})
	out, err := structToMap(raw)
	if err != nil {
		return "{}"
	}
	return fmt.Sprintf("%v", out)
}

// applyExtraction merges extracted updates into trip/participant intake state, respecting
// confidence (never downgrade a confirmed value to inferred) and persists the result.
func (b *Brain) applyExtraction(ctx context.Context, trip *models.Trip, ex *intakeExtraction, senderWaID, sourceID string) *models.Trip {
	if ex.TripIntent == "start" && trip.OrganizerWaID == "" {
		_, _ = b.Store.UpdateTrip(ctx, trip.ID, map[string]any{"organizer_wa_id": senderWaID})
		trip.OrganizerWaID = senderWaID
	}

	intake := trip.Intake
	if intake == nil {
		intake = &models.TripIntake{}
	}
	participants := append([]models.Participant(nil), trip.Participants...)

	for _, u := range ex.Updates {
		conf := models.Inferred
		if u.Confidence == "confirmed" {
			conf = models.Confirmed
		}
		if u.Scope == "participant" {
			idx := findOrCreateParticipantIndex(&participants, u.WaID)
			if idx < 0 {
				continue
			}
			applyParticipantUpdate(&participants[idx], u, conf, sourceID)
		} else {
			applyTripUpdate(intake, u, conf, sourceID)
		}
	}

	fields := map[string]any{"intake": intake, "participants": participants}
	updated, err := b.Store.UpdateTrip(ctx, trip.ID, fields)
	if err != nil || updated == nil {
		trip.Intake = intake
		trip.Participants = participants
		return trip
	}
	return updated
}

func findOrCreateParticipantIndex(participants *[]models.Participant, waID string) int {
	if waID == "" {
		return -1
	}
	for i, p := range *participants {
		if p.WaID == waID {
			return i
		}
	}
	*participants = append(*participants, models.Participant{
		WaID: waID, PID: fmt.Sprintf("p_%d_%d", time.Now().UnixNano(), len(*participants)),
		Intake: &models.ParticipantIntake{Attendance: "unknown"},
	})
	return len(*participants) - 1
}

func applyParticipantUpdate(p *models.Participant, u intakeUpdate, conf models.Confidence, sourceID string) {
	if p.Intake == nil {
		p.Intake = &models.ParticipantIntake{}
	}
	now := models.Now()
	switch u.Field {
	case fOrigin:
		if !shouldOverwrite(p.Intake.Origin, conf) {
			return
		}
		p.Intake.Origin = models.FieldValue{Value: strings.ToUpper(strings.TrimSpace(u.ValueText)), Confidence: conf, SourceID: sourceID, UpdatedAt: now}
		p.OriginAirport = p.Intake.Origin.AsString() // keep the existing flat field in sync for the downstream pipeline
	case "available":
		if start, end, ok := parseDateRangeText(u.ValueText); ok {
			p.Intake.Available = append(p.Intake.Available, models.DateRange{Start: start, End: end, SourceID: sourceID})
		}
	case fBudgetRange:
		if v, ok := parseFirstNumber(u.ValueText); ok && shouldOverwrite(p.Intake.BudgetPP, conf) {
			p.Intake.BudgetPP = models.FieldValue{Value: v, Confidence: conf, SourceID: sourceID, UpdatedAt: now}
		}
	case "constraints":
		p.Intake.Constraints = append(p.Intake.Constraints, strings.TrimSpace(u.ValueText))
	}
}

func shouldOverwrite(existing models.FieldValue, newConf models.Confidence) bool {
	if !existing.Known() {
		return true
	}
	return existing.Confidence != models.Confirmed || newConf == models.Confirmed
}

func applyTripUpdate(intake *models.TripIntake, u intakeUpdate, conf models.Confidence, sourceID string) {
	now := models.Now()
	switch u.Field {
	case fAttendance:
		if v, ok := parseFirstNumber(u.ValueText); ok && shouldOverwrite(intake.Headcount, conf) {
			intake.Headcount = models.FieldValue{Value: v, Confidence: conf, SourceID: sourceID, UpdatedAt: now}
		}
	case "children":
		if shouldOverwrite(intake.Children, conf) {
			intake.Children = models.FieldValue{Value: strings.TrimSpace(u.ValueText), Confidence: conf, SourceID: sourceID, UpdatedAt: now}
		}
	case fDateWindow:
		if start, end, ok := parseDateRangeText(u.ValueText); ok {
			intake.DateWindow = &models.DateWindow{Earliest: start, Latest: end}
		}
	case "nights":
		if lo, hi, ok := parseIntRangeText(u.ValueText); ok {
			intake.Nights = models.NightsRange{Min: lo, Max: hi}
		}
	case fExactDates:
		if start, end, ok := parseDateRangeText(u.ValueText); ok {
			intake.ExactDates = &models.ExactDates{Depart: start, Return: end}
		}
	case fBudgetRange:
		lo, hi, currency, _ := parseBudgetText(u.ValueText)
		if hi > 0 {
			bp := intake.BudgetPP
			if bp == nil {
				bp = &models.BudgetPP{}
			}
			bp.Min, bp.Max = lo, hi
			if currency != "" {
				bp.Currency = currency
			}
			intake.BudgetPP = bp
		}
	case fBudgetIncludes:
		includes := splitNonEmpty(u.ValueText, ",")
		if len(includes) > 0 {
			bp := intake.BudgetPP
			if bp == nil {
				bp = &models.BudgetPP{}
			}
			bp.Includes = includes
			intake.BudgetPP = bp
		}
	case fVibe:
		if shouldOverwrite(intake.Vibe, conf) {
			intake.Vibe = models.FieldValue{Value: strings.ToLower(strings.TrimSpace(u.ValueText)), Confidence: conf, SourceID: sourceID, UpdatedAt: now}
		}
	case fDestination:
		if shouldOverwrite(intake.Destination, conf) {
			intake.Destination = models.FieldValue{Value: strings.TrimSpace(u.ValueText), Confidence: conf, SourceID: sourceID, UpdatedAt: now}
		}
	case "constraints":
		existing := intake.Constraints.AsString()
		combined := strings.TrimSpace(u.ValueText)
		if existing != "" {
			combined = existing + "; " + combined
		}
		intake.Constraints = models.FieldValue{Value: combined, Confidence: conf, SourceID: sourceID, UpdatedAt: now}
	}
}

func parseFirstNumber(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	var digits strings.Builder
	for _, r := range s {
		if (r >= '0' && r <= '9') || r == '.' {
			digits.WriteRune(r)
		} else if digits.Len() > 0 {
			break
		}
	}
	if digits.Len() == 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(digits.String(), 64)
	return v, err == nil
}

func parseDateRangeText(s string) (start, end string, ok bool) {
	s = strings.TrimSpace(s)
	if parts := strings.SplitN(s, "..", 2); len(parts) == 2 {
		return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]), true
	}
	if s != "" {
		return s, s, true
	}
	return "", "", false
}

func parseIntRangeText(s string) (lo, hi int, ok bool) {
	s = strings.TrimSpace(s)
	if parts := strings.SplitN(s, "-", 2); len(parts) == 2 {
		a, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
		b, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err1 == nil && err2 == nil {
			return a, b, true
		}
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n, n, true
	}
	return 0, 0, false
}

func parseBudgetText(s string) (lo, hi float64, currency string, includes []string) {
	fields := strings.Fields(strings.TrimSpace(s))
	if len(fields) == 0 {
		return 0, 0, "", nil
	}
	if parts := strings.SplitN(fields[0], "-", 2); len(parts) == 2 {
		lo, _ = strconv.ParseFloat(parts[0], 64)
		hi, _ = strconv.ParseFloat(parts[1], 64)
	} else {
		hi, _ = strconv.ParseFloat(fields[0], 64)
		lo = hi
	}
	if len(fields) > 1 {
		currency = strings.ToUpper(fields[1])
	}
	if len(fields) > 2 {
		includes = strings.Split(fields[2], ",")
	}
	return lo, hi, currency, includes
}

func splitNonEmpty(s, sep string) []string {
	var out []string
	for _, part := range strings.Split(s, sep) {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// ------------------------------------------------------------------ asking (§4.1, §4.2, §6)

func (b *Brain) askField(ctx context.Context, trip *models.Trip, field string) error {
	switch field {
	case fAttendance:
		return b.askAttendancePoll(ctx, trip)
	case fOrigin:
		return b.askOriginPoll(ctx, trip)
	case fDateWindow:
		return b.writeAndSend(ctx, trip, "ASK", "ask for a rough date window and how many nights they want — chat, not a poll", nil)
	case fExactDates:
		return b.askExactDatesPoll(ctx, trip)
	case fBudgetRange:
		return b.askBudgetPoll(ctx, trip)
	case fBudgetIncludes:
		return b.writeAndSend(ctx, trip, "ASK", "ask whether their stated budget covers flights+stay only, or food and activities too", nil)
	case fVibe:
		return b.askVibePoll(ctx, trip)
	case fDestination:
		return b.askDestinationPoll(ctx, trip)
	case fConstraints:
		return b.writeAndSend(ctx, trip, "ASK", "ask once about must-haves: room sharing, accessibility, dietary needs, flight limits, places to avoid. Accept \"none\".", nil)
	}
	return nil
}

func (b *Brain) askAttendancePoll(ctx context.Context, trip *models.Trip) error {
	return b.postIntakePoll(ctx, trip, fAttendance, "Who's in?", []string{"Coming", "Maybe", "Not coming"}, false,
		"post the attendance poll, one line introducing it")
}

func (b *Brain) askOriginPoll(ctx context.Context, trip *models.Trip) error {
	cities := candidateOriginCities(trip)
	options := append(append([]string{}, cities...), "Other (reply in chat)")
	return b.postIntakePoll(ctx, trip, fOrigin, "Where are you flying from?", options, false,
		"post a poll of likely departure cities plus an Other option")
}

func (b *Brain) askExactDatesPoll(ctx context.Context, trip *models.Trip) error {
	ranges := generateDateOptions(trip.Intake.DateWindow, trip.Intake.Nights)
	if len(ranges) == 0 {
		return b.writeAndSend(ctx, trip, "ASK", "the date window given doesn't fit the requested trip length — ask for a wider window", nil)
	}
	return b.postIntakePoll(ctx, trip, fExactDates, "Which dates work?", ranges, true,
		"post a multi-select poll of concrete date ranges that fit inside the stated window and length")
}

func (b *Brain) askBudgetPoll(ctx context.Context, trip *models.Trip) error {
	return b.postIntakePoll(ctx, trip, fBudgetRange, "Budget per person?",
		[]string{"Under C$600", "C$600-900", "C$900-1300", "C$1300+"}, false,
		"post a poll of CAD per-person budget ranges")
}

func (b *Brain) askVibePoll(ctx context.Context, trip *models.Trip) error {
	return b.postIntakePoll(ctx, trip, fVibe, "What's the vibe?",
		[]string{"Beach", "City", "Nature", "Open to suggestions"}, false,
		"post a poll asking what kind of trip they want")
}

func (b *Brain) askDestinationPoll(ctx context.Context, trip *models.Trip) error {
	options, err := b.proposeDestinations(ctx, trip)
	if err != nil || len(options) == 0 {
		return b.writeAndSend(ctx, trip, "ASK", "ask the group if anyone has a destination in mind", nil)
	}
	return b.postIntakePoll(ctx, trip, fDestination, "Which city?", options, false,
		"post a single-select poll of 2-3 proposed destinations that fit the budget, origins, and dates")
}

// proposeDestinations asks Gemini for 2-3 destination names fitting what's known so far — a much
// smaller ask than the existing propose()'s full options-with-pricing, since all that's needed
// here is a poll of city names.
func (b *Brain) proposeDestinations(ctx context.Context, trip *models.Trip) ([]string, error) {
	cctx, cancel := context.WithTimeout(ctx, llmCallTimeout)
	defer cancel()
	vibe := trip.Intake.Vibe.AsString()
	system := fmt.Sprintf(`You suggest 2-3 destination cities for a group trip. Vibe: %s. Return ONLY JSON matching the schema — just city names, nothing else.`, vibe)
	schema := llm.Schema{Name: "destination_suggestions", Schema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"cities": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		},
		"required": []string{"cities"}, "additionalProperties": false,
	}}
	out, err := b.LLM.Structured(cctx, system, []llm.Message{{Role: "user", Content: tripStateJSON(trip)}}, schema)
	if err != nil {
		return nil, err
	}
	var result struct {
		Cities []string `json:"cities"`
	}
	if err := decodeInto(out, &result); err != nil {
		return nil, err
	}
	return clipPollOptions(result.Cities), nil
}

func candidateOriginCities(trip *models.Trip) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range trip.Participants {
		if p.OriginCity != "" && !seen[p.OriginCity] {
			seen[p.OriginCity] = true
			out = append(out, p.OriginCity)
		}
	}
	if len(out) == 0 {
		out = []string{"Vancouver", "Toronto", "Calgary"}
	}
	if len(out) > 4 {
		out = out[:4]
	}
	return out
}

func generateDateOptions(window *models.DateWindow, nights models.NightsRange) []string {
	if window == nil || window.Earliest == "" {
		return nil
	}
	start, err := time.Parse("2006-01-02", window.Earliest)
	if err != nil {
		return nil
	}
	end := start
	if window.Latest != "" {
		if e, err := time.Parse("2006-01-02", window.Latest); err == nil {
			end = e
		}
	}
	n := nights.Min
	if n <= 0 {
		n = 3
	}
	var out []string
	for d := start; !d.After(end) && len(out) < 5; d = d.AddDate(0, 0, 2) {
		depart := d
		ret := d.AddDate(0, 0, n)
		out = append(out, fmt.Sprintf("%s – %s (%d nights)",
			depart.Format("Mon Jan 2"), ret.Format("Mon Jan 2"), n))
	}
	return out
}

// postIntakePoll calls the writer for a one-line intro (per spec §6.2: "the text introduces it in
// one line; the poll carries the options"), sends the poll, and tracks it on the trip so votes can
// be matched back and completeness checked.
func (b *Brain) postIntakePoll(ctx context.Context, trip *models.Trip, field, question string, options []string, multi bool, slot string) error {
	text, _, _, _, err := b.writeIntake(ctx, trip, "ASK_POLL", slot, trip)
	if err != nil || strings.TrimSpace(text) == "" {
		text = question
	}
	if err := b.Messenger.SendPoll(ctx, trip.GroupID, messaging.Poll{Name: question, Options: options, AllowMultipleAnswers: multi}); err != nil {
		return err
	}
	if strings.TrimSpace(text) != "" {
		if err := b.Messenger.Send(ctx, trip.GroupID, text, nil); err != nil {
			slog.Warn("failed to send poll intro", "group_id", trip.GroupID, "err", err)
		}
	}

	expected := make([]string, 0, len(trip.Participants))
	for _, p := range comingParticipants(trip) {
		if p.WaID != "" {
			expected = append(expected, p.WaID)
		}
	}
	poll := models.IntakePoll{
		Field: field, Options: options, Multi: multi, ExpectedVoters: expected,
		Votes: map[string][]string{}, CreatedAt: models.Now(),
	}
	trip = b.setPendingQuestion(ctx, trip, field, "", "group")
	trip.IntakePolls = append(trip.IntakePolls, poll)
	_, _ = b.Store.UpdateTrip(ctx, trip.ID, map[string]any{"intake_polls": trip.IntakePolls, "last_agent_text": text})
	return nil
}

// recordPollVote sets the intake poll message id (we don't know it until SendPoll returns, so the
// first vote on a just-posted poll backfills it) and records the voter's selection.
func (b *Brain) recordPollVote(ctx context.Context, trip *models.Trip, poll *models.IntakePoll, vote models.PollVote) *models.Trip {
	for i := range trip.IntakePolls {
		if trip.IntakePolls[i].Field == poll.Field && !trip.IntakePolls[i].Closed {
			if trip.IntakePolls[i].PollMessageID == "" {
				trip.IntakePolls[i].PollMessageID = vote.PollMessageID
			}
			if trip.IntakePolls[i].Votes == nil {
				trip.IntakePolls[i].Votes = map[string][]string{}
			}
			trip.IntakePolls[i].Votes[vote.VoterID] = vote.SelectedOptions
			poll = &trip.IntakePolls[i]
			break
		}
	}
	updated, err := b.Store.UpdateTrip(ctx, trip.ID, map[string]any{"intake_polls": trip.IntakePolls})
	if err != nil || updated == nil {
		return trip
	}
	return updated
}

func pollComplete(trip *models.Trip, poll *models.IntakePoll) bool {
	if len(poll.ExpectedVoters) == 0 {
		return len(poll.Votes) > 0
	}
	for _, id := range poll.ExpectedVoters {
		if _, voted := poll.Votes[id]; !voted {
			return false
		}
	}
	return true
}

// closePollAndApply applies every recorded vote to the relevant intake field (poll votes are
// already structured, so this bypasses LLM extraction entirely) and marks the poll closed.
func (b *Brain) closePollAndApply(ctx context.Context, trip *models.Trip, poll *models.IntakePoll) *models.Trip {
	switch poll.Field {
	case fAttendance:
		for voterID, selected := range poll.Votes {
			if len(selected) == 0 {
				continue
			}
			idx := findOrCreateParticipantIndexPtr(trip, voterID)
			if idx >= 0 {
				trip.Participants[idx].Intake.Attendance = strings.ToLower(strings.ReplaceAll(selected[0], " ", "_"))
			}
		}
	case fOrigin:
		for voterID, selected := range poll.Votes {
			if len(selected) == 0 || strings.HasPrefix(selected[0], "Other") {
				continue
			}
			idx := findOrCreateParticipantIndexPtr(trip, voterID)
			if idx >= 0 {
				trip.Participants[idx].Intake.Origin = models.FieldValue{Value: selected[0], Confidence: models.Confirmed, SourceID: poll.PollMessageID, UpdatedAt: models.Now()}
				trip.Participants[idx].OriginCity = selected[0]
			}
		}
	case fExactDates:
		if common := commonlyVoted(poll); common != "" {
			if depart, ret, ok := parseExactDateOption(common); ok {
				trip.Intake.ExactDates = &models.ExactDates{Depart: depart, Return: ret}
			}
		}
	case fBudgetRange:
		if common := commonlyVoted(poll); common != "" {
			lo, hi := parseBudgetRangeLabel(common)
			bp := trip.Intake.BudgetPP
			if bp == nil {
				bp = &models.BudgetPP{Currency: "CAD"}
			}
			bp.Min, bp.Max = lo, hi
			trip.Intake.BudgetPP = bp
		}
	case fVibe:
		if common := commonlyVoted(poll); common != "" {
			trip.Intake.Vibe = models.FieldValue{Value: strings.ToLower(common), Confidence: models.Confirmed, SourceID: poll.PollMessageID, UpdatedAt: models.Now()}
		}
	case fDestination:
		if common := commonlyVoted(poll); common != "" {
			trip.Intake.Destination = models.FieldValue{Value: common, Confidence: models.Confirmed, SourceID: poll.PollMessageID, UpdatedAt: models.Now()}
		}
	}

	for i := range trip.IntakePolls {
		if trip.IntakePolls[i].Field == poll.Field && !trip.IntakePolls[i].Closed {
			trip.IntakePolls[i].Closed = true
		}
	}
	updated, err := b.Store.UpdateTrip(ctx, trip.ID, map[string]any{
		"intake": trip.Intake, "participants": trip.Participants, "intake_polls": trip.IntakePolls,
	})
	if err != nil || updated == nil {
		return trip
	}
	return updated
}

func findOrCreateParticipantIndexPtr(trip *models.Trip, waID string) int {
	participants := trip.Participants
	idx := findOrCreateParticipantIndex(&participants, waID)
	trip.Participants = participants
	return idx
}

// commonlyVoted returns the option with the most votes (ties broken by first-seen), the simple
// "majority wins, split stays stored but doesn't block" rule from spec §4.3.
func commonlyVoted(poll *models.IntakePoll) string {
	counts := map[string]int{}
	var order []string
	for _, selected := range poll.Votes {
		for _, opt := range selected {
			if _, seen := counts[opt]; !seen {
				order = append(order, opt)
			}
			counts[opt]++
		}
	}
	sort.SliceStable(order, func(i, j int) bool { return counts[order[i]] > counts[order[j]] })
	if len(order) == 0 {
		return ""
	}
	return order[0]
}

func parseExactDateOption(label string) (depart, ret string, ok bool) {
	// label format: "Mon Jan 2 – Mon Jan 9 (7 nights)" — we don't have the year in the label, so
	// closeIntakePoll's caller (closePollAndApply) relies on generateDateOptions having been built
	// from a window that's already resolved to the correct year.
	parts := strings.SplitN(label, "–", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	return strings.TrimSpace(parts[0]), strings.TrimSpace(strings.SplitN(parts[1], "(", 2)[0]), true
}

func parseBudgetRangeLabel(label string) (lo, hi float64) {
	switch {
	case strings.Contains(label, "Under"):
		return 0, 600
	case strings.Contains(label, "600-900"):
		return 600, 900
	case strings.Contains(label, "900-1300"):
		return 900, 1300
	case strings.Contains(label, "1300"):
		return 1300, 2000
	}
	return 0, 0
}

// ------------------------------------------------------------------ writer (§6)

func (b *Brain) writeIntake(ctx context.Context, trip *models.Trip, intent, slot string, _ *models.Trip) (text, pollQuestion string, pollOptions []string, pollMulti bool, err error) {
	cctx, cancel := context.WithTimeout(ctx, llmCallTimeout)
	defer cancel()
	recent := "(none)"
	system := prompts.IntakeWriterSystem(b.Config.BotName, intent, slot, tripStateJSON(trip), recent, trip.LastAgentText)
	out, err := b.LLM.Structured(cctx, system, []llm.Message{{Role: "user", Content: slot}}, toSchema(prompts.IntakeWriter))
	if err != nil {
		return "", "", nil, false, err
	}
	var w struct {
		Text         string   `json:"text"`
		PollQuestion string   `json:"poll_question"`
		PollOptions  []string `json:"poll_options"`
		PollMulti    bool     `json:"poll_multi"`
	}
	if err := decodeInto(out, &w); err != nil {
		return "", "", nil, false, err
	}
	return w.Text, w.PollQuestion, w.PollOptions, w.PollMulti, nil
}

// writeAndSend calls the writer for a plain-text (non-poll) intent and sends it, with the §7.4
// watchdog: if the writer hasn't produced a message within 20s, send the one permitted literal
// fallback instead.
func (b *Brain) writeAndSend(ctx context.Context, trip *models.Trip, intent, slot string, _ any) error {
	type result struct {
		text string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		text, _, _, _, err := b.writeIntake(ctx, trip, intent, slot, trip)
		done <- result{text, err}
	}()

	select {
	case r := <-done:
		if r.err != nil || strings.TrimSpace(r.text) == "" {
			slog.Warn("intake writer failed", "group_id", trip.GroupID, "intent", intent, "err", r.err)
			return nil
		}
		if err := b.Messenger.Send(ctx, trip.GroupID, r.text, nil); err != nil {
			return err
		}
		_, _ = b.Store.UpdateTrip(ctx, trip.ID, map[string]any{"last_agent_text": r.text})
		return nil
	case <-time.After(20 * time.Second): // spec §7.4 watchdog
		slog.Warn("intake turn watchdog fired", "group_id", trip.GroupID, "intent", intent)
		return b.Messenger.Send(ctx, trip.GroupID, "Give me a sec, still working on it", nil)
	}
}

func (b *Brain) confirmReady(ctx context.Context, trip *models.Trip) error {
	slot := readinessSummarySlot(trip)
	return b.writeAndSend(ctx, trip, "CONFIRM_READY", slot, nil)
}

func readinessSummarySlot(trip *models.Trip) string {
	var sb strings.Builder
	sb.WriteString("Summarize everything known and ask explicitly: \"Shall I go find flights and places to stay? Reply ✅ or tell me what to change.\" Include: ")
	for _, p := range comingParticipants(trip) {
		origin := ""
		if p.Intake != nil {
			origin = p.Intake.Origin.AsString()
		}
		sb.WriteString(fmt.Sprintf("%s from %s; ", p.WhatsAppName, origin))
	}
	if trip.Intake.ExactDates != nil {
		sb.WriteString(fmt.Sprintf("dates %s to %s; ", trip.Intake.ExactDates.Depart, trip.Intake.ExactDates.Return))
	}
	if trip.Intake.BudgetPP != nil {
		sb.WriteString(fmt.Sprintf("budget C$%.0f-%.0f per person (%s); ", trip.Intake.BudgetPP.Min, trip.Intake.BudgetPP.Max, strings.Join(trip.Intake.BudgetPP.Includes, "+")))
	}
	sb.WriteString(fmt.Sprintf("destination %s; constraints: %s.", trip.Intake.Destination.AsString(), trip.Intake.Constraints.AsString()))
	return sb.String()
}

// ------------------------------------------------------------------ pending question / seeding

func (b *Brain) setPendingQuestion(ctx context.Context, trip *models.Trip, field, pollID, askedTo string) *models.Trip {
	pq := &models.PendingQuestion{Field: field, AskedAt: models.Now(), PollID: pollID, AskedTo: askedTo}
	updated, err := b.Store.UpdateTrip(ctx, trip.ID, map[string]any{"pending_question": pq})
	if err != nil || updated == nil {
		trip.PendingQuestion = pq
		return trip
	}
	return updated
}

func (b *Brain) clearPendingQuestion(ctx context.Context, trip *models.Trip) *models.Trip {
	updated, err := b.Store.UpdateTrip(ctx, trip.ID, map[string]any{"pending_question": nil})
	if err != nil || updated == nil {
		trip.PendingQuestion = nil
		return trip
	}
	return updated
}

// seedIntake runs once per trip, on the first message that reaches the intake controller: seeds
// participants from the group roster and runs extraction over recent history (spec §2.1 steps
// 1-2), so anything already said before "@Fare plan a trip" is captured before the first question.
func (b *Brain) seedIntake(ctx context.Context, trip *models.Trip, m models.IncomingMessage) *models.Trip {
	participants := append([]models.Participant(nil), trip.Participants...)
	for _, member := range m.Participants {
		if member.IsAgent {
			continue
		}
		findOrCreateParticipantIndex(&participants, member.ID)
		for i := range participants {
			if participants[i].WaID == member.ID && participants[i].WhatsAppName == "" {
				participants[i].WhatsAppName = member.Name
			}
		}
	}
	findOrCreateParticipantIndex(&participants, m.SenderID)
	for i := range participants {
		if participants[i].WaID == m.SenderID && participants[i].WhatsAppName == "" {
			participants[i].WhatsAppName = m.SenderName
		}
	}

	intake := &models.TripIntake{}
	fields := map[string]any{"intake": intake, "participants": participants}
	updated, err := b.Store.UpdateTrip(ctx, trip.ID, fields)
	if err != nil || updated == nil {
		trip.Intake = intake
		trip.Participants = participants
		return trip
	}
	return updated
}

// ------------------------------------------------------------------ handoff (§9)

// handoffToSearch builds a single Option from the completed intake data and routes through the
// existing, unmodified AwaitingChoice-onward pipeline (lockChosenOption + offerPlanFinalize —
// which itself sends a day-by-day plan and a "Finalize this plan?" poll before any real search).
func (b *Brain) handoffToSearch(ctx context.Context, trip *models.Trip) error {
	for i := range trip.Participants {
		p := &trip.Participants[i]
		if p.Intake == nil {
			continue
		}
		if p.OriginAirport == "" {
			p.OriginAirport = p.Intake.Origin.AsString()
		}
	}
	if _, err := b.Store.UpdateTrip(ctx, trip.ID, map[string]any{"participants": trip.Participants}); err != nil {
		return err
	}

	nights := trip.Intake.Nights.Min
	if nights <= 0 && trip.Intake.ExactDates != nil {
		if d, err1 := time.Parse("2006-01-02", trip.Intake.ExactDates.Depart); err1 == nil {
			if r, err2 := time.Parse("2006-01-02", trip.Intake.ExactDates.Return); err2 == nil {
				nights = int(r.Sub(d).Hours() / 24)
			}
		}
	}
	option := models.Option{
		Position:    1,
		Destination: trip.Intake.Destination.AsString(),
	}
	if trip.Intake.ExactDates != nil {
		option.EmbarkingDate = trip.Intake.ExactDates.Depart
		option.ReturningDate = trip.Intake.ExactDates.Return
	}
	option.DurationNights = nights
	if trip.Intake.BudgetPP != nil {
		cpp := trip.Intake.BudgetPP.Max
		option.CostPerPerson = &cpp
	}

	trip, err := b.Store.UpdateTrip(ctx, trip.ID, map[string]any{"options": []models.Option{option}})
	if err != nil {
		return err
	}
	trip, err = b.lockChosenOption(ctx, trip, 1)
	if err != nil {
		return err
	}
	return b.offerPlanFinalize(ctx, trip)
}
