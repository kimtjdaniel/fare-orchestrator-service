// Package models holds the data shapes shared by every package. If you change one, update
// CONTRACTS.md too.
package models

import (
	"encoding/json"
	"time"
)

func Now() time.Time {
	return time.Now().UTC()
}

// TripState drives the orchestration state machine (state/state.go is the only place allowed to
// move a trip between these). It's richer than the Mongo schema's own booking_status concept
// (below) because the group-chat flow needs the finer stages (proposing, awaiting a choice,
// searching, awaiting approval) to actually sequence the conversation.
type TripState string

const (
	Collecting       TripState = "COLLECTING"        // listening, no plan yet
	AwaitingChoice   TripState = "AWAITING_CHOICE"   // options posted, waiting for "1/2/3"
	Searching        TripState = "SEARCHING"         // transient: pulling flights + hotel
	AwaitingApproval TripState = "AWAITING_APPROVAL" // summary posted, waiting for ✅ / ❌
	BookingState     TripState = "BOOKING"           // approved; flights + hotel in progress
	Booked           TripState = "BOOKED"            // done (terminal)
	Cancelled        TripState = "CANCELLED"         // abandoned (terminal)
)

// ActiveStates are every state except the two terminal ones.
var ActiveStates = map[TripState]bool{
	Collecting:       true,
	AwaitingChoice:   true,
	Searching:        true,
	AwaitingApproval: true,
	BookingState:     true,
}

func (s TripState) Active() bool { return ActiveStates[s] }

// BookingStatus is the simple per-item status for a Flight or Accommodation sub-document
// (the "enum booking_status (e.g. collecting, incomplete, booked)" from the schema outline).
type BookingStatus string

const (
	StatusCollecting BookingStatus = "collecting" // not yet searched/selected
	StatusIncomplete BookingStatus = "incomplete" // selected or booking attempted, not confirmed
	StatusBooked     BookingStatus = "booked"     // confirmed
)

// ---------- incoming (from P2's messaging layer) ----------

// IncomingMessage is what the WhatsApp robot / Telegram adapter POSTs to /webhook.
// CONTRACTS.md is flat (group_id, sender_id). Older robot builds sent nested
// chat/sender objects; UnmarshalJSON accepts both.
type IncomingMessage struct {
	GroupID      string         `json:"group_id"`
	GroupName    string         `json:"group_name"`
	SenderID     string         `json:"sender_id"`
	SenderName   string         `json:"sender_name"`
	Participants []GroupMember  `json:"participants,omitempty"`
	Text         string         `json:"text"`
	Tagged       bool           `json:"tagged"`    // was the bot @mentioned?
	Timestamp    int64          `json:"timestamp"` // unix seconds
	MessageID    string         `json:"message_id,omitempty"`
	AgentID      string         `json:"agent_id,omitempty"`
	Quoted       *QuotedMessage `json:"quoted,omitempty"`
	CoAskers     []string       `json:"-"` // other people in a batched @mention burst
}

// QuotedMessage is the WhatsApp message this inbound line is replying to.
type QuotedMessage struct {
	ID       string `json:"id"`
	SenderID string `json:"sender_id"`
	Text     string `json:"text"`
	FromMe   bool   `json:"from_me"`
}

// GroupMember is one WhatsApp group participant as reported by the robot.
type GroupMember struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	IsAdmin bool   `json:"is_admin,omitempty"`
	IsAgent bool   `json:"is_agent,omitempty"`
}

// ---------- deterministic intake (WhatsApp Agent Flow Spec §3-§5) ----------
//
// This is additive, parallel data alongside the existing flat Trip/Participant fields (Origin,
// DestinationAirport, etc.) that the AwaitingChoice-onward pipeline already reads — nothing here
// replaces those. The intake turn controller (orchestrator/intake.go) is the only thing that
// reads/writes this block; it owns the trip only while State == Collecting, and copies its
// conclusions onto the existing flat fields once it hands off to the existing pipeline.

// Confidence tracks how sure extraction is about a field: stated outright (confirmed), implied or
// defaulted (inferred), or not yet known. Spec §4.1: inferred fields are good enough to not
// re-ask, but get surfaced in the readiness summary so people can correct them.
type Confidence string

const (
	Confirmed Confidence = "confirmed"
	Inferred  Confidence = "inferred"
	Unknown   Confidence = "unknown"
)

