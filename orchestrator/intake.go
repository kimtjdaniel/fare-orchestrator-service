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
	"regexp"
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
	fChildren       = "children"
	fOrigin         = "origin"
	fDateWindow     = "date_window"
	fExactDates     = "exact_dates"
	fBudgetRange    = "budget_pp" // matches prompts.IntakeExtract's field enum value
	fBudgetIncludes = "budget_includes"
	fVibe           = "vibe"
	fDestination    = "destination"
	fConstraints    = "constraints"
	fPackage        = "package"
	fRevise         = "revise_intake"
)

const llmCallTimeout = 45 * time.Second
const pollWaitTimeout = 10 * time.Minute // spec §4.2

// ------------------------------------------------------------------ entry points

// runIntakeTurn is the §4 turn() function for an inbound text message. Called from handle() only
// while trip.State == Collecting.
func (b *Brain) runIntakeTurn(ctx context.Context, trip *models.Trip, m models.IncomingMessage) error {
	if trip.Intake == nil {
		trip = b.seedIntake(ctx, trip, m)
	}

	trip = b.applyChatShortcuts(ctx, trip, m)

	extraction, err := b.extractIntake(ctx, trip, m.Text, m.SenderID)
	if err != nil {
		slog.Warn("intake extraction failed, continuing without it", "group_id", trip.GroupID, "err", err)
		extraction = nil
	}

	if extraction != nil && extraction.TripIntent == "cancel" && looksLikeCancelBooking(m.Text) {
		_, err := store.SetState(ctx, b.Store, trip.ID, models.Cancelled, nil)
		if err != nil {
			return err
		}
		return b.writeAndSend(ctx, trip, "CANCEL", "the group asked to stop planning. One short line.", "")
	}

	harvestedDates := false
	if extraction != nil {
		trip = b.applyExtraction(ctx, trip, extraction, m.SenderID, m.MessageID)
	}
	trip, harvestedDates = b.applyHarvestedTripFacts(ctx, trip, m.Text)
	if harvestedDates {
		if extraction == nil {
			extraction = &intakeExtraction{TripIntent: "none"}
		}
		extraction.Updates = append(extraction.Updates, intakeUpdate{
			Scope: "trip", Field: fExactDates, ValueText: m.Text, Confidence: "confirmed",
		})
		if trip.PendingQuestion != nil && (trip.PendingQuestion.Field == fDateWindow || trip.PendingQuestion.Field == fExactDates) {
			extraction.AnswersPendingQuestion = true
		}
	}
	trip = b.applyChatShortcuts(ctx, trip, m)
	if extraction == nil {
		extraction = &intakeExtraction{TripIntent: "none"}
	}
	extraction.SourceText = m.Text
	extraction.SenderName = m.SenderName
	if trip.PendingQuestion != nil && fieldSatisfied(trip, trip.PendingQuestion.Field) {
		extraction.AnswersPendingQuestion = true
	}
	if anyAttendanceKnown(trip) && trip.PendingQuestion != nil && trip.PendingQuestion.Field == fAttendance {
		extraction.AnswersPendingQuestion = true
	}
	if looksLikeAdvisorAsk(m.Text) {
		return b.answerDuringIntake(ctx, trip, m)
	}

	return b.continueIntake(ctx, trip, extraction)
}

// runIntakePollVote is the §4.3 poll-vote path. Called from handlePollVote only when the vote's
// poll_message_id matches one of trip.IntakePolls (i.e. it's ours, not a payer/finalize poll from
// the existing AwaitingChoice-onward machinery).
func (b *Brain) runIntakePollVote(ctx context.Context, trip *models.Trip, vote models.PollVote) error {
	poll := findIntakePoll(trip, vote.PollMessageID, vote.PollName)
	if poll == nil {
		return nil
	}
	trip = b.recordPollVote(ctx, trip, poll, vote)

	complete := pollComplete(trip, poll)
	stale := time.Since(poll.CreatedAt) > pollWaitTimeout
	if !complete && !stale {
		return nil
	}
	if !complete && poll.Field == fExactDates {
		return b.writeAndSend(ctx, trip, "ASK", "some people have not picked dates. Ask those people by name. Do not lock a date from the votes so far.", fExactDates)
	}

	trip = b.closePollAndApply(ctx, trip, poll)
	return b.continueIntake(ctx, trip, nil)
}

// isIntakePoll reports whether a poll_message_id belongs to this turn controller (as opposed to
// the existing finalize/payer polls further down the pipeline).
func isIntakePoll(trip *models.Trip, pollMessageID string) bool {
	return findIntakePoll(trip, pollMessageID, "") != nil
}

// findIntakePoll matches a vote to the open intake poll it belongs to. A vote only binds to a
// poll with an empty id when that is the single open poll — otherwise an older poll's vote would
// advance the question we just asked.
func findIntakePoll(trip *models.Trip, pollMessageID, pollName string) *models.IntakePoll {
	if trip == nil {
		return nil
	}
	var open []*models.IntakePoll
	for i := range trip.IntakePolls {
		p := &trip.IntakePolls[i]
		if p.Closed {
			continue
		}
		open = append(open, p)
		if pollMessageID != "" && p.PollMessageID == pollMessageID {
			return p
		}
	}
	if name := strings.TrimSpace(pollName); name != "" {
		for _, p := range open {
			if strings.EqualFold(strings.TrimSpace(p.Question), name) {
				return p
			}
		}
	}
	if len(open) == 1 && open[0].PollMessageID == "" {
		return open[0]
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
		return b.writeAndSend(ctx, trip, "RESOLVE_CONFLICT", conflicts[0].Description, conflicts[0].Field)
	}
	if len(trip.Conflicts) > 0 {
		trip = b.clearResolvedConflicts(ctx, trip)
	}

	pendingField := ""
	if trip.PendingQuestion != nil {
		pendingField = trip.PendingQuestion.Field
	}
	if pendingField != "" && fieldSatisfied(trip, pendingField) {
		trip = b.closeOpenPollsForField(ctx, trip, pendingField)
		trip = b.clearPendingQuestion(ctx, trip)
	}

	if extraction != nil && extraction.Approval == "yes" && searchEssentials(trip) {
		pendingPkg := pendingField == fPackage || pendingField == fRevise
		if pendingPkg || (trip.Intake != nil && trip.Intake.Confirmed) {
			trip = b.markPackageConfirmed(ctx, trip)
			return b.handoffToSearch(ctx, trip)
		}
	}

	missing := nextMissingField(trip)
	if missing == "" {
		if searchEssentials(trip) {
			return b.handoffToSearch(ctx, trip)
		}
		if extraction != nil && strings.TrimSpace(extraction.SourceText) != "" {
			return b.answerQuestion(ctx, trip, models.IncomingMessage{SenderName: extraction.SenderName, Text: extraction.SourceText, Tagged: true})
		}
		return nil
	}
	if hasOpenIntakePoll(trip, missing) {
		if extraction != nil && strings.TrimSpace(extraction.SourceText) != "" {
			return b.answerQuestion(ctx, trip, models.IncomingMessage{SenderName: extraction.SenderName, Text: extraction.SourceText, Tagged: true})
		}
		return nil
	}
	return b.askField(ctx, trip, missing)
}

