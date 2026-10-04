// Package dashboard owns group-scoped planning snapshots and WebSocket events.
package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"fare-brain/models"
	"fare-brain/store"
	"github.com/google/uuid"
)

type Session struct {
	ID                 string    `json:"id"`
	GroupID            string    `json:"groupId"`
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
	Recordings    map[string]map[string]any            `json:"recordings,omitempty"`
	Session       Session                              `json:"session"`
	Revision      uint64                               `json:"revision"`
	Flight        string                               `json:"flight"`
	Hotel         string                               `json:"hotel"`
	Planning      string                               `json:"planning"`
	PlanningTasks map[string]string                    `json:"planningTasks"`
	FlightMessage string                               `json:"flightMessage"`
	HotelMessage  string                               `json:"hotelMessage"`
	Flights       []map[string]any                     `json:"flights"`
	Hotels        []map[string]any                     `json:"hotels"`
	Plan          map[string]any                       `json:"plan"`
	Activity      []Activity                           `json:"activity"`
	Error         *string                              `json:"error"`
	Previews      map[string]map[string]map[string]any `json:"previews"`
}
type group struct {
	Sessions    []*Snapshot `json:"sessions"`
	Revision    uint64      `json:"revision"`
	subscribers map[chan []byte]bool
}
type Manager struct {
	mu     sync.Mutex
	store  store.Store
	groups map[string]*group
}

func New(st store.Store) *Manager { return &Manager{store: st, groups: map[string]*group{}} }

func (m *Manager) load(ctx context.Context, id string) (*group, error) {
	if g := m.groups[id]; g != nil {
		return g, nil
	}
	g := &group{Sessions: []*Snapshot{}, subscribers: map[chan []byte]bool{}}
	saved, err := m.store.GetWhatsAppSession(ctx, "dashboard:"+id)
	if err != nil {
		return nil, err
	}
	if saved != nil {
		raw, err := json.Marshal(saved.Data)
		if err != nil {
			return nil, err
		}
		if err = json.Unmarshal(raw, g); err != nil {
			return nil, err
		}
	}
	if g.Sessions == nil {
		g.Sessions = []*Snapshot{}
	}
	g.subscribers = map[chan []byte]bool{}
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
}
func (m *Manager) broadcast(g *group, event map[string]any) {
	raw, err := json.Marshal(event)
	if err != nil {
		return
	}
	ephemeral := event["type"] == "agent.browser.frame"
	for ch := range g.subscribers {
		select {
		case ch <- raw:
		default:
			if !ephemeral {
				close(ch)
				delete(g.subscribers, ch)
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
	g, err := m.load(ctx, t.GroupID)
	if err != nil {
		return "", err
	}
	if len(g.Sessions) > 0 {
		s := g.Sessions[0]
		if s.Session.Status != "failed" && s.Session.Status != "completed" {
			syncSession(&s.Session, t)
			return s.Session.ID, nil
		}
	}
	s := &Snapshot{Session: Session{ID: uuid.NewString(), GroupID: t.GroupID, Destination: "Planning your trip", Status: "created", CreatedAt: models.Now()}, Flight: "pending", Hotel: "pending", Planning: "pending", PlanningTasks: map[string]string{"flight-prices": "pending", "hotel-location": "pending", "group-budget": "pending", "daily-schedule": "pending"}, FlightMessage: "Waiting for your group’s destination choice", HotelMessage: "Waiting for your group’s destination choice", Flights: []map[string]any{}, Hotels: []map[string]any{}, Activity: []Activity{}, Previews: map[string]map[string]map[string]any{"flight": {}, "hotel": {}}}
	syncSession(&s.Session, t)
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
	if s.Session.Status == "completed" || s.Session.Status == "failed" {
		return nil
	}
	syncSession(&s.Session, t)
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
	switch kind {
	case "flight_search.started":
		s.Flight = "running"
		s.FlightMessage = "Searching flights"
		s.Session.Status = "searching"
		s.Session.Message = "Your flight and hotel agents are searching."
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
		if msg, ok := payload["message"].(string); ok {
			s.FlightMessage = msg
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
		s.Recordings[agent] = payload
	case "planning.started":
		s.Planning = "running"
		s.Session.Status = "planning"
		s.Session.Message = "Building your itinerary."
	case "planning.task.updated":
		task, _ := payload["taskId"].(string)
		status, _ := payload["status"].(string)
		if _, ok := s.PlanningTasks[task]; ok {
			s.PlanningTasks[task] = status
		}
	case "planning.completed":
		s.Planning = "completed"
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
		if origin == "" {
			origin, _ = wire["website"].(string)
		}
		if origin == "" {
			origin = "browser"
		}
		browser, _ := wire["browser_session_id"].(string)
		if agent != "flight" && agent != "hotel" {
			return nil
		}
		if s.Previews == nil {
			s.Previews = map[string]map[string]map[string]any{}
		}
		if s.Previews[agent] == nil {
			s.Previews[agent] = map[string]map[string]any{}
		}
		preview := s.Previews[agent][origin]
		if preview == nil || preview["browserSessionId"] != browser {
			preview = map[string]any{"browserSessionId": browser, "status": "starting"}
		}
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
		s.Previews[agent][origin] = preview
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