// FieldValue is one intake fact with provenance. Value is left as `any` because the fields it
// backs vary in shape (a string, a number, a DateWindow, …); callers type-assert.
type FieldValue struct {
	Value      any        `json:"value,omitempty" bson:"value,omitempty"`
	Confidence Confidence `json:"confidence" bson:"confidence"`
	SourceID   string     `json:"source_id,omitempty" bson:"source_id,omitempty"` // a message_id or poll_id
	UpdatedAt  time.Time  `json:"updated_at,omitempty" bson:"updated_at,omitempty"`
}

func (f FieldValue) Known() bool { return f.Confidence != "" && f.Confidence != Unknown }

func (f FieldValue) AsString() string {
	s, _ := f.Value.(string)
	return s
}

// DateWindow is an inclusive earliest/latest range (ISO YYYY-MM-DD), used for the intake
// field.date_window before exact dates are nailed down to a single pair.
type DateWindow struct {
	Earliest string `json:"earliest,omitempty" bson:"earliest,omitempty"`
	Latest   string `json:"latest,omitempty" bson:"latest,omitempty"`
}

// NightsRange is a min/max trip length in nights.
type NightsRange struct {
	Min int `json:"min,omitempty" bson:"min,omitempty"`
	Max int `json:"max,omitempty" bson:"max,omitempty"`
}

// ExactDates is a single confirmed depart/return pair, the thing a date poll resolves to.
type ExactDates struct {
	Depart string `json:"depart,omitempty" bson:"depart,omitempty"`
	Return string `json:"return,omitempty" bson:"return,omitempty"`
}

// BudgetPP is per-person budget, resolved from a poll range or a stated number.
type BudgetPP struct {
	Min      float64  `json:"min,omitempty" bson:"min,omitempty"`
	Max      float64  `json:"max,omitempty" bson:"max,omitempty"`
	Currency string   `json:"currency,omitempty" bson:"currency,omitempty"`
	Includes []string `json:"includes,omitempty" bson:"includes,omitempty"` // "flights","stay","food","activities"
}

// DateRange is one date span a participant said they're available for (spec §3's
// participants[].available).
type DateRange struct {
	Start    string `json:"start" bson:"start"`
	End      string `json:"end" bson:"end"`
	SourceID string `json:"source_id,omitempty" bson:"source_id,omitempty"`
}

// TripIntake is the trip-level half of spec §3's intake block. Per-person facts (origin,
// availability, budget) live on ParticipantIntake instead — TripIntake.Destination/Vibe/
// Constraints are the only fields that are inherently trip-wide rather than derived from people.
type TripIntake struct {
	Headcount   FieldValue  `json:"headcount,omitempty" bson:"headcount,omitempty"`
	Children    FieldValue  `json:"children,omitempty" bson:"children,omitempty"`
	DateWindow  *DateWindow `json:"date_window,omitempty" bson:"date_window,omitempty"`
	Nights      NightsRange `json:"nights,omitempty" bson:"nights,omitempty"`
	ExactDates  *ExactDates `json:"exact_dates,omitempty" bson:"exact_dates,omitempty"`
	BudgetPP    *BudgetPP   `json:"budget_pp,omitempty" bson:"budget_pp,omitempty"`
	Vibe        FieldValue  `json:"vibe,omitempty" bson:"vibe,omitempty"`
	Destination FieldValue  `json:"destination,omitempty" bson:"destination,omitempty"`
	Constraints FieldValue  `json:"constraints,omitempty" bson:"constraints,omitempty"`
}

// ParticipantIntake is the per-person half of spec §3's participants[] shape. Embedded as a
// pointer on Participant so trips created before this spec (nil) don't need a migration.
type ParticipantIntake struct {
	Attendance  string      `json:"attendance,omitempty" bson:"attendance,omitempty"` // coming|maybe|not_coming|unknown
	Origin      FieldValue  `json:"origin,omitempty" bson:"origin,omitempty"`
	Available   []DateRange `json:"available,omitempty" bson:"available,omitempty"`
	BudgetPP    FieldValue  `json:"budget_pp,omitempty" bson:"budget_pp,omitempty"`
	Constraints []string    `json:"constraints,omitempty" bson:"constraints,omitempty"`
}

