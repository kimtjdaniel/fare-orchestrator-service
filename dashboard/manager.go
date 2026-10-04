// Package dashboard owns group-scoped planning snapshots and WebSocket events.
package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"fare-brain/models"
	"fare-brain/store"
	"github.com/google/uuid"
)

type Session struct {
	ID                 string    `json:"id"`
	GroupID            string    `json:"groupId"`
	GroupName          string    `json:"groupName,omitempty"`
	Destination        string    `json:"destination"`
	Origin             string    `json:"origin"`
	OriginAirport      string    `json:"originAirport,omitempty"`
	DestinationAirport string    `json:"destinationAirport,omitempty"`
	Adults             int       `json:"adults"`
	StartDate          string    `json:"startDate"`
	EndDate            string    `json:"endDate"`
	Status             string    `json:"status"`
	Message            string    `json:"message,omitempty"`
	CreatedAt          time.Time `json:"createdAt"`
}
type Activity struct {
	ID        uint64    `json:"id"`
	Timestamp time.Time `json:"timestamp"`
	Message   string    `json:"message"`
}
type Snapshot struct {
	Recordings           map[string]map[string]any            `json:"recordings,omitempty"`
	Session              Session                              `json:"session"`
	Revision             uint64                               `json:"revision"`
	Flight               string                               `json:"flight"`
	Hotel                string                               `json:"hotel"`
	Planning             string                               `json:"planning"`
	PlanningTasks        map[string]string                    `json:"planningTasks"`
	PlanningTaskMessages map[string]string                    `json:"planningTaskMessages,omitempty"`
	FlightMessage        string                               `json:"flightMessage"`
	HotelMessage         string                               `json:"hotelMessage"`
	Flights              []map[string]any                     `json:"flights"`
	Hotels               []map[string]any                     `json:"hotels"`
	Plan                 map[string]any                       `json:"plan"`
	Activity             []Activity                           `json:"activity"`
	Error                *string                              `json:"error"`
	Previews             map[string]map[string]map[string]any `json:"previews"`
}
type group struct {
	Sessions    []*Snapshot `json:"sessions"`
	Revision    uint64      `json:"revision"`
	subscribers map[chan []byte]bool
}
type Manager struct {
	mu                   sync.Mutex
	store                store.Store
	groups               map[string]*group
	dashboardSubscribers map[chan []byte]bool
	dashboardRevision    uint64
}

func New(st store.Store) *Manager {
	return &Manager{store: st, groups: map[string]*group{}, dashboardSubscribers: map[chan []byte]bool{}}
}

// dashboardSessionsLocked loads persisted groups as well as groups active in this process.
// The caller holds m.mu so the initial snapshot and subscription are atomic.
func (m *Manager) dashboardSessionsLocked(ctx context.Context) ([]*Snapshot, error) {
	trips, err := m.store.ListTrips(ctx)
	if err != nil {
		return nil, err
	}
	for _, trip := range trips {
		if _, err := m.load(ctx, trip.GroupID); err != nil {
			return nil, err
		}
	}
	sessions := []*Snapshot{}
	seen := map[string]bool{}
	for _, g := range m.groups {
		for _, snapshot := range g.Sessions {
			if !seen[snapshot.Session.ID] {
				seen[snapshot.Session.ID] = true
				sessions = append(sessions, snapshot)
			}
		}
	}
	sort.Slice(sessions, func(i, j int) bool {
		if sessions[i].Session.CreatedAt.Equal(sessions[j].Session.CreatedAt) {
			return sessions[i].Session.ID < sessions[j].Session.ID
		}
		return sessions[i].Session.CreatedAt.After(sessions[j].Session.CreatedAt)
	})
	return sessions, nil
}

