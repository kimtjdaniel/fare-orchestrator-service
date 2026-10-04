package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"time"

	"fare-brain/formatting"
	"fare-brain/models"
	"fare-brain/store"
)

func destIsSet(trip *models.Trip) bool {
	if trip.Destination != "" {
		return true
	}
	return len(trip.Options) > 0
}

func humanRoster(trip *models.Trip, incoming []models.GroupMember, agentID, extraID, extraName string, extraHosts ...string) []models.GroupMember {
	seen := map[string]bool{}
	hosts := hostIDs(agentID, extraHosts...)
	var out []models.GroupMember
	add := func(m models.GroupMember) {
		id := strings.TrimSpace(m.ID)
		name := strings.TrimSpace(m.Name)
		if id == "" && name == "" {
			return
		}
		if m.IsAgent || isHost(id, hosts) || looksLikeWhatsAppID(name) {
			return
		}
		key := id
		if key == "" {
			key = strings.ToLower(name)
		}
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, m)
	}
	for _, m := range incoming {
		add(m)
	}
	for _, m := range trip.Roster {
		add(m)
	}
	if extraID != "" || extraName != "" {
		add(models.GroupMember{ID: extraID, Name: extraName})
	}
	return out
}

func neededIDs(members []models.GroupMember) []string {
	var ids []string
	for _, m := range members {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	return ids
}

func hostIDs(agentID string, extra ...string) []string {
	var out []string
	if strings.TrimSpace(agentID) != "" {
		out = append(out, agentID)
	}
	for _, id := range extra {
		if strings.TrimSpace(id) != "" {
			out = append(out, id)
		}
	}
	return out
}

func isHost(id string, hosts []string) bool {
	for _, host := range hosts {
		if samePerson(id, host) {
			return true
		}
	}
	return false
}

func samePerson(a, b string) bool {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	da, db := digitsOnly(a), digitsOnly(b)
	return len(da) >= 8 && da == db
}

func digitsOnly(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func voteKey(needed []string, voterID string) string {
	for _, id := range needed {
		if samePerson(id, voterID) {
			return id
		}
	}
	return voterID
}

func allVotedYes(pc *models.PendingChange) bool {
	if pc == nil || len(pc.Needed) == 0 {
		return false
	}
	for _, id := range pc.Needed {
		if !strings.EqualFold(pc.Votes[id], "yes") {
			return false
		}
	}
	return true
}

func anyoneVotedNo(pc *models.PendingChange) bool {
	if pc == nil {
		return false
	}
	for _, v := range pc.Votes {
		if strings.EqualFold(v, "no") {
			return true
		}
	}
	return false
}

func (b *Brain) rememberRoster(ctx context.Context, trip *models.Trip, incoming models.IncomingMessage) {
	roster := humanRoster(trip, incoming.Participants, incoming.AgentID, incoming.SenderID, incoming.SenderName, incoming.AgentIDs...)
	if len(roster) == 0 {
		return
	}
	updated, err := b.Store.UpdateTrip(ctx, trip.ID, map[string]any{"roster": roster})
	if err == nil && updated != nil {
		*trip = *updated
	}
}

func (b *Brain) proposedLockedChange(trip *models.Trip, text string) *models.PendingChange {
	if looksLikeBookAsk(text) {
		return nil
	}
	h := harvestText(text, b.today())
	if len(h.Dates) > 0 && hasAnyDates(trip.Participants) && !datesEqual(h.Dates, allStoredDates(trip.Participants)) {
		start, end := h.Dates[0], h.Dates[len(h.Dates)-1]
		return &models.PendingChange{
			Kind:    "dates",
			Summary: fmt.Sprintf("Change dates to %s instead?", formatting.Dates(start, end)),
			Dates:   h.Dates,
		}
	}
	if (h.Airport != "" || h.City != "") && hasSharedOrigin(trip.Participants) {
		cur := strings.ToLower(strings.TrimSpace(trip.Origin))
		next := strings.ToLower(strings.TrimSpace(h.City))
		if next == "" {
			next = strings.ToLower(h.Airport)
		}
		if cur != "" && next != "" && cur != next && !strings.EqualFold(trip.Origin, h.Airport) {
			label := h.City
			if label == "" {
				label = h.Airport
			}
			return &models.PendingChange{
				Kind:    "origin",
				Summary: fmt.Sprintf("Fly out of %s instead?", label),
				Airport: h.Airport,
				City:    h.City,
			}
		}
	}
	if h.Destination != "" && destIsSet(trip) {
		for _, o := range trip.Options {
			if strings.EqualFold(o.Destination, h.Destination) {
				return nil
			}
		}
		if trip.Destination == "" || !strings.EqualFold(h.Destination, trip.Destination) {
			return &models.PendingChange{
				Kind:        "destination",
				Summary:     fmt.Sprintf("Change destination to %s instead?", h.Destination),
				Destination: h.Destination,
			}
		}
	}
	if h.Budget != "" && trip.BudgetNote != "" && !strings.EqualFold(h.Budget, trip.BudgetNote) {
		return &models.PendingChange{
			Kind:    "budget",
			Summary: fmt.Sprintf("Change budget to %s each instead?", h.Budget),
			Budget:  h.Budget,
		}
	}
	return nil
}

func (b *Brain) startChangePoll(ctx context.Context, trip *models.Trip, m models.IncomingMessage, change *models.PendingChange) error {
	roster := humanRoster(trip, m.Participants, m.AgentID, m.SenderID, m.SenderName, m.AgentIDs...)
	needed := neededIDs(roster)
	if len(needed) == 0 && m.SenderID != "" {
		needed = []string{m.SenderID}
	}
	change.Needed = needed
	change.Votes = map[string]string{}
	if _, err := b.Store.UpdateTrip(ctx, trip.ID, map[string]any{
		"pending_change": change,
		"roster":         roster,
	}); err != nil {
		return err
	}
	if err := b.say(ctx, trip.GroupID, change.Summary+" Everyone in the group needs to vote.", nil); err != nil {
		return err
	}
	return b.sendPoll(ctx, trip.GroupID, change.Summary, []string{"Yes — change it", "No — keep it"}, "change", trip.ID)
}

func (b *Brain) handleChangeVote(ctx context.Context, trip *models.Trip, vote models.PollVote, selected string) error {
	pc := trip.PendingChange
	if pc == nil {
		return nil
	}
	hosts := hostIDs(vote.AgentID, vote.AgentIDs...)
	if isHost(vote.VoterID, hosts) {
		return nil
	}
	if pc.Votes == nil {
		pc.Votes = map[string]string{}
	}
	needed := make([]string, 0, len(pc.Needed))
	for _, id := range pc.Needed {
		if !isHost(id, hosts) {
			needed = append(needed, id)
		}
	}
	pc.Needed = needed
	key := voteKey(pc.Needed, vote.VoterID)
	if selected == "" {
		delete(pc.Votes, key)
		_, err := b.Store.UpdateTrip(ctx, trip.ID, map[string]any{"pending_change": pc})
		return err
	}
	low := strings.ToLower(selected)
	if strings.Contains(low, "yes") || strings.Contains(low, "change it") {
		pc.Votes[key] = "yes"
	} else if strings.Contains(low, "no") || strings.Contains(low, "keep") {
		pc.Votes[key] = "no"
	} else {
		return nil
	}
	if anyoneVotedNo(pc) {
		if _, err := b.Store.UpdateTrip(ctx, trip.ID, map[string]any{"pending_change": (*models.PendingChange)(nil)}); err != nil {
			return err
		}
		return b.say(ctx, trip.GroupID, "Leaving it as is.", nil)
	}
	if !allVotedYes(pc) {
		_, err := b.Store.UpdateTrip(ctx, trip.ID, map[string]any{"pending_change": pc})
		return err
	}
	return b.applyPendingChange(ctx, trip, vote)
}

func (b *Brain) applyPendingChange(ctx context.Context, trip *models.Trip, vote models.PollVote) error {
	pc := trip.PendingChange
	if pc == nil {
		return nil
	}
	incoming := models.IncomingMessage{
		GroupID: vote.GroupID, GroupName: vote.GroupName,
		SenderID: vote.VoterID, SenderName: vote.VoterName,
		Text: pc.Summary, Tagged: true, Timestamp: vote.Timestamp,
		AgentID: vote.AgentID, Participants: trip.Roster,
	}
	fields := map[string]any{"pending_change": (*models.PendingChange)(nil)}
	feedback := pc.Summary
	switch pc.Kind {
	case "reopen":
		incoming.Text = strings.TrimSpace(pc.Destination + " " + pc.Summary)
		if _, err := b.Store.UpdateTrip(ctx, trip.ID, map[string]any{"pending_change": (*models.PendingChange)(nil)}); err != nil {
			return err
		}
		trip.PendingChange = nil
		return b.reopenAndPlan(ctx, trip, incoming)
	case "dates":
		people := applyHarvestForced(trip.Participants, harvestedFacts{Dates: pc.Dates}, trip.Roster)
		fields["participants"] = people
		fields["asked_dates"] = false
		if len(pc.Dates) > 0 {
			fields["embarking_date"] = pc.Dates[0]
			fields["returning_date"] = pc.Dates[len(pc.Dates)-1]
		}
		feedback = "The group agreed to the new dates. Use only these dates."
	case "origin":
		people := applyHarvestForced(trip.Participants, harvestedFacts{Airport: pc.Airport, City: pc.City}, trip.Roster)
		fields["participants"] = people
		fields["asked_origin"] = false
		if pc.City != "" {
			fields["origin"] = pc.City
		} else {
			fields["origin"] = pc.Airport
		}
		feedback = "The group agreed to fly from " + fields["origin"].(string) + "."
	case "destination":
		fields["destination"] = pc.Destination
		feedback = "The group agreed the destination is " + pc.Destination + ". Propose options for that city."
	case "budget":
		fields["budget_note"] = pc.Budget
		feedback = "The group agreed on a budget of " + pc.Budget + " CAD each."
	case "flights":
		people := trip.Participants
		direct := pc.Direct != nil && *pc.Direct
		for i := range people {
			people[i].FlightPreferences.IsDirect = direct
		}
		fields["participants"] = people
		fields["flights_locked"] = true
		if _, err := b.Store.UpdateTrip(ctx, trip.ID, fields); err != nil {
			return err
		}
		return b.say(ctx, trip.GroupID, "Got it — flights updated.", nil)
	}
	updated, err := b.Store.UpdateTrip(ctx, trip.ID, fields)
	if err != nil {
		return err
	}
	if err := b.say(ctx, trip.GroupID, "Everyone's in. Updating the plan.", nil); err != nil {
		return err
	}
	return b.plan(ctx, updated, feedback, incoming)
}

func (b *Brain) reopenAndPlan(ctx context.Context, trip *models.Trip, m models.IncomingMessage) error {
	oldDest := trip.Destination
	history, err := b.sessionHistory(ctx, trip)
	if err != nil {
		return err
	}
	cut := b.replanCutover(historySince(history, trip.HistoryStart), m)
	recent := historySince(history, &cut)
	h := harvestFacts(recent, b.today())
	h = mergeHarvest(h, harvestText(m.Text, b.today()))

	clear := map[string]any{
		"destination":            "",
		"destination_airport":    "",
		"options":                []models.Option{},
		"itinerary":              map[string]any{},
		"flights":                []models.Flight{},
		"accommodations":         []models.Accommodation{},
		"chosen_option_position": 0,
		"embarking_date":         "",
		"returning_date":         "",
		"approved_by":            "",
		"pending_change":         (*models.PendingChange)(nil),
		"history_start":          cut,
		"asked_dates":            false,
		"asked_origin":           false,
		"introduced":             false,
	}
	if h.Destination != "" {
		clear["destination"] = h.Destination
	}
	people := trip.Participants
	if len(h.Dates) > 0 {
		people = applyHarvestForced(people, harvestedFacts{Dates: h.Dates, Nights: h.Nights}, trip.Roster)
		clear["participants"] = people
		clear["embarking_date"] = h.Dates[0]
		clear["returning_date"] = h.Dates[len(h.Dates)-1]
	}
	switch trip.State {
	case models.Booked, models.Cancelled:
		trip, err = store.SetState(ctx, b.Store, trip.ID, models.Collecting, clear)
	default:
		trip, err = b.Store.UpdateTrip(ctx, trip.ID, clear)
	}
	if err != nil {
		return err
	}

	ack := "Okay, that booking's off."
	if oldDest != "" && h.Destination != "" {
		ack = fmt.Sprintf("Got it — %s is cancelled. Looking at %s.", oldDest, h.Destination)
	} else if h.Destination != "" {
		ack = "Got it. Looking at " + h.Destination + "."
	}
	if err := b.say(ctx, trip.GroupID, ack, nil); err != nil {
		return err
	}
	feedback := "The previous booking is cancelled and must not be reused. Plan this new trip from the latest request: " + m.Text
	if h.Destination != "" {
		feedback += " Destination is " + h.Destination + "."
	}
	if len(h.Dates) > 0 {
		feedback += " Dates: " + strings.Join(h.Dates, ", ") + "."
	}
	return b.plan(ctx, trip, feedback, m)
}

func (b *Brain) replanCutover(messages []models.Message, m models.IncomingMessage) time.Time {
	fallback := models.Now().Add(-2 * time.Second)
	if m.Timestamp != 0 {
		fallback = time.Unix(m.Timestamp, 0).UTC().Add(-2 * time.Second)
	}
	var first time.Time
	found := false
	for _, msg := range messages {
		if !msg.IsBot && (looksLikeReplan(msg.Text) || looksLikeCancelBooking(msg.Text)) {
			if !found || msg.SentAt.Before(first) {
				first = msg.SentAt
				found = true
			}
		}
	}
	if found {
		return first.Add(-time.Second)
	}
	return fallback
}