// PendingQuestion is the one outstanding ask the turn controller is waiting on (spec §3/§4.2:
// never stack two questions). AskedTo is "group" or a specific wa_id.
type PendingQuestion struct {
	Field   string    `json:"field" bson:"field"`
	AskedAt time.Time `json:"asked_at" bson:"asked_at"`
	PollID  string    `json:"poll_id,omitempty" bson:"poll_id,omitempty"`
	AskedTo string    `json:"asked_to" bson:"asked_to"`
}

// Conflict is one detected contradiction the turn controller is surfacing to the group
// (spec §4.4) rather than silently resolving.
type Conflict struct {
	Field       string `json:"field" bson:"field"`
	Description string `json:"description" bson:"description"`
	Resolved    bool   `json:"resolved" bson:"resolved"`
}

// IntakePoll is one poll the intake turn controller posted, tracked so it can tell when every
// expected voter has answered (spec §4.3) without re-deriving that from the robot each time.
type IntakePoll struct {
	PollMessageID  string              `json:"poll_message_id" bson:"poll_message_id"`
	Field          string              `json:"field" bson:"field"`
	Options        []string            `json:"options" bson:"options"`
	Multi          bool                `json:"multi" bson:"multi"`
	ExpectedVoters []string            `json:"expected_voters,omitempty" bson:"expected_voters,omitempty"`
	Votes          map[string][]string `json:"votes" bson:"votes"` // voter_id -> selected option labels
	CreatedAt      time.Time           `json:"created_at" bson:"created_at"`
	Closed         bool                `json:"closed" bson:"closed"`
}

func (m *IncomingMessage) UnmarshalJSON(data []byte) error {
	type flat IncomingMessage
	var f flat
	if err := json.Unmarshal(data, &f); err != nil {
		return err
	}
	*m = IncomingMessage(f)
	if m.GroupID != "" && m.SenderID != "" {
		return nil
	}
	var nested struct {
		Chat *struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"chat"`
		Sender *struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"sender"`
	}
	if err := json.Unmarshal(data, &nested); err != nil {
		return err
	}
	if m.GroupID == "" && nested.Chat != nil {
		m.GroupID = nested.Chat.ID
		if m.GroupName == "" {
			m.GroupName = nested.Chat.Name
		}
	}
	if m.SenderID == "" && nested.Sender != nil {
		m.SenderID = nested.Sender.ID
		if m.SenderName == "" {
			m.SenderName = nested.Sender.Name
		}
	}
	return nil
}

// ---------- stored records ----------

type Message struct {
	ID         string    `json:"id,omitempty" bson:"_id,omitempty"`
	GroupID    string    `json:"group_id" bson:"group_id"`
	TripID     string    `json:"trip_id,omitempty" bson:"trip_id,omitempty"`
	ExternalID string    `json:"external_id,omitempty" bson:"external_id,omitempty"`
	SenderID   string    `json:"sender_id" bson:"sender_id"`
	SenderName string    `json:"sender_name" bson:"sender_name"`
	Text       string    `json:"text" bson:"text"`
	Tagged     bool      `json:"tagged" bson:"tagged"`
	IsBot      bool      `json:"is_bot" bson:"is_bot"`
	SentAt     time.Time `json:"sent_at" bson:"sent_at"`
}

func ParseDate(s string) (time.Time, error) {
	return time.Parse("2006-01-02", s)
}

// FlightClass is the participant's preferred cabin.
type FlightClass string

const (
	Economy     FlightClass = "economy"
	EconomyPlus FlightClass = "economy_plus"
	Business    FlightClass = "business"
)

// AccommodationType is the participant's preferred lodging type.
type AccommodationType string

const (
	HotelType  AccommodationType = "hotel"
	AirBnBType AccommodationType = "airbnb"
	HostelType AccommodationType = "hostel"
)

// GeneralPreferences mirrors the schema outline's "struct general_preferences".
type GeneralPreferences struct {
	DurationNights int      `json:"duration_nights,omitempty" bson:"duration_nights,omitempty"`
	Availability   []string `json:"availability" bson:"availability"` // every "YYYY-MM-DD" they're free, not ranges
	Activity       string   `json:"activity,omitempty" bson:"activity,omitempty"`
	Culinary       string   `json:"culinary,omitempty" bson:"culinary,omitempty"` // whitelist, not allergies
}