func (m *Manager) load(ctx context.Context, id string) (*group, error) {
	aliases := models.GroupIDKeys(id)
	id = models.CanonicalGroupID(id)
	if g := m.groups[id]; g != nil {
		return g, nil
	}
	for _, alias := range aliases {
		if g := m.groups[alias]; g != nil {
			m.groups[id] = g
			return g, nil
		}
	}
	g := &group{Sessions: []*Snapshot{}, subscribers: map[chan []byte]bool{}}
	for _, alias := range aliases {
		saved, err := m.store.GetWhatsAppSession(ctx, "dashboard:"+alias)
		if err != nil {
			return nil, err
		}
		if saved == nil {
			continue
		}
		raw, err := json.Marshal(saved.Data)
		if err != nil {
			return nil, err
		}
		if err = json.Unmarshal(raw, g); err != nil {
			return nil, err
		}
		break
	}
	if g.Sessions == nil {
		g.Sessions = []*Snapshot{}
	}

	g.subscribers = map[chan []byte]bool{}
	for index, snapshot := range g.Sessions {
		if index > 0 && snapshot.Plan != nil {
			snapshot.Plan["itineraryEditable"] = false
			snapshot.Plan["canUndoActivityEdit"] = false
			delete(snapshot.Plan, "pendingActivityReplacement")
		}
	}
	if len(g.Sessions) > 0 {
		trip, err := m.store.GetTrip(ctx, id)
		if err != nil {
			return nil, err
		}
		if trip != nil {
			for _, snapshot := range g.Sessions {
				if snapshot.Session.GroupName == "" {
					snapshot.Session.GroupName = trip.GroupName
				}
			}
		}
		if trip != nil && trip.Itinerary != nil {
			itinerary := models.ScheduleItinerary(trip, trip.Itinerary, false)
			plan, _ := itinerary["dashboard_plan"].(map[string]any)
			if days, _ := plan["days"].([]any); len(days) > 0 {
				g.Sessions[0].Plan = plan
			}
		}
	}
	m.groups[id] = g
	return g, nil
}
func (m *Manager) persist(ctx context.Context, id string, g *group) error {
	raw, err := json.Marshal(g)
	if err != nil {
		return err
	}
	var data map[string]any
	if err = json.Unmarshal(raw, &data); err != nil {
		return err
	}
	// Browser frames stay ephemeral; retain only metadata in Mongo snapshots.
	if sessions, ok := data["sessions"].([]any); ok {
		for _, raw := range sessions {
			s, _ := raw.(map[string]any)
			p, _ := s["previews"].(map[string]any)
			for _, agents := range p {
				origins, _ := agents.(map[string]any)
				for _, item := range origins {
					preview, _ := item.(map[string]any)
					delete(preview, "src")
				}
			}
		}
	}
	_, err = m.store.SaveWhatsAppSession(ctx, "dashboard:"+id, data)
	return err
}
func syncSession(s *Session, t *models.Trip) {
	s.GroupID = t.GroupID
	s.GroupName = t.GroupName
	s.Origin = t.Origin
	s.DestinationAirport = t.DestinationAirport
	if t.Destination != "" {
		s.Destination = t.Destination
	}
	s.StartDate = t.EmbarkingDate
	s.EndDate = t.ReturningDate
	s.Adults = len(t.Participants)
	if s.Adults < 1 {
		s.Adults = 1
	}
	if len(t.Participants) > 0 {
		s.OriginAirport = t.Participants[0].OriginAirport
		if s.Origin == "" {
			s.Origin = t.Participants[0].OriginCity
		}
	}
	if s.StartDate == "" && len(t.Participants) > 0 {
		dates := t.Participants[0].GeneralPreferences.Availability
		if len(dates) > 0 {
			s.StartDate = dates[0]
			s.EndDate = dates[len(dates)-1]
		}
	}
	if t.State == models.AwaitingChoice {
		s.Message = "Waiting for your group to choose a destination in the chat."
	} else if t.State == models.Collecting {
		s.Message = "Understanding your group’s dates and preferences."
	}
	if (t.State == models.Collecting || t.State == models.AwaitingChoice) && (s.Status == "completed" || s.Status == "failed") {
		s.Status = "created"
	}
}
func (m *Manager) broadcast(g *group, event map[string]any) {
	broadcastSubscribers(g.subscribers, event)
	m.dashboardRevision++
	// Group revisions remain scoped to the group; dashboard revisions cover all groups.
	dashboardEvent := make(map[string]any, len(event))
	for key, value := range event {
		dashboardEvent[key] = value
	}
	dashboardEvent["revision"] = m.dashboardRevision
	broadcastSubscribers(m.dashboardSubscribers, dashboardEvent)
}

