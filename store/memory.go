package store

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"fare-brain/models"
)

// MemoryStore is used when MONGODB_URI is empty (solo dev, tests). Lost on restart.
type MemoryStore struct {
	mu       sync.Mutex
	messages []models.Message
	trips    map[string]*models.Trip // keyed by group_id (== trip.ID, singleton per group)
	sessions map[string]*models.WhatsAppSession
	expenses map[string]*models.ExpenseLedger
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{trips: map[string]*models.Trip{}, sessions: map[string]*models.WhatsAppSession{}, expenses: map[string]*models.ExpenseLedger{}}
}

func (s *MemoryStore) Connect(ctx context.Context) error { return nil }
func (s *MemoryStore) Close(ctx context.Context) error   { return nil }

func (s *MemoryStore) SaveMessage(ctx context.Context, msg *models.Message) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if msg.ExternalID != "" {
		for _, m := range s.messages {
			if m.GroupID == msg.GroupID && m.ExternalID == msg.ExternalID {
				return false, nil
			}
		}
	}
	cp := *msg
	cp.ID = uuid.NewString()
	cp.RecordedAt = models.Now()
	s.messages = append(s.messages, cp)
	*msg = cp
	return true, nil
}

func (s *MemoryStore) GetSessionMessages(ctx context.Context, groupID, sessionID string) ([]models.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows := []models.Message{}
	if sessionID == "" {
		return rows, nil
	}
	for _, msg := range s.messages {
		if msg.GroupID == groupID && msg.SessionID == sessionID {
			rows = append(rows, msg)
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].SentAt.Equal(rows[j].SentAt) {
			return rows[i].RecordedAt.Before(rows[j].RecordedAt)
		}
		return rows[i].SentAt.Before(rows[j].SentAt)
	})
	return rows, nil
}

func (s *MemoryStore) AssignMessageSession(ctx context.Context, groupID, sessionID string, messageIDs []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make(map[string]bool, len(messageIDs))
	for _, id := range messageIDs {
		ids[id] = true
	}
	for i := range s.messages {
		msg := &s.messages[i]
		if msg.GroupID == groupID && ids[msg.ID] {
			msg.SessionID = sessionID
			msg.TripID = groupID
		}
	}
	return nil
}

func (s *MemoryStore) BindUnassignedMessages(ctx context.Context, groupID, sessionID string, since *time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.messages {
		msg := &s.messages[i]
		if msg.GroupID == groupID && msg.SessionID == "" && (since == nil || !msg.SentAt.Before(*since)) {
			msg.SessionID = sessionID
			msg.TripID = groupID
		}
	}
	return nil
}

func (s *MemoryStore) GetMessages(ctx context.Context, groupID string, since *time.Time, limit int, includeBot bool) ([]models.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var rows []models.Message
	for _, m := range s.messages {
		if m.GroupID != groupID {
			continue
		}
		if since != nil && !m.SentAt.After(*since) {
			continue
		}
		if !includeBot && m.IsBot {
			continue
		}
		rows = append(rows, m)
	}
	if len(rows) > limit {
		rows = rows[len(rows)-limit:]
	}
	return rows, nil
}

func (s *MemoryStore) CreateTrip(ctx context.Context, groupID, groupName string) (*models.Trip, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.trips[groupID]; ok {
		cp := *t
		return &cp, nil
	}
	now := models.Now()
	trip := &models.Trip{
		ID: groupID, GroupID: groupID, GroupName: groupName, State: models.Collecting,
		SessionID: uuid.NewString(),
		CreatedAt: now, UpdatedAt: now,
		Participants: []models.Participant{}, Options: []models.Option{},
		Flights: []models.Flight{}, Accommodations: []models.Accommodation{},
	}
	s.trips[groupID] = trip
	cp := *trip
	return &cp, nil
}

func (s *MemoryStore) ListTrips(ctx context.Context) ([]*models.Trip, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*models.Trip, 0, len(s.trips))
	for _, t := range s.trips {
		cp := *t
		out = append(out, &cp)
	}
	return out, nil
}

func (s *MemoryStore) GetTrip(ctx context.Context, tripID string) (*models.Trip, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range models.GroupIDKeys(tripID) {
		if t, ok := s.trips[id]; ok {
			cp := *t
			return &cp, nil
		}
	}
	return nil, nil
}

func (s *MemoryStore) ResetTrip(ctx context.Context, tripID, groupName string) (*models.Trip, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.trips[tripID]
	if !ok {
		return nil, nil
	}
	resetTrip(t, groupName)
	cp := *t
	return &cp, nil
}

func (s *MemoryStore) UpdateTrip(ctx context.Context, tripID string, fields map[string]any) (*models.Trip, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.trips[tripID]
	if !ok {
		return nil, nil
	}
	cp := *t
	if err := applyTripFields(&cp, fields); err != nil {
		return nil, err
	}
	s.trips[tripID] = &cp
	out := cp
	return &out, nil
}

func (s *MemoryStore) GetWhatsAppSession(ctx context.Context, id string) (*models.WhatsAppSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return nil, nil
	}
	cp := *sess
	return &cp, nil
}

func (s *MemoryStore) SaveWhatsAppSession(ctx context.Context, id string, data map[string]any) (*models.WhatsAppSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := &models.WhatsAppSession{ID: id, Data: data, UpdatedAt: models.Now()}
	s.sessions[id] = sess
	cp := *sess
	return &cp, nil
}

func (s *MemoryStore) UpdateItinerary(ctx context.Context, expected *models.Trip, itinerary map[string]any) (*models.Trip, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.trips[expected.ID]
	if t == nil || t.ItineraryRevision != expected.ItineraryRevision || !t.UpdatedAt.Equal(expected.UpdatedAt) {
		return nil, ErrItineraryConflict
	}
	cp := *t
	if err := applyTripFields(&cp, map[string]any{"itinerary": itinerary}); err != nil {
		return nil, err
	}
	s.trips[expected.ID] = &cp
	return &cp, nil
}
