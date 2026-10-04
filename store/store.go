// Package store implements persistence behind one interface.
//
// MemoryStore — used when MONGODB_URI is empty (solo dev, tests). Lost on restart.
// MongoStore  — MongoDB. Same methods, same return types. Each trip is a SINGLETON per WhatsApp
// group (document _id == group_id): there's no history of past trips, so a finished trip's
// document is reset and reused rather than a new one created. Participants, options, flights and
// accommodations are embedded inside the trip document; messages are their own collection.
//
// Every state change goes through SetState, which enforces the state machine.
package store

import (
	"context"
	"fmt"
	"time"

	"fare-brain/models"
	"fare-brain/state"
)

type Store interface {
	Connect(ctx context.Context) error
	Close(ctx context.Context) error

	// SaveMessage returns false if this external_id was already saved (duplicate delivery).
	SaveMessage(ctx context.Context, msg *models.Message) (bool, error)
	GetMessages(ctx context.Context, groupID string, since *time.Time, limit int, includeBot bool) ([]models.Message, error)

	// CreateTrip gets-or-creates the group's singleton trip document.
	CreateTrip(ctx context.Context, groupID, groupName string) (*models.Trip, error)
	GetTrip(ctx context.Context, tripID string) (*models.Trip, error)
	ListTrips(ctx context.Context) ([]*models.Trip, error)
	// ResetTrip reuses the same document for a new trip cycle: clears the prior round's fields and
	// moves back to Collecting, so old chat history doesn't leak into the new extraction.
	ResetTrip(ctx context.Context, tripID, groupName string) (*models.Trip, error)
	UpdateTrip(ctx context.Context, tripID string, fields map[string]any) (*models.Trip, error)

	GetWhatsAppSession(ctx context.Context, id string) (*models.WhatsAppSession, error)
	SaveWhatsAppSession(ctx context.Context, id string, data map[string]any) (*models.WhatsAppSession, error)
}

// SetState validates the transition then persists it. Shared by every Store implementation since
// Go has no base class to hang this on the way store.py's Store ABC did.
func SetState(ctx context.Context, s Store, tripID string, newState models.TripState, fields map[string]any) (*models.Trip, error) {
	trip, err := s.GetTrip(ctx, tripID)
	if err != nil {
		return nil, err
	}
	if trip == nil {
		return nil, fmt.Errorf("trip %s not found", tripID)
	}
	if err := state.CheckTransition(trip.State, newState); err != nil {
		return nil, err
	}
	merged := map[string]any{"state": newState}
	for k, v := range fields {
		merged[k] = v
	}
	return s.UpdateTrip(ctx, tripID, merged)
}

// TripView assembles everything the dashboard needs in one JSON-able blob.
func TripView(ctx context.Context, s Store, tripID string) (map[string]any, error) {
	trip, err := s.GetTrip(ctx, tripID)
	if err != nil {
		return nil, err
	}
	if trip == nil {
		return nil, nil
	}
	return map[string]any{
		"trip":           trip,
		"participants":   trip.Participants,
		"options":        trip.Options,
		"flights":        trip.Flights,
		"accommodations": trip.Accommodations,
	}, nil
}

// allowedTripFields whitelists what UpdateTrip may change.
var allowedTripFields = map[string]bool{
	"group_name": true, "state": true, "chosen_option_position": true, "itinerary": true,
	"approved_by": true, "approved_at": true, "pending_approver": true, "history_start": true, "options": true,
	"participants": true, "flights": true, "accommodations": true,
	"origin": true, "destination": true, "destination_airport": true,
	"activity_description": true, "culinary_description": true, "duration_nights": true,
	"cost_per_person": true, "embarking_date": true, "returning_date": true,
	"asked_origin": true, "asked_dates": true, "asked_payer": true, "payer_name": true,
	"introduced":       true,
	"shared_dashboard": true, "last_poll": true,
	"roster": true, "budget_note": true, "flights_locked": true, "pending_change": true,
	"pending_trip_request": true,
	"organizer_wa_id": true, "intake": true, "pending_question": true, "conflicts": true,
	"intake_polls": true, "last_agent_text": true,
}