func broadcastSubscribers(subscribers map[chan []byte]bool, event map[string]any) {
	raw, err := json.Marshal(event)
	if err != nil {
		return
	}
	ephemeral := event["type"] == "agent.browser.frame"
	for ch := range subscribers {
		select {
		case ch <- raw:
		default:
			if !ephemeral {
				close(ch)
				delete(subscribers, ch)
			}
		}
	}
}
func (m *Manager) publishLocked(ctx context.Context, id string, g *group, s *Snapshot, kind string, payload map[string]any) error {
	g.Revision++
	s.Revision = g.Revision
	event := map[string]any{"version": 1, "type": kind, "groupId": id, "sessionId": s.Session.ID, "session": s.Session, "timestamp": models.Now(), "revision": g.Revision}
	for k, v := range payload {
		event[k] = v
	}
	if kind != "agent.browser.frame" && kind != "agent.browser.stream" && kind != "agent.browser.live_view" {
		if message, ok := payload["message"].(string); ok && message != "" {
			s.Activity = append([]Activity{{ID: g.Revision, Timestamp: models.Now(), Message: message}}, s.Activity...)
			if len(s.Activity) > 100 {
				s.Activity = s.Activity[:100]
			}
		}
		if err := m.persist(ctx, id, g); err != nil {
			return err
		}
		event["snapshot"] = s
	}
	m.broadcast(g, event)
	return nil
}
func (m *Manager) Begin(ctx context.Context, t *models.Trip) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.beginLocked(ctx, t, false)
}

// BeginNew always creates a session, regardless of the previous session's status.
func (m *Manager) BeginNew(ctx context.Context, t *models.Trip) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.beginLocked(ctx, t, true)
}

func (m *Manager) BeginLive(ctx context.Context, t *models.Trip) (string, error) {
	return m.Begin(ctx, t)
}