func (b *Brain) markPackageConfirmed(ctx context.Context, trip *models.Trip) *models.Trip {
	if trip.Intake == nil {
		trip.Intake = &models.TripIntake{}
	}
	trip.Intake.Confirmed = true
	trip.Intake.Revision = false
	updated, err := b.Store.UpdateTrip(ctx, trip.ID, map[string]any{"intake": trip.Intake})
	if err != nil || updated == nil {
		return trip
	}
	return updated
}

// shouldUseIntake reports whether this trip should keep collecting requirements instead of
// proposing options from origin and overlapping dates alone.
func shouldUseIntake(trip *models.Trip) bool {
	if trip == nil {
		return false
	}
	if trip.State == models.Collecting {
		return true
	}
	if trip.Intake == nil {
		return false
	}
	switch trip.State {
	case models.Booked, models.BookingState, models.Searching, models.Cancelled:
		return false
	default:
		return !searchEssentials(trip)
	}
}

func isoDateOK(s string) bool {
	_, err := time.Parse("2006-01-02", strings.TrimSpace(s))
	return err == nil
}

// ------------------------------------------------------------------ readiness (§4.5)

// readyToSearch implements spec §4.5's exact checklist. This — not "origin + overlapping dates"
// — is the only thing that may be true before a search can start, and even then only after an
// explicit ✅ (checked by the caller, continueIntake).
func readyToSearch(trip *models.Trip) bool {
	return searchEssentials(trip) && trip.Intake != nil && trip.Intake.Confirmed
}

// searchEssentials is the checklist that must be true before any flight or stay search.
func searchEssentials(trip *models.Trip) bool {
	if trip == nil || trip.Intake == nil {
		return false
	}
	for _, f := range []string{fAttendance, fOrigin, fExactDates, fBudgetRange, fBudgetIncludes, fDestination} {
		if !fieldSatisfied(trip, f) {
			return false
		}
	}
	ed := trip.Intake.ExactDates
	return ed != nil && isoDateOK(ed.Depart) && isoDateOK(ed.Return)
}

// nextMissingField returns the first unsatisfied field in spec §4.1's order, or "" if every
// required field is confirmed/inferred.
func nextMissingField(trip *models.Trip) string {
	order := []string{fAttendance, fChildren, fOrigin, fDateWindow, fExactDates, fBudgetRange, fBudgetIncludes, fVibe, fDestination, fConstraints, fPackage}
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
		coming := 0
		unknown := 0
		for _, p := range trip.Participants {
			if p.Intake == nil || p.Intake.Attendance == "" || p.Intake.Attendance == "unknown" {
				unknown++
				continue
			}
			if p.Intake.Attendance == "coming" || p.Intake.Attendance == "maybe" {
				coming++
			}
		}
	if coming < 1 {
		return false
	}
	if n, ok := headcountOf(intake.Headcount); ok && n <= 1 {
		return true
	}
	if unknown == 0 {
			return true
		}
		// A closed attendance poll means everyone who is going has had a chance to say so.
		return pollWasAsked(trip, fAttendance) && !hasOpenIntakePoll(trip, fAttendance)
	case fChildren:
		if intake.Children.Known() {
			return true
		}
		return !childrenFollowUp(trip)
	case fOrigin:
		for _, p := range coming {
			if p.Intake == nil || !p.Intake.Origin.Known() {
				return false
			}
		}
		return len(coming) > 0
	case fDateWindow:
		if intake.ExactDates != nil && isoDateOK(intake.ExactDates.Depart) && isoDateOK(intake.ExactDates.Return) {
			return true
		}
		return intake.DateWindow != nil && isoDateOK(intake.DateWindow.Earliest) && intake.Nights.Min > 0
	case fExactDates:
		return intake.ExactDates != nil && isoDateOK(intake.ExactDates.Depart) && isoDateOK(intake.ExactDates.Return)
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
	case fPackage:
		return intake.Confirmed
	case fRevise:
		return !intake.Revision
	}
	return true
}