// FlightPreferences mirrors the schema outline's "struct flight_preferences".
type FlightPreferences struct {
	IsDirect bool        `json:"is_direct" bson:"is_direct"`
	Class    FlightClass `json:"class,omitempty" bson:"class,omitempty"`
}

// AccommodationPreferences mirrors the schema outline's "struct accomodation_preferences".
type AccommodationPreferences struct {
	Type   AccommodationType `json:"type,omitempty" bson:"type,omitempty"`
	Rating *float64          `json:"rating,omitempty" bson:"rating,omitempty"`
}

// Participant is one person's preferences: the shape Gemini fills via record_preferences. Payment
// and passport data live in the 1Password vault, never here — only whether they've uploaded it.
type Participant struct {
	PID                  string `json:"pid" bson:"pid"`
	WaID                 string `json:"wa_id,omitempty" bson:"wa_id,omitempty"` // stable WhatsApp id, when known — the intake turn controller's preferred match key over WhatsAppName
	WhatsAppName         string `json:"whatsapp_name" bson:"whatsapp_name"`
	Payer                bool   `json:"payer" bson:"payer"`
	UploadedPaymentInfo  bool   `json:"uploaded_payment_info" bson:"uploaded_payment_info"`
	UploadedPassportInfo bool   `json:"uploaded_passport_info" bson:"uploaded_passport_info"`
	OriginCity           string `json:"origin_city,omitempty" bson:"origin_city,omitempty"`
	OriginAirport        string `json:"origin_airport,omitempty" bson:"origin_airport,omitempty"` // IATA
	Origin               string `json:"origin,omitempty" bson:"origin,omitempty"`                 // schema: same origin for now; city or IATA

	LegalName      string `json:"legal_name,omitempty" bson:"legal_name,omitempty"`
	DateOfBirth    string `json:"date_of_birth,omitempty" bson:"date_of_birth,omitempty"` // YYYY-MM-DD
	PassportNumber string `json:"passport_number,omitempty" bson:"passport_number,omitempty"`

	GeneralPreferences       GeneralPreferences       `json:"general_preferences" bson:"general_preferences"`
	FlightPreferences        FlightPreferences        `json:"flight_preferences" bson:"flight_preferences"`
	AccommodationPreferences AccommodationPreferences `json:"accommodation_preferences" bson:"accommodation_preferences"`

	// Intake is the confidence-tracked per-person intake data (spec §3). Nil for trips/participants
	// that predate this, or that never went through the deterministic intake flow.
	Intake *ParticipantIntake `json:"intake,omitempty" bson:"intake,omitempty"`
}

// Option is one proposed destination, shown during the vote. Options are never independently
// persisted: they live only as the current round attached to Trip.Options. Once the group picks
// one, its fields are copied onto the Trip's own flat fields (below) — that's the "singleton trip"
// shape from the schema outline.
type Option struct {
	Position            int      `json:"position" bson:"position"`
	Destination         string   `json:"destination" bson:"destination"`
	DestinationAirport  string   `json:"destination_airport" bson:"destination_airport"`
	ActivityDescription string   `json:"activity_description" bson:"activity_description"`
	CulinaryDescription string   `json:"culinary_description" bson:"culinary_description"`
	DurationNights      int      `json:"duration_nights" bson:"duration_nights"`
	CostPerPerson       *float64 `json:"cost_per_person,omitempty" bson:"cost_per_person,omitempty"`
	EmbarkingDate       string   `json:"embarking_date" bson:"embarking_date"` // YYYY-MM-DD
	ReturningDate       string   `json:"returning_date" bson:"returning_date"`
	WhyItWorks          string   `json:"why_it_works" bson:"why_it_works"`
	Tradeoffs           string   `json:"tradeoffs" bson:"tradeoffs"`
	Chosen              bool     `json:"chosen" bson:"chosen"`
}

// FlightDirection distinguishes the outbound leg from the return leg of the group's shared
// itinerary (everyone currently flies from the same origin — see Participant.OriginAirport).
type FlightDirection string

const (
	Embarking FlightDirection = "embarking"
	Returning FlightDirection = "returning"
)