func (m *Manager) beginLocked(ctx context.Context, t *models.Trip, fresh bool) (string, error) {
	g, err := m.load(ctx, t.GroupID)
	if err != nil {
		return "", err
	}
	if t.SessionID == "" || (fresh && len(g.Sessions) > 0 && t.SessionID == g.Sessions[0].Session.ID) {
		id := uuid.NewString()
		if !fresh && len(g.Sessions) > 0 {
			id = g.Sessions[0].Session.ID
		}
		if err := m.store.BindUnassignedMessages(ctx, t.GroupID, id, t.HistoryStart); err != nil {
			return "", err
		}
		updated, err := m.store.UpdateTrip(ctx, t.ID, map[string]any{"session_id": id})
		if err != nil {
			return "", err
		}
		if updated == nil {
			return "", fmt.Errorf("trip %s unavailable", t.ID)
		}
		*t = *updated
	}
	if !fresh && len(g.Sessions) > 0 && g.Sessions[0].Session.ID == t.SessionID {
		s := g.Sessions[0]
		// Completion and retries belong to this trip. Only BeginNew starts another.
		syncSession(&s.Session, t)
		return s.Session.ID, nil
	}
	s := &Snapshot{Session: Session{ID: t.SessionID, GroupID: t.GroupID, Destination: "Planning your trip", Status: "created", CreatedAt: models.Now()}, Flight: "pending", Hotel: "pending", Planning: "pending", PlanningTasks: map[string]string{"flight-prices": "pending", "hotel-location": "pending", "group-budget": "pending", "daily-schedule": "pending"}, FlightMessage: "Waiting for your group’s destination choice", HotelMessage: "Waiting for your group’s destination choice", Flights: []map[string]any{}, Hotels: []map[string]any{}, Activity: []Activity{}, Previews: map[string]map[string]map[string]any{"flight": {}, "hotel": {}}}
	syncSession(&s.Session, t)
	for _, previous := range g.Sessions {
		if previous.Plan != nil {
			previous.Plan["itineraryEditable"] = false
			previous.Plan["canUndoActivityEdit"] = false
			delete(previous.Plan, "pendingActivityReplacement")
		}
	}
	g.Sessions = append([]*Snapshot{s}, g.Sessions...)
	if len(g.Sessions) > 20 {
		g.Sessions = g.Sessions[:20]
	}
	if err = m.publishLocked(ctx, t.GroupID, g, s, "session.started", map[string]any{"message": "Trip planning started"}); err != nil {
		return "", err
	}
	return s.Session.ID, nil
}
func (m *Manager) SyncTrip(ctx context.Context, t *models.Trip) error {
	if t == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	g, err := m.load(ctx, t.GroupID)
	if err != nil {
		return err
	}
	if len(g.Sessions) == 0 {
		return nil
	}
	s := g.Sessions[0]
	syncSession(&s.Session, t)
	if t.Itinerary != nil {
		itinerary := models.ScheduleItinerary(t, t.Itinerary, false)
		if plan, ok := itinerary["dashboard_plan"].(map[string]any); ok {
			if days, _ := plan["days"].([]any); len(days) > 0 {
				s.Plan = plan
			}
		}
	}
	return m.publishLocked(ctx, t.GroupID, g, s, "session.updated", nil)
}
func (m *Manager) CurrentID(ctx context.Context, id string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	g, err := m.load(ctx, id)
	if err != nil {
		return "", err
	}
	if len(g.Sessions) == 0 {
		return "", nil
	}
	return g.Sessions[0].Session.ID, nil
}
func (m *Manager) Emit(ctx context.Context, id, kind string, payload map[string]any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	g, err := m.load(ctx, id)
	if err != nil {
		return err
	}
	if len(g.Sessions) == 0 {
		return nil
	}
	s := g.Sessions[0]
	if wire, ok := payload["event"].(map[string]any); ok {
		if sessionID, _ := wire["session_id"].(string); sessionID != "" && sessionID != s.Session.ID {
			return nil // An older search must not paint a newer planning session.
		}
	}
	switch kind {
	case "flight_search.started":
		s.Flight = "running"
		s.FlightMessage = "Searching flights"
		s.Session.Status = "searching"
		s.Error = nil
		if msg, ok := payload["message"].(string); ok && msg != "" {
			s.Session.Message = msg
		} else {
			s.Session.Message = "Your flight and hotel agents are searching."
		}
	case "hotel_search.started":
		s.Hotel = "running"
		s.HotelMessage = "Searching stays"
		s.Session.Status = "searching"
	case "flight_search.progress":
		if msg, ok := payload["message"].(string); ok {
			s.FlightMessage = msg
		}
	case "hotel_search.progress":
		if msg, ok := payload["message"].(string); ok {
			s.HotelMessage = msg
		}
	case "flight_search.failed":
		s.Flight = "failed"
		msg, _ := payload["message"].(string)
		if msg == "" {
			msg = "Flight search failed."
		}
		s.FlightMessage = msg
		if s.Hotel == "failed" {
			s.Session.Status = "failed"
			s.Session.Message = msg
			s.Error = nil
		} else {
			s.Session.Status = "searching"
			s.Session.Message = "Flights didn't come back. Retry the flight search, or skip it and keep the stay."
			s.Error = nil
		}
	case "hotel_search.failed":
		s.Hotel = "failed"
		if msg, ok := payload["message"].(string); ok {
			s.HotelMessage = msg
		}
	case "flight_search.completed":
		s.Flight = "completed"
		if rows, ok := payload["flights"].([]map[string]any); ok {
			s.Flights = rows
		}
		s.FlightMessage = fmt.Sprintf("Found %d flight options", len(s.Flights))
		if msg, ok := payload["message"].(string); ok && strings.TrimSpace(msg) != "" {
			s.FlightMessage = msg
		}
		if s.Session.Status == "failed" && s.Hotel != "failed" {
			s.Session.Status = "searching"
			s.Error = nil
			s.Session.Message = s.FlightMessage
		}
	case "hotel_search.completed":
		s.Hotel = "completed"
		if rows, ok := payload["hotels"].([]map[string]any); ok {
			s.Hotels = rows
		}
		s.HotelMessage = fmt.Sprintf("Found %d stays", len(s.Hotels))
	case "flight_search.recording.completed", "hotel_search.recording.completed":
		if s.Recordings == nil {
			s.Recordings = map[string]map[string]any{}
		}
		agent, _ := payload["agentType"].(string)
		// Persist the full source list, including partial failures, for REST and reconnects.
		s.Recordings[agent] = payload
	case "booking.started":
		s.Session.Status = "booking"
		s.Session.Message = "Booking flights and the stay."
		s.Flight = "running"
		s.FlightMessage = "Booking the selected flights"
		s.Hotel = "running"
		s.HotelMessage = "Booking the stay"
	case "planning.started":
		s.Planning = "running"
		s.Session.Status = "planning"
		s.Session.Message = "Building your itinerary."
		if msg, ok := payload["message"].(string); ok && msg != "" {
			s.Session.Message = msg
		}
	case "planning.task.updated":
		task, _ := payload["taskId"].(string)
		status, _ := payload["status"].(string)
		if _, ok := s.PlanningTasks[task]; ok {
			s.PlanningTasks[task] = status
			if status == "running" {
				s.Planning = "running"
				s.Session.Status = "planning"
			}
			if msg, ok := payload["message"].(string); ok && msg != "" {
				if s.PlanningTaskMessages == nil {
					s.PlanningTaskMessages = map[string]string{}
				}
				s.PlanningTaskMessages[task] = msg
				s.Session.Message = msg
			}
		}
	case "itinerary.updated":
		if plan, ok := payload["plan"].(map[string]any); ok {
			s.Plan = plan
		}
	case "planning.completed":
		s.Planning = "completed"
		s.Session.Message = "Your itinerary is ready. Finalizing your trip plan."
		if plan, ok := payload["plan"].(map[string]any); ok {
			s.Plan = plan
		}
	case "session.completed":
		s.Session.Status = "completed"
		s.Session.Message = "Your trip plan is ready."
	case "session.failed":
		if s.Session.Status == "completed" {
			return nil
		}
		s.Session.Status = "failed"
		msg, _ := payload["message"].(string)
		if msg == "" {
			msg = "Trip planning was interrupted."
		}
		s.Session.Message = msg
		s.Error = &msg
		if s.Flight == "running" {
			s.Flight = "failed"
		}
		if s.Hotel == "running" {
			s.Hotel = "failed"
		}
		if s.Planning == "running" {
			s.Planning = "failed"
		}
		for id, status := range s.PlanningTasks {
			if status == "running" {
				s.PlanningTasks[id] = "failed"
			}
		}
	case "agent.browser.frame", "agent.browser.stream", "agent.browser.live_view":
		agent, _ := payload["agentType"].(string)
		wire, _ := payload["event"].(map[string]any)
		origin, _ := wire["origin"].(string)
		website, _ := wire["website"].(string)
		browser, _ := wire["browser_session_id"].(string)
		if agent != "flight" && agent != "hotel" {
			return nil
		}
		if website == "" {
			if agent == "flight" {
				website = "google_flights"
			} else {
				website = origin
			}
		}
		previewKey := website
		if origin != "" && origin != website {
			if previewKey != "" {
				previewKey += ":"
			}
			previewKey += origin
		}
		if previewKey == "" {
			previewKey = "browser"
		}
		if s.Previews == nil {
			s.Previews = map[string]map[string]map[string]any{}
		}
		if s.Previews[agent] == nil {
			s.Previews[agent] = map[string]map[string]any{}
		}
		preview := s.Previews[agent][previewKey]
		if preview == nil || preview["browserSessionId"] != browser {
			preview = map[string]any{"browserSessionId": browser, "status": "starting"}
		}
		preview["website"] = website
		preview["origin"] = origin
		if kind == "agent.browser.frame" {
			data, _ := wire["data"].(string)
			mime, _ := wire["mime_type"].(string)
			if mime != "image/jpeg" || data == "" {
				return nil
			}
			preview["src"] = "data:image/jpeg;base64," + data
			preview["status"] = "live"
		} else if kind == "agent.browser.stream" {
			preview["status"] = wire["status"]
		} else {
			preview["liveViewUrl"] = wire["url"]
		}
		s.Previews[agent][previewKey] = preview
	}
	return m.publishLocked(ctx, id, g, s, kind, payload)
}

// Store forwards persistence unchanged and pushes trip-detail updates to the dashboard.
type Store struct {
	store.Store
	Dashboard *Manager
}

func (s *Store) UpdateTrip(ctx context.Context, id string, fields map[string]any) (*models.Trip, error) {
	t, err := s.Store.UpdateTrip(ctx, id, fields)
	if err != nil {
		return t, err
	}
	if err = s.Dashboard.SyncTrip(ctx, t); err != nil {
		return t, err
	}
	return t, nil
}

func (s *Store) UpdateItinerary(ctx context.Context, expected *models.Trip, itinerary map[string]any) (*models.Trip, error) {
	t, err := s.Store.UpdateItinerary(ctx, expected, itinerary)
	if err != nil {
		return t, err
	}
	// Persistence succeeded. A dashboard delivery error must not invite a duplicate edit.
	_ = s.Dashboard.SyncTrip(ctx, t)
	return t, nil
}