// applyTripFields mutates trip in place from a whitelisted fields map, shared by MemoryStore and
// MongoStore so the field-set logic (and its validation) lives in exactly one place.
func applyTripFields(trip *models.Trip, fields map[string]any) error {
	for k, v := range fields {
		if !allowedTripFields[k] {
			return fmt.Errorf("unknown trip field: %s", k)
		}
		switch k {
		case "group_name":
			trip.GroupName, _ = v.(string)
		case "state":
			trip.State, _ = v.(models.TripState)
		case "chosen_option_position":
			switch n := v.(type) {
			case int:
				trip.ChosenOptionPosition = n
			case int32:
				trip.ChosenOptionPosition = int(n)
			case int64:
				trip.ChosenOptionPosition = int(n)
			}
		case "itinerary":
			trip.Itinerary, _ = v.(map[string]any)
		case "approved_by":
			trip.ApprovedBy, _ = v.(string)
		case "pending_approver":
			trip.PendingApprover, _ = v.(string)
		case "approved_at":
			if t, ok := v.(time.Time); ok {
				trip.ApprovedAt = &t
			}
		case "history_start":
			if t, ok := v.(*time.Time); ok {
				trip.HistoryStart = t
			} else if t, ok := v.(time.Time); ok {
				trip.HistoryStart = &t
			}
		case "options":
			trip.Options, _ = v.([]models.Option)
		case "participants":
			trip.Participants, _ = v.([]models.Participant)
		case "flights":
			trip.Flights, _ = v.([]models.Flight)
		case "accommodations":
			trip.Accommodations, _ = v.([]models.Accommodation)
		case "origin":
			trip.Origin, _ = v.(string)
		case "destination":
			trip.Destination, _ = v.(string)
		case "destination_airport":
			trip.DestinationAirport, _ = v.(string)
		case "activity_description":
			trip.ActivityDescription, _ = v.(string)
		case "culinary_description":
			trip.CulinaryDescription, _ = v.(string)
		case "duration_nights":
			switch n := v.(type) {
			case int:
				trip.DurationNights = n
			case int32:
				trip.DurationNights = int(n)
			case int64:
				trip.DurationNights = int(n)
			}
		case "cost_per_person":
			if f, ok := v.(float64); ok {
				trip.CostPerPerson = &f
			}
		case "embarking_date":
			trip.EmbarkingDate, _ = v.(string)
		case "returning_date":
			trip.ReturningDate, _ = v.(string)
		case "asked_origin":
			trip.AskedOrigin, _ = v.(bool)
		case "asked_dates":
			trip.AskedDates, _ = v.(bool)
		case "asked_payer":
			trip.AskedPayer, _ = v.(bool)
		case "payer_name":
			trip.PayerName, _ = v.(string)
		case "introduced":
			trip.Introduced, _ = v.(bool)
		case "shared_dashboard":
			trip.SharedDashboard, _ = v.(bool)
		case "last_poll":
			trip.LastPoll, _ = v.(string)
		case "roster":
			trip.Roster, _ = v.([]models.GroupMember)
		case "budget_note":
			trip.BudgetNote, _ = v.(string)
		case "flights_locked":
			trip.FlightsLocked, _ = v.(bool)
		case "pending_trip_request":
			trip.PendingTripRequest, _ = v.(*models.PendingTripRequest)
		case "pending_change":
			switch pc := v.(type) {
			case nil:
				trip.PendingChange = nil
			case *models.PendingChange:
				trip.PendingChange = pc
			case models.PendingChange:
				cp := pc
				trip.PendingChange = &cp
			}
		case "organizer_wa_id":
			trip.OrganizerWaID, _ = v.(string)
		case "intake":
			switch iv := v.(type) {
			case nil:
				trip.Intake = nil
			case *models.TripIntake:
				trip.Intake = iv
			case models.TripIntake:
				cp := iv
				trip.Intake = &cp
			}
		case "pending_question":
			switch pq := v.(type) {
			case nil:
				trip.PendingQuestion = nil
			case *models.PendingQuestion:
				trip.PendingQuestion = pq
			case models.PendingQuestion:
				cp := pq
				trip.PendingQuestion = &cp
			}
		case "conflicts":
			trip.Conflicts, _ = v.([]models.Conflict)
		case "intake_polls":
			trip.IntakePolls, _ = v.([]models.IntakePoll)
		case "last_agent_text":
			trip.LastAgentText, _ = v.(string)
		}
	}
	trip.UpdatedAt = models.Now()
	return nil
}

// resetTrip clears a document in place for a new trip cycle (same group, no history of the old
// one kept). Shared by MemoryStore and MongoStore, mirroring applyTripFields's role.
func resetTrip(trip *models.Trip, groupName string) {
	now := models.Now()
	*trip = models.Trip{
		ID: trip.ID, GroupID: trip.GroupID, GroupName: groupName, State: models.Collecting,
		Participants: []models.Participant{}, Options: []models.Option{},
		Flights: []models.Flight{}, Accommodations: []models.Accommodation{},
		HistoryStart: &now, CreatedAt: trip.CreatedAt, UpdatedAt: now,
	}
}