// Flight is one leg (embarking or returning) of the group's shared flight itinerary.
type Flight struct {
	Source        string          `json:"source,omitempty" bson:"source,omitempty"`
	Direction     FlightDirection `json:"direction" bson:"direction"`
	BookingStatus BookingStatus   `json:"booking_status" bson:"booking_status"`
	DepartingDate time.Time       `json:"departing_date" bson:"departing_date"`
	ArrivalDate   time.Time       `json:"arrival_date" bson:"arrival_date"`
	Costs         *float64        `json:"costs,omitempty" bson:"costs,omitempty"`
	BookingURL    string          `json:"booking_url,omitempty" bson:"booking_url,omitempty"`
	ProviderRef   string          `json:"provider_ref,omitempty" bson:"provider_ref,omitempty"` // PNR once booked
}

// Accommodation is the group's shared stay.
type Accommodation struct {
	Source              string        `json:"source,omitempty" bson:"source,omitempty"`
	PropertyType        string        `json:"property_type,omitempty" bson:"property_type,omitempty"`
	OriginalRating      *float64      `json:"original_rating,omitempty" bson:"original_rating,omitempty"`
	OriginalRatingScale *float64      `json:"original_rating_scale,omitempty" bson:"original_rating_scale,omitempty"`
	PriceNote           string        `json:"price_note,omitempty" bson:"price_note,omitempty"`
	BookingStatus       BookingStatus `json:"booking_status" bson:"booking_status"`
	CheckInDate         time.Time     `json:"check_in_date" bson:"check_in_date"`
	CheckOutDate        time.Time     `json:"check_out_date" bson:"check_out_date"`
	Rating              *float64      `json:"rating,omitempty" bson:"rating,omitempty"`
	Costs               *float64      `json:"costs,omitempty" bson:"costs,omitempty"`
	BookingURL          string        `json:"booking_url,omitempty" bson:"booking_url,omitempty"`
	ProviderRef         string        `json:"provider_ref,omitempty" bson:"provider_ref,omitempty"` // confirmation number once booked
}

// Trip is a SINGLETON per WhatsApp group: ID == GroupID, one document reused for every trip cycle
// that group runs (no history of past trips). When a Booked/Cancelled trip's group gets
// @mentioned again, the same document resets back to Collecting (see store.ResetTrip).
type Trip struct {
	ID        string    `json:"id" bson:"_id"` // == GroupID
	GroupID   string    `json:"group_id" bson:"group_id"`
	GroupName string    `json:"group_name" bson:"group_name"`
	State     TripState `json:"state" bson:"state"`

	Participants         []Participant `json:"participants" bson:"participants"`
	Options              []Option      `json:"options" bson:"options"`
	ChosenOptionPosition int           `json:"chosen_option_position,omitempty" bson:"chosen_option_position,omitempty"`

	// Flat scalar fields, matching the schema outline's SINGLETON trip shape. Origin is set once
	// participants are extracted (everyone shares one origin, for now); the rest are copied from
	// the chosen Option once the group votes.
	Origin              string   `json:"origin,omitempty" bson:"origin,omitempty"`
	Destination         string   `json:"destination,omitempty" bson:"destination,omitempty"`
	DestinationAirport  string   `json:"destination_airport,omitempty" bson:"destination_airport,omitempty"`
	ActivityDescription string   `json:"activity_description,omitempty" bson:"activity_description,omitempty"`
	CulinaryDescription string   `json:"culinary_description,omitempty" bson:"culinary_description,omitempty"`
	DurationNights      int      `json:"duration_nights,omitempty" bson:"duration_nights,omitempty"`
	CostPerPerson       *float64 `json:"cost_per_person,omitempty" bson:"cost_per_person,omitempty"`
	EmbarkingDate       string   `json:"embarking_date,omitempty" bson:"embarking_date,omitempty"`
	ReturningDate       string   `json:"returning_date,omitempty" bson:"returning_date,omitempty"`

	Flights        []Flight        `json:"flights,omitempty" bson:"flights,omitempty"`
	Accommodations []Accommodation `json:"accommodations,omitempty" bson:"accommodations,omitempty"`

	Itinerary  map[string]any `json:"itinerary,omitempty" bson:"itinerary,omitempty"`
	ApprovedBy string         `json:"approved_by,omitempty" bson:"approved_by,omitempty"`
	ApprovedAt *time.Time     `json:"approved_at,omitempty" bson:"approved_at,omitempty"`
	// PendingApprover holds the name of whoever voted/said "yes, book it" while no payer was
	// designated yet. Once a payer is picked, this is used to finish the booking they already approved.
	PendingApprover string         `json:"pending_approver,omitempty" bson:"pending_approver,omitempty"`
	HistoryStart    *time.Time     `json:"history_start,omitempty" bson:"history_start,omitempty"`
	AskedOrigin     bool           `json:"asked_origin" bson:"asked_origin"`
	AskedDates      bool           `json:"asked_dates" bson:"asked_dates"`
	AskedPayer      bool           `json:"asked_payer" bson:"asked_payer"`
	PayerName       string         `json:"payer_name,omitempty" bson:"payer_name,omitempty"`
	Introduced      bool           `json:"introduced" bson:"introduced"`
	SharedDashboard bool           `json:"shared_dashboard" bson:"shared_dashboard"`
	LastPoll        string         `json:"last_poll,omitempty" bson:"last_poll,omitempty"`
	Roster          []GroupMember  `json:"roster,omitempty" bson:"roster,omitempty"`
	BudgetNote      string         `json:"budget_note,omitempty" bson:"budget_note,omitempty"`
	FlightsLocked   bool           `json:"flights_locked" bson:"flights_locked"`
	PendingChange   *PendingChange `json:"pending_change,omitempty" bson:"pending_change,omitempty"`

	// Deterministic intake (spec §3-§4). Owned exclusively by the intake turn controller
	// (orchestrator/intake.go) while State == Collecting; untouched afterward.
	OrganizerWaID   string           `json:"organizer_wa_id,omitempty" bson:"organizer_wa_id,omitempty"`
	Intake          *TripIntake      `json:"intake,omitempty" bson:"intake,omitempty"`
	PendingQuestion *PendingQuestion `json:"pending_question,omitempty" bson:"pending_question,omitempty"`
	Conflicts       []Conflict       `json:"conflicts,omitempty" bson:"conflicts,omitempty"`
	IntakePolls     []IntakePoll     `json:"intake_polls,omitempty" bson:"intake_polls,omitempty"`
	LastAgentText   string           `json:"last_agent_text,omitempty" bson:"last_agent_text,omitempty"` // spec §6.2: never repeat the previous message verbatim

	CreatedAt time.Time `json:"created_at" bson:"created_at"`
	UpdatedAt time.Time `json:"updated_at" bson:"updated_at"`
}

