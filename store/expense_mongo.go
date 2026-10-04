package store

import (
	"context"

	"fare-brain/models"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

type expenseLedgerDocument struct {
	ID                   string `bson:"_id"`
	models.ExpenseLedger `bson:",inline"`
}

func (s *MongoStore) GetExpenseLedger(ctx context.Context, groupID, sessionID string) (*models.ExpenseLedger, error) {
	var document expenseLedgerDocument
	err := s.expenses.FindOne(ctx, bson.M{"_id": expenseLedgerKey(groupID, sessionID)}).Decode(&document)
	if err == mongo.ErrNoDocuments {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return cloneExpenseLedger(&document.ExpenseLedger), nil
}

func (s *MongoStore) FindExpenseLedgerForOperation(ctx context.Context, groupID, operationID string) (*models.ExpenseLedger, error) {
	if operationID == "" {
		return nil, nil
	}
	var document expenseLedgerDocument
	err := s.expenses.FindOne(ctx, bson.M{"group_id": models.CanonicalGroupID(groupID), "operations.message_id": operationID}).Decode(&document)
	if err == mongo.ErrNoDocuments {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return cloneExpenseLedger(&document.ExpenseLedger), nil
}

func (s *MongoStore) SaveExpenseLedger(ctx context.Context, ledger *models.ExpenseLedger, expectedRevision int64) (*models.ExpenseLedger, error) {
	next, err := nextExpenseLedger(ledger, expectedRevision)
	if err != nil {
		return nil, err
	}
	document := expenseLedgerDocument{ID: expenseLedgerKey(next.GroupID, next.SessionID), ExpenseLedger: *next}
	if expectedRevision == 0 {
		_, err = s.expenses.InsertOne(ctx, document)
		if mongo.IsDuplicateKeyError(err) {
			return nil, ErrExpenseConflict
		}
	} else {
		result, replaceErr := s.expenses.ReplaceOne(ctx, bson.M{"_id": document.ID, "revision": expectedRevision}, document)
		err = replaceErr
		if err == nil && result.MatchedCount == 0 {
			return nil, ErrExpenseConflict
		}
	}
	if err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return nil, ErrExpenseConflict
		}
		return nil, err
	}
	return cloneExpenseLedger(next), nil
}