func childrenFollowUp(trip *models.Trip) bool {
	n := len(comingParticipants(trip))
	if trip != nil && trip.Intake != nil && trip.Intake.Headcount.Known() {
		switch v := trip.Intake.Headcount.Value.(type) {
		case float64:
			if int(v) > n {
				n = int(v)
			}
		case int:
			if v > n {
				n = v
			}
		}
	}
	return n >= 2
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
	user := text
	if recent := b.recentChat(cctx, trip); recent != "(none)" {
		user = "Recent chat:\n" + recent + "\nLatest message:\n" + text
	}

	out, err := b.LLM.Structured(cctx, system, []llm.Message{{Role: "user", Content: user}}, toSchema(prompts.IntakeExtract))
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

func (b *Brain) applyHarvestedTripFacts(ctx context.Context, trip *models.Trip, text string) (*models.Trip, bool) {
	h := harvestText(text, b.today())
	if len(h.Dates) < 2 {
		return trip, false
	}
	dates := unionDates(nil, h.Dates)
	if len(dates) < 2 {
		return trip, false
	}
	start, end := dates[0], dates[len(dates)-1]
	intake := trip.Intake
	if intake == nil {
		intake = &models.TripIntake{}
	}
	if intake.DateWindow == nil || intake.DateWindow.Earliest == "" {
		intake.DateWindow = &models.DateWindow{Earliest: start, Latest: end}
	}
	span := len(dates)
	exact := span > 0 && span <= 8 && isoDateOK(start) && isoDateOK(end)
	if exact && (intake.ExactDates == nil || !isoDateOK(intake.ExactDates.Depart)) {
		intake.ExactDates = &models.ExactDates{Depart: start, Return: end}
	}
	if intake.Nights.Min == 0 && exact {
		if t1, err1 := models.ParseDate(start); err1 == nil {
			if t2, err2 := models.ParseDate(end); err2 == nil {
				n := int(t2.Sub(t1).Hours() / 24)
				if n < 1 {
					n = 1
				}
				intake.Nights = models.NightsRange{Min: n, Max: n}
			}
		}
	}
	if h.Destination != "" && !intake.Destination.Known() {
		intake.Destination = models.FieldValue{Value: h.Destination, Confidence: models.Confirmed, UpdatedAt: models.Now()}
	}
	updated, err := b.Store.UpdateTrip(ctx, trip.ID, map[string]any{"intake": intake})
	if err != nil || updated == nil {
		trip.Intake = intake
		return trip, true
	}
	return updated, true
}

var (
	soloTravelerRe   = regexp.MustCompile(`(?i)\b(just me|only me|just myself|i(?:'m| am) the only(?: one| person)?|only (?:one|i)(?:'s| is)? going|solo(?: trip)?)\b`)
	attendanceYesRe  = regexp.MustCompile(`(?i)^\s*(?:@\S+\s+)*(?:yes(?:\s+i\s+can)?|yeah|yep|yup|sure|ok(?:ay)?|i\s+can|i\s+confirm|confirmed|confirm|i'?m\s+in|count\s+me\s+in|just\s+me|only\s+me|coming)[\s!.]*$`)
	originCueRe      = regexp.MustCompile(`(?i)\b(from|out of|flying from|fly from|depart(?:ing)? from)\b`)
	sharedOriginRe   = regexp.MustCompile(`(?i)\b(we all|everyone|all of us|we(?:'re| are) all)\b`)
	noneAnswerRe     = regexp.MustCompile(`(?i)^\s*(?:none|nope|no|nothing|all good|no constraints?|no preferences?|all adults|no kids|no children|just adults)\b`)
	kidsCountRe      = regexp.MustCompile(`(?i)\b(\d+)\s*(?:kids?|children|child)\b`)
	includesFoodRe   = regexp.MustCompile(`(?i)\b(food|activities|everything|all of it|the whole thing)\b`)
	includesFlightRe = regexp.MustCompile(`(?i)\bflights?\b`)
	includesStayRe   = regexp.MustCompile(`(?i)\b(hotels?|stay|stays|accommodation)\b`)
	mentionDatesRe   = regexp.MustCompile(`(?i)\b(dates?|weekend|nights?|when)\b`)
	mentionBudgetRe  = regexp.MustCompile(`(?i)\bbudget\b|\$|c\$`)
	mentionDestRe    = regexp.MustCompile(`(?i)\b(destination|city|vibe|where)\b`)
)

func strongAttendanceYes(text string) bool {
	text = strings.TrimSpace(text)
	if text == "" || len(text) > 80 {
		return soloTravelerRe.MatchString(text)
	}
	return attendanceYesRe.MatchString(text) || soloTravelerRe.MatchString(text)
}

func (b *Brain) applyChatShortcuts(ctx context.Context, trip *models.Trip, m models.IncomingMessage) *models.Trip {
	text := strings.TrimSpace(m.Text)
	if text == "" {
		return trip
	}
	h := harvestText(text, b.today())
	changed := false
	intake := trip.Intake
	if intake == nil {
		intake = &models.TripIntake{}
		trip.Intake = intake
	}
	pending := ""
	if trip.PendingQuestion != nil {
		pending = trip.PendingQuestion.Field
	}
	if h.Destination != "" && !intake.Destination.Known() {
		intake.Destination = models.FieldValue{Value: h.Destination, Confidence: models.Confirmed, UpdatedAt: models.Now()}
		changed = true
	}
	pendingAttendance := pending == fAttendance
	if m.SenderID != "" && (strongAttendanceYes(text) || (pendingAttendance && attendanceYesRe.MatchString(text))) && (!anyAttendanceKnown(trip) || pendingAttendance) {
		idx := findOrCreateParticipantIndexPtr(trip, m.SenderID)
		if idx >= 0 {
			if trip.Participants[idx].WhatsAppName == "" {
				trip.Participants[idx].WhatsAppName = m.SenderName
			}
			if trip.Participants[idx].Intake == nil {
				trip.Participants[idx].Intake = &models.ParticipantIntake{}
			}
			trip.Participants[idx].Intake.Attendance = "coming"
			changed = true
		}
		if soloTravelerRe.MatchString(text) && !intake.Headcount.Known() {
			intake.Headcount = models.FieldValue{Value: float64(1), Confidence: models.Confirmed, UpdatedAt: models.Now()}
			changed = true
		}
		if soloTravelerRe.MatchString(text) {
			for i := range trip.Participants {
				if trip.Participants[i].WaID == m.SenderID {
					continue
				}
				if trip.Participants[i].Intake == nil {
					trip.Participants[i].Intake = &models.ParticipantIntake{}
				}
				if trip.Participants[i].Intake.Attendance == "" || trip.Participants[i].Intake.Attendance == "unknown" {
					trip.Participants[i].Intake.Attendance = "not_coming"
					changed = true
				}
			}
		}
	}
	if (h.City != "" || h.Airport != "") && (pending == fOrigin || originCueRe.MatchString(text)) {
		label := h.City
		if label == "" {
			label = h.Airport
		}
		if sharedOriginRe.MatchString(text) {
			targets := comingParticipants(trip)
			if len(targets) == 0 {
				targets = trip.Participants
			}
			for _, person := range targets {
				if setParticipantOrigin(trip, person.WaID, label) {
					changed = true
				}
			}
		} else if m.SenderID != "" {
			if setParticipantOrigin(trip, m.SenderID, label) {
				if m.SenderName != "" {
					if idx := findOrCreateParticipantIndexPtr(trip, m.SenderID); idx >= 0 && trip.Participants[idx].WhatsAppName == "" {
						trip.Participants[idx].WhatsAppName = m.SenderName
					}
				}
				changed = true
			}
		}
	}
	if h.Budget != "" && (intake.BudgetPP == nil || intake.BudgetPP.Max == 0) {
		if n, ok := parseFirstNumber(h.Budget); ok && n > 0 {
			bp := intake.BudgetPP
			if bp == nil {
				bp = &models.BudgetPP{Currency: "CAD"}
			}
			bp.Min, bp.Max = n, n
			if bp.Currency == "" {
				bp.Currency = "CAD"
			}
			intake.BudgetPP = bp
			changed = true
		}
	}
	if h.Nights > 0 && intake.Nights.Min == 0 {
		intake.Nights = models.NightsRange{Min: h.Nights, Max: h.Nights}
		changed = true
	}
	if includes := budgetIncludesFromText(text); len(includes) > 0 && (pending == fBudgetIncludes || intake.BudgetPP == nil || len(intake.BudgetPP.Includes) == 0) {
		if pending == fBudgetIncludes || includesFlightRe.MatchString(text) {
			bp := intake.BudgetPP
			if bp == nil {
				bp = &models.BudgetPP{Currency: "CAD"}
			}
			bp.Includes = includes
			intake.BudgetPP = bp
			changed = true
		}
	}
	if pending == fChildren {
		if v, ok := childrenFromText(text); ok {
			intake.Children = models.FieldValue{Value: v, Confidence: models.Confirmed, UpdatedAt: models.Now()}
			changed = true
		}
	}
	if pending == fConstraints && text != "" {
		val := text
		if noneAnswerRe.MatchString(text) {
			val = "none"
		}
		intake.Constraints = models.FieldValue{Value: val, Confidence: models.Confirmed, UpdatedAt: models.Now()}
		changed = true
	}
	if intake.Revision || pending == fRevise {
		if mentionDatesRe.MatchString(text) && len(h.Dates) == 0 {
			intake.ExactDates = nil
			intake.DateWindow = nil
			intake.Nights = models.NightsRange{}
			changed = true
		}
		if mentionBudgetRe.MatchString(text) && h.Budget == "" {
			if intake.BudgetPP != nil {
				intake.BudgetPP.Min, intake.BudgetPP.Max = 0, 0
				intake.BudgetPP.Includes = nil
			}
			changed = true
		}
		if mentionDestRe.MatchString(text) && h.Destination == "" && !strings.Contains(strings.ToLower(text), "where are you flying") {
			intake.Destination = models.FieldValue{}
			intake.Vibe = models.FieldValue{}
			changed = true
		}
		if changed {
			intake.Revision = false
			intake.Confirmed = false
		}
	}
	if !changed {
		return trip
	}
	updated, err := b.Store.UpdateTrip(ctx, trip.ID, map[string]any{"intake": intake, "participants": trip.Participants})
	if err != nil || updated == nil {
		trip.Intake = intake
		return trip
	}
	return updated
}

func setParticipantOrigin(trip *models.Trip, waID, label string) bool {
	if trip == nil || waID == "" || strings.TrimSpace(label) == "" {
		return false
	}
	idx := findOrCreateParticipantIndexPtr(trip, waID)
	if idx < 0 {
		return false
	}
	p := &trip.Participants[idx]
	if p.Intake == nil {
		p.Intake = &models.ParticipantIntake{}
	}
	if p.Intake.Origin.Known() && p.Intake.Origin.Confidence == models.Confirmed {
		return false
	}
	p.Intake.Origin = models.FieldValue{Value: label, Confidence: models.Confirmed, UpdatedAt: models.Now()}
	if code := airportFromLabel(label); code != "" {
		p.OriginAirport = code
	}
	p.OriginCity = label
	p.Origin = label
	return true
}

func budgetIncludesFromText(text string) []string {
	flights := includesFlightRe.MatchString(text)
	stay := includesStayRe.MatchString(text)
	food := includesFoodRe.MatchString(text)
	switch {
	case food && (flights || stay || strings.Contains(strings.ToLower(text), "everything") || strings.Contains(strings.ToLower(text), "all of it")):
		return []string{"flights", "stay", "food", "activities"}
	case flights && stay:
		return []string{"flights", "stay"}
	case flights && !stay && !food:
		return []string{"flights"}
	case stay && !flights && !food:
		return []string{"stay"}
	default:
		return nil
	}
}

func childrenFromText(text string) (string, bool) {
	if noneAnswerRe.MatchString(text) || regexp.MustCompile(`(?i)\bno kids\b|\bno children\b|\ball adults\b`).MatchString(text) {
		return "0", true
	}
	if m := kidsCountRe.FindStringSubmatch(text); len(m) == 2 {
		return m[1], true
	}
	return "", false
}

func hasOpenIntakePoll(trip *models.Trip, field string) bool {
	if trip == nil {
		return false
	}
	for _, p := range trip.IntakePolls {
		if !p.Closed && p.Field == field {
			return true
		}
	}
	return false
}

func (b *Brain) closeOpenPollsForField(ctx context.Context, trip *models.Trip, field string) *models.Trip {
	changed := false
	for i := range trip.IntakePolls {
		if !trip.IntakePolls[i].Closed && trip.IntakePolls[i].Field == field {
			trip.IntakePolls[i].Closed = true
			changed = true
		}
	}
	if !changed {
		return trip
	}
	updated, err := b.Store.UpdateTrip(ctx, trip.ID, map[string]any{"intake_polls": trip.IntakePolls})
	if err != nil || updated == nil {
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
	if fieldSatisfied(trip, field) || hasOpenIntakePoll(trip, field) {
		return nil
	}
	switch field {
	case fAttendance:
		if pollWasAsked(trip, field) {
			return b.writeAndSend(ctx, trip, "ASK", "ask who is actually coming. One question. Accept names.", fAttendance)
		}
		return b.askAttendancePoll(ctx, trip)
	case fChildren:
		return b.writeAndSend(ctx, trip, "ASK", "ask whether any children are coming, or if it is all adults. One question.", fChildren)
	case fOrigin:
		if pollWasAsked(trip, field) {
			return b.writeAndSend(ctx, trip, "ASK", "ask only the people still missing a departure city, by name. One question.", fOrigin)
		}
		return b.askOriginPoll(ctx, trip)
	case fDateWindow:
		return b.writeAndSend(ctx, trip, "ASK", "ask for a rough availability window and how many nights they want. Chat, not a poll. One question.", fDateWindow)
	case fExactDates:
		if pollWasAsked(trip, field) {
			return b.writeAndSend(ctx, trip, "ASK", "the date picks do not overlap for everyone who is coming. Ask for one range that works for all of them. Do not use a majority.", fExactDates)
		}
		return b.askExactDatesPoll(ctx, trip)
	case fBudgetRange:
		if pollWasAsked(trip, field) {
			return b.writeAndSend(ctx, trip, "ASK", "ask for a per-person budget in CAD. One question.", fBudgetRange)
		}
		return b.askBudgetPoll(ctx, trip)
	case fBudgetIncludes:
		return b.writeAndSend(ctx, trip, "ASK", "ask whether that per-person budget covers flights and accommodation only, or also food and activities. One question.", fBudgetIncludes)
	case fVibe:
		if pollWasAsked(trip, field) {
			return b.writeAndSend(ctx, trip, "ASK", "ask what kind of trip they want: beach, city, nature, or open to suggestions. One question.", fVibe)
		}
		return b.askVibePoll(ctx, trip)
	case fDestination:
		if pollWasAsked(trip, field) {
			return b.writeAndSend(ctx, trip, "ASK", "ask which destination they want. One question.", fDestination)
		}
		return b.askDestinationPoll(ctx, trip)
	case fConstraints:
		return b.writeAndSend(ctx, trip, "ASK", "ask once about must-haves: room sharing, accessibility, dietary needs, flight limits, and places to avoid. Accept none.", fConstraints)
	case fPackage:
		if trip.Intake != nil && trip.Intake.Revision {
			return b.writeAndSend(ctx, trip, "ASK", "they want to change the trip package. Ask which part to change: who's coming, departure city, dates, budget, or destination. One question.", fRevise)
		}
		return b.askPackagePoll(ctx, trip)
	}
	return nil
}

func pollWasAsked(trip *models.Trip, field string) bool {
	if trip == nil {
		return false
	}
	for _, p := range trip.IntakePolls {
		if p.Field == field {
			return true
		}
	}
	return false
}

func (b *Brain) askAttendancePoll(ctx context.Context, trip *models.Trip) error {
	return b.postIntakePoll(ctx, trip, fAttendance, "Who's coming?", []string{"Coming", "Maybe", "Not coming"}, nil, false,
		"post the attendance poll. One short line introducing it, in the group's voice.")
}

func (b *Brain) askOriginPoll(ctx context.Context, trip *models.Trip) error {
	cities := candidateOriginCities(trip)
	options := append(append([]string{}, cities...), "Other — I'll reply in chat")
	return b.postIntakePoll(ctx, trip, fOrigin, "Where are you flying from?", options, nil, false,
		"post a poll of likely departure cities plus an Other option. One short line. Each person answers for themselves.")
}

func (b *Brain) askExactDatesPoll(ctx context.Context, trip *models.Trip) error {
	window := (*models.DateWindow)(nil)
	nights := models.NightsRange{}
	if trip.Intake != nil {
		window = trip.Intake.DateWindow
		nights = trip.Intake.Nights
		if window == nil && trip.Intake.ExactDates != nil {
			window = &models.DateWindow{Earliest: trip.Intake.ExactDates.Depart, Latest: trip.Intake.ExactDates.Return}
		}
	}
	labels, values := generateDateOptions(window, nights)
	if len(labels) == 0 {
		return b.writeAndSend(ctx, trip, "ASK", "the date window does not fit the trip length. Ask for a wider window. One question.", fDateWindow)
	}
	if len(labels) == 1 {
		if start, end, ok := parseDateRangeText(values[0]); ok && trip.Intake != nil {
			trip.Intake.ExactDates = &models.ExactDates{Depart: start, Return: end}
			if _, err := b.Store.UpdateTrip(ctx, trip.ID, map[string]any{"intake": trip.Intake}); err == nil {
				return b.continueIntake(ctx, trip, nil)
			}
		}
	}
	return b.postIntakePoll(ctx, trip, fExactDates, "Which dates work for everyone?", labels, values, true,
		"post a multi-select poll of concrete date ranges inside their window. One short line. Say that a date only counts if everyone can do it.")
}

func (b *Brain) askBudgetPoll(ctx context.Context, trip *models.Trip) error {
	return b.postIntakePoll(ctx, trip, fBudgetRange, "Budget per person?",
		[]string{"Under C$600", "C$600–900", "C$900–1300", "C$1300+"}, nil, false,
		"post a poll of CAD per-person budget ranges. One short line. This is each person's own cap, not a group average.")
}

func (b *Brain) askVibePoll(ctx context.Context, trip *models.Trip) error {
	return b.postIntakePoll(ctx, trip, fVibe, "What kind of trip?",
		[]string{"Beach", "City", "Nature", "Open to suggestions"}, nil, false,
		"post a poll asking what kind of trip they want. One short line.")
}

func (b *Brain) askPackagePoll(ctx context.Context, trip *models.Trip) error {
	label := packageLabel(trip)
	if label == "" {
		return b.writeAndSend(ctx, trip, "ASK", "summarize the destination, exact dates, and budget per person, and ask if you should search. One message.", fPackage)
	}
	return b.postIntakePoll(ctx, trip, fPackage, "Lock this trip?",
		[]string{label, "Change something"}, []string{"go", "change"}, false,
		"post a poll with the full trip package (destination, exact dates, budget per person) and a change option. One short line. Do not say you have searched.")
}

func packageLabel(trip *models.Trip) string {
	if trip == nil || trip.Intake == nil || !trip.Intake.Destination.Known() || trip.Intake.ExactDates == nil {
		return ""
	}
	dest := trip.Intake.Destination.AsString()
	when := formatShortDate(trip.Intake.ExactDates.Depart) + "–" + formatShortDate(trip.Intake.ExactDates.Return)
	cost := "budget TBD"
	if trip.Intake.BudgetPP != nil && trip.Intake.BudgetPP.Max > 0 {
		cost = fmt.Sprintf("up to C$%.0f each", trip.Intake.BudgetPP.Max)
	}
	label := dest + " · " + when + " · " + cost
	if len(label) > 90 {
		label = strings.TrimSpace(label[:87]) + "…"
	}
	return label
}

func formatShortDate(iso string) string {
	t, err := time.Parse("2006-01-02", iso)
	if err != nil {
		return iso
	}
	return t.Format("Jan 2")
}

func (b *Brain) askDestinationPoll(ctx context.Context, trip *models.Trip) error {
	options, err := b.proposeDestinations(ctx, trip)
	if err != nil || len(options) == 0 {
		return b.writeAndSend(ctx, trip, "ASK", "ask if anyone has a destination in mind. One question.", fDestination)
	}
	return b.postIntakePoll(ctx, trip, fDestination, "Which city?", options, nil, false,
		"post a single-select poll of 2-3 destinations that fit the budget, origins, and dates. One short line.")
}

// proposeDestinations asks Gemini for 2-3 destination names fitting what's known so far — a much
// smaller ask than the existing propose()'s full options-with-pricing, since all that's needed
// here is a poll of city names.
func (b *Brain) proposeDestinations(ctx context.Context, trip *models.Trip) ([]string, error) {
	cctx, cancel := context.WithTimeout(ctx, llmCallTimeout)
	defer cancel()
	vibe := ""
	if trip.Intake != nil {
		vibe = trip.Intake.Vibe.AsString()
	}
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

func generateDateOptions(window *models.DateWindow, nights models.NightsRange) (labels, values []string) {
	if window == nil || !isoDateOK(window.Earliest) {
		return nil, nil
	}
	start, err := time.Parse("2006-01-02", window.Earliest)
	if err != nil {
		return nil, nil
	}
	end := start
	if isoDateOK(window.Latest) {
		if e, err := time.Parse("2006-01-02", window.Latest); err == nil {
			end = e
		}
	}
	n := nights.Min
	if n <= 0 {
		n = nights.Max
	}
	if n <= 0 {
		n = 3
	}
	for d := start; !d.After(end) && len(labels) < 5; d = d.AddDate(0, 0, 2) {
		ret := d.AddDate(0, 0, n)
		if ret.After(end) && !ret.Equal(end) {
			continue
		}
		labels = append(labels, fmt.Sprintf("%s – %s", d.Format("Mon Jan 2"), ret.Format("Mon Jan 2")))
		values = append(values, d.Format("2006-01-02")+".."+ret.Format("2006-01-02"))
	}
	return labels, values
}

// postIntakePoll calls the writer for a one-line intro, sends the poll, and tracks it so votes
// bind to this question instead of an older poll.
func (b *Brain) postIntakePoll(ctx context.Context, trip *models.Trip, field, question string, options, values []string, multi bool, slot string) error {
	options, values = clipPollPair(options, values)
	if len(options) < 2 {
		return b.writeAndSend(ctx, trip, "ASK", slot, field)
	}
	text, _, _, _, err := b.writeIntake(ctx, trip, "ASK_POLL", slot, trip)
	intro := strings.TrimSpace(text)
	if err != nil || intro == "" || strings.EqualFold(intro, question) || strings.Contains(intro, slot) {
		intro = ""
	}
	messageID, err := b.Messenger.SendPoll(ctx, trip.GroupID, messaging.Poll{Name: question, Options: options, AllowMultipleAnswers: multi})
	if err != nil {
		return err
	}
	if intro != "" && intro != trip.LastAgentText {
		if err := b.Messenger.Send(ctx, trip.GroupID, intro, nil); err != nil {
			slog.Warn("failed to send poll intro", "group_id", trip.GroupID, "err", err)
		}
	}

	expected := make([]string, 0, len(trip.Participants))
	people := comingParticipants(trip)
	if field == fAttendance {
		people = trip.Participants
	}
	for _, p := range people {
		if p.WaID != "" {
			expected = append(expected, p.WaID)
		}
	}
	poll := models.IntakePoll{
		PollMessageID: messageID, Field: field, Question: question, Options: options, OptionValues: values,
		Multi: multi, ExpectedVoters: expected, Votes: map[string][]string{}, CreatedAt: models.Now(),
	}
	trip = b.setPendingQuestion(ctx, trip, field, messageID, "group")
	trip.IntakePolls = append(trip.IntakePolls, poll)
	_, _ = b.Store.UpdateTrip(ctx, trip.ID, map[string]any{"intake_polls": trip.IntakePolls, "last_agent_text": intro})
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

func pollVoteSelection(votes map[string][]string, id string) []string {
	if sel, ok := votes[id]; ok && len(sel) > 0 && strings.TrimSpace(sel[0]) != "" {
		return sel
	}
	user := strings.Split(id, "@")[0]
	for voter, sel := range votes {
		if len(sel) == 0 || strings.TrimSpace(sel[0]) == "" {
			continue
		}
		if voter == id || (user != "" && strings.Split(voter, "@")[0] == user) {
			return sel
		}
	}
	return nil
}

func pollHasSelection(votes map[string][]string) bool {
	for _, sel := range votes {
		if len(sel) > 0 && strings.TrimSpace(sel[0]) != "" {
			return true
		}
	}
	return false
}

func pollComplete(trip *models.Trip, poll *models.IntakePoll) bool {
	if poll == nil || !pollHasSelection(poll.Votes) {
		return false
	}
	if len(poll.ExpectedVoters) == 0 || len(comingParticipants(trip)) <= 1 {
		return true
	}
	matched, unmatched := 0, 0
	for _, id := range poll.ExpectedVoters {
		if len(pollVoteSelection(poll.Votes, id)) > 0 {
			matched++
		} else {
			unmatched++
		}
	}
	if unmatched == 0 {
		return true
	}
	// Roster ids and the vote id differ (@lid vs @c.us). Don't wait forever.
	return matched == 0
}

// closePollAndApply applies every recorded vote to the relevant intake field (poll votes are
// already structured, so this bypasses LLM extraction entirely) and marks the poll closed.
func (b *Brain) closePollAndApply(ctx context.Context, trip *models.Trip, poll *models.IntakePoll) *models.Trip {
	if trip.Intake == nil {
		trip.Intake = &models.TripIntake{}
	}
	switch poll.Field {
	case fAttendance:
		coming := 0
		for voterID, selected := range poll.Votes {
			if len(selected) == 0 || strings.TrimSpace(selected[0]) == "" {
				continue
			}
			idx := findOrCreateParticipantIndexPtr(trip, voterID)
			if idx >= 0 {
				if trip.Participants[idx].Intake == nil {
					trip.Participants[idx].Intake = &models.ParticipantIntake{}
				}
				trip.Participants[idx].Intake.Attendance = normalizeAttendance(selected[0])
				if trip.Participants[idx].Intake.Attendance == "coming" || trip.Participants[idx].Intake.Attendance == "maybe" {
					coming++
				}
			}
		}
		if coming > 0 && trip.Intake != nil && !trip.Intake.Headcount.Known() {
			trip.Intake.Headcount = models.FieldValue{Value: float64(coming), Confidence: models.Confirmed, UpdatedAt: models.Now()}
		}
	case fOrigin:
		for voterID, selected := range poll.Votes {
			if len(selected) == 0 || strings.HasPrefix(selected[0], "Other") {
				continue
			}
			setParticipantOrigin(trip, voterID, selected[0])
		}
	case fExactDates:
		for voterID, selected := range poll.Votes {
			idx := findOrCreateParticipantIndexPtr(trip, voterID)
			if idx < 0 {
				continue
			}
			if trip.Participants[idx].Intake == nil {
				trip.Participants[idx].Intake = &models.ParticipantIntake{}
			}
			for _, label := range selected {
				if start, end, ok := parseDateRangeText(optionValueFor(poll, label)); ok {
					trip.Participants[idx].Intake.Available = append(trip.Participants[idx].Intake.Available, models.DateRange{Start: start, End: end, SourceID: poll.PollMessageID})
				}
			}
		}
		if depart, ret, ok := sharedDateChoice(poll); ok && trip.Intake != nil {
			trip.Intake.ExactDates = &models.ExactDates{Depart: depart, Return: ret}
		}
	case fBudgetRange:
		lo, hi := tightestBudget(poll)
		if hi > 0 && trip.Intake != nil {
			bp := trip.Intake.BudgetPP
			if bp == nil {
				bp = &models.BudgetPP{Currency: "CAD"}
			}
			bp.Min, bp.Max = lo, hi
			if bp.Currency == "" {
				bp.Currency = "CAD"
			}
			trip.Intake.BudgetPP = bp
		}
		for voterID, selected := range poll.Votes {
			if len(selected) == 0 {
				continue
			}
			_, b := parseBudgetRangeLabel(selected[0])
			if b <= 0 {
				continue
			}
			idx := findOrCreateParticipantIndexPtr(trip, voterID)
			if idx >= 0 {
				if trip.Participants[idx].Intake == nil {
					trip.Participants[idx].Intake = &models.ParticipantIntake{}
				}
				trip.Participants[idx].Intake.BudgetPP = models.FieldValue{Value: b, Confidence: models.Confirmed, SourceID: poll.PollMessageID, UpdatedAt: models.Now()}
			}
		}
	case fVibe:
		if common := commonlyVoted(poll); common != "" && trip.Intake != nil {
			trip.Intake.Vibe = models.FieldValue{Value: normalizeVibe(common), Confidence: models.Confirmed, SourceID: poll.PollMessageID, UpdatedAt: models.Now()}
		}
	case fDestination:
		if common := commonlyVoted(poll); common != "" && trip.Intake != nil {
			trip.Intake.Destination = models.FieldValue{Value: common, Confidence: models.Confirmed, SourceID: poll.PollMessageID, UpdatedAt: models.Now()}
		}
	case fPackage:
		goVotes, changeVotes := 0, 0
		for _, selected := range poll.Votes {
			if len(selected) == 0 {
				continue
			}
			value := optionValueFor(poll, selected[0])
			if value == "change" || strings.Contains(strings.ToLower(selected[0]), "change") {
				changeVotes++
			} else {
				goVotes++
			}
		}
		if trip.Intake != nil {
			if changeVotes > 0 {
				trip.Intake.Revision = true
				trip.Intake.Confirmed = false
			} else if goVotes > 0 {
				trip.Intake.Confirmed = true
				trip.Intake.Revision = false
			}
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

func parseBudgetRangeLabel(label string) (lo, hi float64) {
	s := strings.ToLower(label)
	s = strings.ReplaceAll(s, "–", "-")
	s = strings.ReplaceAll(s, ",", "")
	switch {
	case strings.Contains(s, "under"):
		return 0, 600
	case strings.Contains(s, "600-900"):
		return 600, 900
	case strings.Contains(s, "900-1300"):
		return 900, 1300
	case strings.Contains(s, "1300"):
		return 1300, 2500
	}
	return 0, 0
}

func optionValueFor(poll *models.IntakePoll, label string) string {
	if poll == nil {
		return label
	}
	for i, opt := range poll.Options {
		if opt != label {
			continue
		}
		if i < len(poll.OptionValues) && strings.TrimSpace(poll.OptionValues[i]) != "" {
			return poll.OptionValues[i]
		}
		return label
	}
	return label
}

func sharedDateChoice(poll *models.IntakePoll) (string, string, bool) {
	var sets []map[string]bool
	for _, selected := range poll.Votes {
		set := map[string]bool{}
		for _, label := range selected {
			v := optionValueFor(poll, label)
			if strings.Contains(v, "..") {
				set[v] = true
			}
		}
		if len(set) > 0 {
			sets = append(sets, set)
		}
	}
	if len(sets) == 0 {
		return "", "", false
	}
	inter := map[string]bool{}
	for k := range sets[0] {
		inter[k] = true
	}
	for _, set := range sets[1:] {
		next := map[string]bool{}
		for k := range inter {
			if set[k] {
				next[k] = true
			}
		}
		inter = next
	}
	if len(inter) == 0 {
		return "", "", false
	}
	for i, label := range poll.Options {
		v := label
		if i < len(poll.OptionValues) && poll.OptionValues[i] != "" {
			v = poll.OptionValues[i]
		}
		if !inter[v] {
			continue
		}
		if start, end, ok := parseDateRangeText(v); ok && isoDateOK(start) && isoDateOK(end) {
			return start, end, true
		}
	}
	return "", "", false
}

func tightestBudget(poll *models.IntakePoll) (float64, float64) {
	var lo, hi float64
	set := false
	for _, selected := range poll.Votes {
		if len(selected) == 0 {
			continue
		}
		a, b := parseBudgetRangeLabel(selected[0])
		if b <= 0 {
			continue
		}
		if !set || b < hi {
			lo, hi = a, b
			set = true
		}
	}
	return lo, hi
}

func normalizeAttendance(label string) string {
	s := strings.ToLower(strings.TrimSpace(label))
	switch {
	case strings.Contains(s, "not"):
		return "not_coming"
	case strings.Contains(s, "maybe"):
		return "maybe"
	default:
		return "coming"
	}
}

func normalizeVibe(label string) string {
	s := strings.ToLower(label)
	switch {
	case strings.Contains(s, "beach"):
		return "beach"
	case strings.Contains(s, "city"):
		return "city"
	case strings.Contains(s, "nature"), strings.Contains(s, "quiet"):
		return "nature"
	default:
		return "open"
	}
}

// ------------------------------------------------------------------ writer (§6)

func (b *Brain) writeIntake(ctx context.Context, trip *models.Trip, intent, slot string, _ *models.Trip) (text, pollQuestion string, pollOptions []string, pollMulti bool, err error) {
	cctx, cancel := context.WithTimeout(ctx, llmCallTimeout)
	defer cancel()
	recent := b.recentChat(ctx, trip)
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

func (b *Brain) recentChat(ctx context.Context, trip *models.Trip) string {
	if trip == nil {
		return "(none)"
	}
	msgs, err := b.Store.GetMessages(ctx, trip.GroupID, trip.HistoryStart, 8, false)
	if err != nil || len(msgs) == 0 {
		return "(none)"
	}
	var sb strings.Builder
	for _, msg := range msgs {
		line := strings.TrimSpace(msg.Text)
		if line == "" {
			continue
		}
		if len(line) > 160 {
			line = line[:160]
		}
		sb.WriteString(msg.SenderName)
		sb.WriteString(": ")
		sb.WriteString(line)
		sb.WriteString("\n")
	}
	if sb.Len() == 0 {
		return "(none)"
	}
	return sb.String()
}

// writeAndSend turns the controller's intent into one chat message. If the writer fails, it asks
// the actual question once. It does not send a holding line, and it does not repeat the same text.
func (b *Brain) writeAndSend(ctx context.Context, trip *models.Trip, intent, slot, field string) error {
	text, _, _, _, err := b.writeIntake(ctx, trip, intent, slot, trip)
	if err != nil || strings.TrimSpace(text) == "" {
		slog.Warn("intake writer failed", "group_id", trip.GroupID, "intent", intent, "err", err)
		text = naturalAsk(field, slot)
	}
	text = strings.TrimSpace(text)
	if text == "" || text == strings.TrimSpace(trip.LastAgentText) {
		return nil
	}
	if err := b.Messenger.Send(ctx, trip.GroupID, text, nil); err != nil {
		return err
	}
	if field != "" {
		trip = b.setPendingQuestion(ctx, trip, field, "", "group")
	}
	_, _ = b.Store.UpdateTrip(ctx, trip.ID, map[string]any{"last_agent_text": text})
	trip.LastAgentText = text
	return nil
}

func naturalAsk(field, slot string) string {
	if !instructionSlot(slot) {
		return strings.TrimSpace(slot)
	}
	switch field {
	case fAttendance:
		return "Who's coming on this one?"
	case fChildren:
		return "Any kids coming, or is it all adults?"
	case fOrigin:
		return "Where's each person flying from?"
	case fDateWindow:
		return "What dates are you free, and how many nights do you want?"
	case fExactDates:
		return "Which exact dates work for everyone who's coming?"
	case fBudgetRange:
		return "What should I cap the trip at, per person, in CAD?"
	case fBudgetIncludes:
		return "Does that budget cover flights and the stay only, or food and activities too?"
	case fVibe:
		return "Beach, city, nature, or open to suggestions?"
	case fDestination:
		return "Any city in mind, or should I suggest a few?"
	case fConstraints:
		return "Anything I should plan around — room sharing, accessibility, food limits, flight limits, or places to skip? None is fine."
	case fPackage, fRevise:
		return "Want me to change who's coming, the cities, the dates, or the budget?"
	default:
		return "Okay, I'll drop this trip."
	}
}

func instructionSlot(slot string) bool {
	s := strings.ToLower(strings.TrimSpace(slot))
	if s == "" {
		return true
	}
	for _, prefix := range []string{"ask ", "post ", "the ", "some ", "they ", "summarize "} {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}

func (b *Brain) confirmReady(ctx context.Context, trip *models.Trip) error {
	return b.askPackagePoll(ctx, trip)
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

// handoffToSearch copies the finished intake onto the trip and starts the flight and stay search.
// It runs only after the package poll (or an explicit yes to that poll).
func (b *Brain) handoffToSearch(ctx context.Context, trip *models.Trip) error {
	if trip.Intake == nil {
		trip.Intake = &models.TripIntake{}
	}
	for i := range trip.Participants {
		p := &trip.Participants[i]
		if p.Intake == nil || !p.Intake.Origin.Known() {
			continue
		}
		label := p.Intake.Origin.AsString()
		if label == "" {
			continue
		}
		if p.OriginCity == "" {
			p.OriginCity = label
		}
		if p.Origin == "" {
			p.Origin = label
		}
		if p.OriginAirport == "" {
			if code := airportFromLabel(label); code != "" {
				p.OriginAirport = code
			} else {
				p.OriginAirport = label
			}
		}
	}
	fields := map[string]any{"participants": trip.Participants}
	if origin := originAirportCode("", trip.Participants); origin != "" {
		fields["origin"] = origin
	}
	if trip.Intake.BudgetPP != nil && trip.BudgetNote == "" {
		fields["budget_note"] = fmt.Sprintf("C$%.0f-%.0f per person", trip.Intake.BudgetPP.Min, trip.Intake.BudgetPP.Max)
	}
	if _, err := b.Store.UpdateTrip(ctx, trip.ID, fields); err != nil {
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
	trip = b.syncTripBasics(ctx, trip, "")
	return b.startTravelSearch(ctx, trip)
}