// PendingChange is a proposed update to already-set trip details. It only applies
// after every human in the group votes yes on the poll.
type PendingChange struct {
	Kind        string            `json:"kind" bson:"kind"`
	Summary     string            `json:"summary" bson:"summary"`
	ProposedBy  string            `json:"proposed_by" bson:"proposed_by"`
	Dates       []string          `json:"dates,omitempty" bson:"dates,omitempty"`
	Airport     string            `json:"airport,omitempty" bson:"airport,omitempty"`
	City        string            `json:"city,omitempty" bson:"city,omitempty"`
	Destination string            `json:"destination,omitempty" bson:"destination,omitempty"`
	Budget      string            `json:"budget,omitempty" bson:"budget,omitempty"`
	Direct      *bool             `json:"direct,omitempty" bson:"direct,omitempty"`
	Needed      []string          `json:"needed" bson:"needed"`
	Votes       map[string]string `json:"votes" bson:"votes"`
}

// PollVote is posted by the WhatsApp robot when someone taps a poll option.
type PollVote struct {
	Event           string   `json:"event"`
	GroupID         string   `json:"group_id"`
	GroupName       string   `json:"group_name"`
	VoterID         string   `json:"voter_id"`
	VoterName       string   `json:"voter_name"`
	PollMessageID   string   `json:"poll_message_id"`
	PollName        string   `json:"poll_name"`
	SelectedOptions []string `json:"selected_options"`
	Timestamp       int64    `json:"timestamp"`
	AgentID         string   `json:"agent_id,omitempty"`
}

// ChosenOption reconstructs an Option-shaped view of the group's pick from the trip's own flat
// fields (the source of truth once a choice has been made).
func (t *Trip) ChosenOption() *Option {
	if t.Destination == "" {
		return nil
	}
	return &Option{
		Position: t.ChosenOptionPosition, Destination: t.Destination, DestinationAirport: t.DestinationAirport,
		ActivityDescription: t.ActivityDescription, CulinaryDescription: t.CulinaryDescription,
		DurationNights: t.DurationNights, CostPerPerson: t.CostPerPerson,
		EmbarkingDate: t.EmbarkingDate, ReturningDate: t.ReturningDate, Chosen: true,
	}
}

