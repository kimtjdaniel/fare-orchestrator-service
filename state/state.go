// Package state implements the trip state machine. The ONLY place that decides which stage can
// follow which.
//
// Rule: code moves the trip between states, never Gemini. Gemini reads and writes text; this file
// guarantees nothing gets booked without a ✅.
package state

import (
	"fmt"

	"fare-brain/models"
)

type InvalidTransitionError struct {
	msg string
}

func (e *InvalidTransitionError) Error() string { return e.msg }

var Allowed = map[models.TripState]map[models.TripState]bool{
	models.Collecting:       {models.AwaitingChoice: true, models.Cancelled: true},
	models.AwaitingChoice:   {models.Searching: true, models.AwaitingChoice: true, models.Cancelled: true},        // re-propose = stay
	models.Searching:        {models.AwaitingApproval: true, models.AwaitingChoice: true, models.Cancelled: true}, // back if search fails
	models.AwaitingApproval: {models.BookingState: true, models.AwaitingChoice: true, models.Cancelled: true},     // ❌ = back to options
	models.BookingState:     {models.Booked: true, models.AwaitingApproval: true},                                 // failure = ✅ again to retry
	// Singleton-per-group trips are reused across cycles instead of creating a new document, so a
	// terminal trip can start over: the group gets @mentioned again -> back to Collecting.
	models.Booked:    {models.Collecting: true},
	models.Cancelled: {models.Collecting: true},
}

func CheckTransition(current, new models.TripState) error {
	if !Allowed[current][new] {
		return &InvalidTransitionError{fmt.Sprintf("%s -> %s is not allowed", current, new)}
	}
	return nil
}

// AssertCanBook is called at the top of every booking function. Belt and braces for the
// approval gate.
func AssertCanBook(trip *models.Trip) error {
	if trip.State != models.BookingState || trip.ApprovedBy == "" {
		return &InvalidTransitionError{fmt.Sprintf(
			"refusing to book trip %s: state=%s, approved_by=%q", trip.ID, trip.State, trip.ApprovedBy)}
	}
	return nil
}
