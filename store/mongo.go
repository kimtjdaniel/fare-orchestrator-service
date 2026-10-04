package store

import (
	"context"
	"time"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"fare-brain/models"
)

// MongoStore is the MongoDB-backed Store. Each trip is a singleton document per group (_id ==
// group_id) with participants/options/flights/accommodations embedded; messages and whatsapp
// sessions are their own collections.
type MongoStore struct {
	uri      string
	dbName   string
	client   *mongo.Client
	trips    *mongo.Collection
	msgs     *mongo.Collection
	sessions *mongo.Collection
}

func NewMongoStore(uri, dbName string) *MongoStore {
	return &MongoStore{uri: uri, dbName: dbName}
}

func (s *MongoStore) Connect(ctx context.Context) error {
	client, err := mongo.Connect(options.Client().ApplyURI(s.uri))
	if err != nil {
		return err
	}
	if err := client.Ping(ctx, nil); err != nil {
		return err
	}
	s.client = client
	db := client.Database(s.dbName)
	s.trips = db.Collection("trips")
	s.msgs = db.Collection("messages")
	s.sessions = db.Collection("whatsapp_sessions")

	// Idempotent index creation.
	_, err = s.msgs.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "group_id", Value: 1}, {Key: "sent_at", Value: 1}}},
		{
			Keys: bson.D{{Key: "group_id", Value: 1}, {Key: "external_id", Value: 1}},
			Options: options.Index().SetUnique(true).SetPartialFilterExpression(
				bson.D{{Key: "external_id", Value: bson.D{{Key: "$exists", Value: true}}}}),
		},
	})
	if err != nil {
		return err
	}
	_, err = s.trips.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "group_id", Value: 1}},
	})
	return err
}

func (s *MongoStore) Close(ctx context.Context) error {
	if s.client == nil {
		return nil
	}
	return s.client.Disconnect(ctx)
}

func (s *MongoStore) SaveMessage(ctx context.Context, msg *models.Message) (bool, error) {
	cp := *msg
	cp.ID = uuid.NewString()
	_, err := s.msgs.InsertOne(ctx, cp)
	if mongo.IsDuplicateKeyError(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (s *MongoStore) GetMessages(ctx context.Context, groupID string, since *time.Time, limit int, includeBot bool) ([]models.Message, error) {
	filter := bson.D{{Key: "group_id", Value: groupID}}
	if since != nil {
		filter = append(filter, bson.E{Key: "sent_at", Value: bson.D{{Key: "$gt", Value: *since}}})
	}
	if !includeBot {
		// $ne true also matches docs where is_bot is missing (plain false does not).
		filter = append(filter, bson.E{Key: "is_bot", Value: bson.D{{Key: "$ne", Value: true}}})
	}
	opts := options.Find().SetSort(bson.D{{Key: "sent_at", Value: -1}}).SetLimit(int64(limit))
	cur, err := s.msgs.Find(ctx, filter, opts)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var rows []models.Message
	if err := cur.All(ctx, &rows); err != nil {
		return nil, err
	}
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}
	return rows, nil
}

func (s *MongoStore) CreateTrip(ctx context.Context, groupID, groupName string) (*models.Trip, error) {
	existing, err := s.GetTrip(ctx, groupID)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return existing, nil
	}
	now := models.Now()
	trip := &models.Trip{
		ID: groupID, GroupID: groupID, GroupName: groupName, State: models.Collecting,
		CreatedAt: now, UpdatedAt: now,
		Participants: []models.Participant{}, Options: []models.Option{},
		Flights: []models.Flight{}, Accommodations: []models.Accommodation{},
	}
	if _, err := s.trips.InsertOne(ctx, trip); err != nil {
		return nil, err
	}
	return trip, nil
}

func (s *MongoStore) GetTrip(ctx context.Context, tripID string) (*models.Trip, error) {
	var trip models.Trip
	err := s.trips.FindOne(ctx, bson.D{{Key: "_id", Value: tripID}}).Decode(&trip)
	if err == mongo.ErrNoDocuments {
		err = s.trips.FindOne(ctx, bson.D{{Key: "group_id", Value: tripID}}).Decode(&trip)
	}
	if err == mongo.ErrNoDocuments {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	normalizeTrip(&trip, tripID)
	return &trip, nil
}

func (s *MongoStore) ListTrips(ctx context.Context) ([]*models.Trip, error) {
	cur, err := s.trips.Find(ctx, bson.D{}, options.Find().SetSort(bson.D{{Key: "updated_at", Value: -1}}))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var rows []models.Trip
	if err := cur.All(ctx, &rows); err != nil {
		return nil, err
	}
	out := make([]*models.Trip, 0, len(rows))
	for i := range rows {
		normalizeTrip(&rows[i], rows[i].ID)
		out = append(out, &rows[i])
	}
	return out, nil
}

func normalizeTrip(trip *models.Trip, fallbackID string) {
	if trip.ID == "" {
		trip.ID = fallbackID
	}
	if trip.GroupID == "" {
		trip.GroupID = trip.ID
	}
	if trip.State == "" {
		trip.State = models.Collecting
	}
}

func (s *MongoStore) ResetTrip(ctx context.Context, tripID, groupName string) (*models.Trip, error) {
	trip, err := s.GetTrip(ctx, tripID)
	if err != nil || trip == nil {
		return trip, err
	}
	resetTrip(trip, groupName)
	_, err = s.trips.ReplaceOne(ctx, bson.D{{Key: "_id", Value: tripID}}, trip)
	if err != nil {
		return nil, err
	}
	return trip, nil
}

func (s *MongoStore) UpdateTrip(ctx context.Context, tripID string, fields map[string]any) (*models.Trip, error) {
	trip, err := s.GetTrip(ctx, tripID)
	if err != nil || trip == nil {
		return trip, err
	}
	if err := applyTripFields(trip, fields); err != nil {
		return nil, err
	}
	_, err = s.trips.ReplaceOne(ctx, bson.D{{Key: "_id", Value: tripID}}, trip)
	if err != nil {
		return nil, err
	}
	return trip, nil
}

func (s *MongoStore) GetWhatsAppSession(ctx context.Context, id string) (*models.WhatsAppSession, error) {
	var sess models.WhatsAppSession
	err := s.sessions.FindOne(ctx, bson.D{{Key: "_id", Value: id}}).Decode(&sess)
	if err == mongo.ErrNoDocuments {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &sess, nil
}

func (s *MongoStore) SaveWhatsAppSession(ctx context.Context, id string, data map[string]any) (*models.WhatsAppSession, error) {
	sess := &models.WhatsAppSession{ID: id, Data: data, UpdatedAt: models.Now()}
	_, err := s.sessions.ReplaceOne(ctx, bson.D{{Key: "_id", Value: id}}, sess, options.Replace().SetUpsert(true))
	if err != nil {
		return nil, err
	}
	return sess, nil
}