// WhatsAppSession stores the whatsapp-web.js robot's own login/auth session (so it doesn't need
// to re-scan a QR code on every restart) — "DB stores WhatsApp sessions" from the schema outline.
// This is the robot's session, not a per-user chat session.
type WhatsAppSession struct {
	ID        string         `json:"id" bson:"_id"` // fixed key, e.g. "robot"
	Data      map[string]any `json:"data" bson:"data"`
	UpdatedAt time.Time      `json:"updated_at" bson:"updated_at"`
}

// ---------- travel tool results (P3 / P4 return these) ----------

type FlightOffer struct {
	Source              string  `json:"source,omitempty"`
	BookingURL          string  `json:"booking_url,omitempty"`
	ReturnDuration      string  `json:"return_duration,omitempty"`
	ReturnStops         *int    `json:"return_stops,omitempty"`
	ReturnDepartureTime string  `json:"return_departure_time,omitempty"`
	ReturnArrivalTime   string  `json:"return_arrival_time,omitempty"`
	Duration            string  `json:"duration,omitempty"`
	Stops               int     `json:"stops"`
	DepartureTime       string  `json:"departure_time,omitempty"`
	ArrivalTime         string  `json:"arrival_time,omitempty"`
	OfferID             string  `json:"offer_id"`
	Origin              string  `json:"origin"`
	Destination         string  `json:"destination"`
	DepartDate          string  `json:"depart_date"`
	ReturnDate          string  `json:"return_date"`
	Airline             string  `json:"airline"`
	Price               float64 `json:"price"`
	Currency            string  `json:"currency"`
	Summary             string  `json:"summary"` // "AC 554 dep 08:10, arr 11:35"
}

// ReturningOffer preserves source/link provenance and uses return details when supplied.
// Google Flights retains the existing fallback until it exposes both legs.
func (offer FlightOffer) ReturningOffer() FlightOffer {
	leg := offer
	leg.Origin, leg.Destination = offer.Destination, offer.Origin
	if offer.ReturnDuration != "" {
		leg.Duration = offer.ReturnDuration
	}
	if offer.ReturnStops != nil {
		leg.Stops = *offer.ReturnStops
	}
	if offer.ReturnDepartureTime != "" {
		leg.DepartureTime = offer.ReturnDepartureTime
	}
	if offer.ReturnArrivalTime != "" {
		leg.ArrivalTime = offer.ReturnArrivalTime
	}
	if offer.ReturnDepartureTime != "" && offer.ReturnArrivalTime != "" {
		leg.Summary = leg.DepartureTime + " → " + leg.ArrivalTime + " · " + leg.Duration
	}
	return leg
}

type HotelOffer struct {
	Source              string   `json:"source,omitempty"`
	PropertyType        string   `json:"property_type,omitempty"`
	OriginalRating      *float64 `json:"original_rating,omitempty"`
	OriginalRatingScale *float64 `json:"original_rating_scale,omitempty"`
	PriceNote           string   `json:"price_note,omitempty"`
	OfferID             string   `json:"offer_id"`
	Name                string   `json:"name"`
	City                string   `json:"city"`
	CheckIn             string   `json:"check_in"`
	CheckOut            string   `json:"check_out"`
	PricePerNight       float64  `json:"price_per_night"`
	TotalPrice          float64  `json:"total_price"`
	Currency            string   `json:"currency"`
	Rating              *float64 `json:"rating,omitempty"`
	CheckoutURL         string   `json:"checkout_url,omitempty"`
	ImageURL            string   `json:"image_url,omitempty"`
}

type FlightBooking struct {
	OfferID  string  `json:"offer_id"`
	PNR      string  `json:"pnr"`
	Price    float64 `json:"price"`
	Currency string  `json:"currency"`
}

type HotelBookingResult struct {
	Status             string   `json:"status"` // completed | failed | ...
	ConfirmationNumber string   `json:"confirmation_number,omitempty"`
	TotalPrice         *float64 `json:"total_price,omitempty"`
	RecordingURL       string   `json:"recording_url,omitempty"`
	ScreenshotURLs     []string `json:"screenshot_urls,omitempty"`
	FailureReason      string   `json:"failure_reason,omitempty"`
}
