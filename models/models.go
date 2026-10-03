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
	GroupID    string `json:"group_id"`
	GroupName  string `json:"group_name"`
	SenderID   string `json:"sender_id"`
	SenderName string `json:"sender_name"`
	Participants []GroupMember `json:"participants,omitempty"`
	Text         string        `json:"text"`
	Tagged       bool          `json:"tagged"`    // was the bot @mentioned?
	Timestamp    int64         `json:"timestamp"` // unix seconds
	MessageID    string        `json:"message_id,omitempty"`
	AgentID      string        `json:"agent_id,omitempty"`
}

// GroupMember is one WhatsApp group participant as reported by the robot.
type GroupMember struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	IsAdmin bool   `json:"is_admin,omitempty"`
	IsAgent bool   `json:"is_agent,omitempty"`
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
	WhatsAppName         string `json:"whatsapp_name" bson:"whatsapp_name"`
	Payer                bool   `json:"payer" bson:"payer"`
	UploadedPaymentInfo  bool   `json:"uploaded_payment_info" bson:"uploaded_payment_info"`
	UploadedPassportInfo bool   `json:"uploaded_passport_info" bson:"uploaded_passport_info"`
	OriginCity           string `json:"origin_city,omitempty" bson:"origin_city,omitempty"`
	OriginAirport        string `json:"origin_airport,omitempty" bson:"origin_airport,omitempty"` // IATA
	Origin               string `json:"origin,omitempty" bson:"origin,omitempty"`                 // schema: same origin for now; city or IATA

	GeneralPreferences       GeneralPreferences       `json:"general_preferences" bson:"general_preferences"`
	FlightPreferences        FlightPreferences        `json:"flight_preferences" bson:"flight_preferences"`
	AccommodationPreferences AccommodationPreferences `json:"accommodation_preferences" bson:"accommodation_preferences"`
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
	BookingStatus BookingStatus `json:"booking_status" bson:"booking_status"`
	CheckInDate   time.Time     `json:"check_in_date" bson:"check_in_date"`
	CheckOutDate  time.Time     `json:"check_out_date" bson:"check_out_date"`
	Rating        *float64      `json:"rating,omitempty" bson:"rating,omitempty"`
	Costs         *float64      `json:"costs,omitempty" bson:"costs,omitempty"`
	BookingURL    string        `json:"booking_url,omitempty" bson:"booking_url,omitempty"`
	ProviderRef   string        `json:"provider_ref,omitempty" bson:"provider_ref,omitempty"` // confirmation number once booked
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

	Itinerary    map[string]any `json:"itinerary,omitempty" bson:"itinerary,omitempty"`
	ApprovedBy   string         `json:"approved_by,omitempty" bson:"approved_by,omitempty"`
	ApprovedAt   *time.Time     `json:"approved_at,omitempty" bson:"approved_at,omitempty"`
	HistoryStart *time.Time     `json:"history_start,omitempty" bson:"history_start,omitempty"`
	CreatedAt    time.Time      `json:"created_at" bson:"created_at"`
	UpdatedAt    time.Time      `json:"updated_at" bson:"updated_at"`
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
	OfferID     string  `json:"offer_id"`
	Origin      string  `json:"origin"`
	Destination string  `json:"destination"`
	DepartDate  string  `json:"depart_date"`
	ReturnDate  string  `json:"return_date"`
	Airline     string  `json:"airline"`
	Price       float64 `json:"price"`
	Currency    string  `json:"currency"`
	Summary     string  `json:"summary"` // "AC 554 dep 08:10, arr 11:35"
}

type HotelOffer struct {
	OfferID       string   `json:"offer_id"`
	Name          string   `json:"name"`
	City          string   `json:"city"`
	CheckIn       string   `json:"check_in"`
	CheckOut      string   `json:"check_out"`
	PricePerNight float64  `json:"price_per_night"`
	TotalPrice    float64  `json:"total_price"`
	Currency      string   `json:"currency"`
	Rating        *float64 `json:"rating,omitempty"`
	CheckoutURL   string   `json:"checkout_url,omitempty"` // page Skyvern drives to book it
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
