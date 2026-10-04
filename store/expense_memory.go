package store

import (
	"context"
	"fmt"
	"math"
	"strings"

	"fare-brain/models"
)

func expenseLedgerKey(groupID, sessionID string) string {
	return models.CanonicalGroupID(groupID) + "|" + sessionID
}

// Clone all nested slices so callers cannot mutate saved entries or pending drafts.
func cloneExpenseLedger(ledger *models.ExpenseLedger) *models.ExpenseLedger {
	if ledger == nil {
		return nil
	}
	copy := *ledger
	copy.Members = append([]models.ExpenseMember(nil), ledger.Members...)
	copy.Expenses = append([]models.Expense(nil), ledger.Expenses...)
	for i := range copy.Expenses {
		copy.Expenses[i].Shares = append([]models.ExpenseShare(nil), ledger.Expenses[i].Shares...)
	}
	copy.Drafts = append([]models.ExpenseDraft(nil), ledger.Drafts...)
	for i := range copy.Drafts {
		copy.Drafts[i].Expense.Shares = append([]models.ExpenseShare(nil), ledger.Drafts[i].Expense.Shares...)
	}
	copy.Operations = append([]models.ExpenseOperation(nil), ledger.Operations...)
	return &copy
}

func nextExpenseLedger(ledger *models.ExpenseLedger, expectedRevision int64) (*models.ExpenseLedger, error) {
	if ledger == nil || strings.TrimSpace(ledger.GroupID) == "" || strings.TrimSpace(ledger.SessionID) == "" {
		return nil, fmt.Errorf("expense ledger requires a group and session")
	}
	if expectedRevision < 0 || expectedRevision == math.MaxInt64 {
		return nil, ErrExpenseConflict
	}
	next := cloneExpenseLedger(ledger)
	next.GroupID = models.CanonicalGroupID(next.GroupID)
	next.Revision = expectedRevision + 1
	next.UpdatedAt = models.Now()
	return next, nil
}

func (s *MemoryStore) GetExpenseLedger(ctx context.Context, groupID, sessionID string) (*models.ExpenseLedger, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneExpenseLedger(s.expenses[expenseLedgerKey(groupID, sessionID)]), nil
}

func (s *MemoryStore) FindExpenseLedgerForOperation(ctx context.Context, groupID, operationID string) (*models.ExpenseLedger, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	groupID = models.CanonicalGroupID(groupID)
	if operationID == "" {
		return nil, nil
	}
	for _, ledger := range s.expenses {
		if ledger.GroupID != groupID {
			continue
		}
		for _, operation := range ledger.Operations {
			if operation.MessageID == operationID {
				return cloneExpenseLedger(ledger), nil
			}
		}
	}
	return nil, nil
}

func (s *MemoryStore) SaveExpenseLedger(ctx context.Context, ledger *models.ExpenseLedger, expectedRevision int64) (*models.ExpenseLedger, error) {
	next, err := nextExpenseLedger(ledger, expectedRevision)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := expenseLedgerKey(next.GroupID, next.SessionID)
	current := s.expenses[key]
	if (current == nil && expectedRevision != 0) || (current != nil && current.Revision != expectedRevision) {
		return nil, ErrExpenseConflict
	}
	for otherKey, other := range s.expenses {
		if otherKey == key || other.GroupID != next.GroupID {
			continue
		}
		claimed := map[string]bool{}
		for _, operation := range other.Operations {
			claimed[operation.MessageID] = true
		}
		for _, operation := range next.Operations {
			if claimed[operation.MessageID] {
				return nil, ErrExpenseConflict
			}
		}
	}
	if s.expenses == nil {
		s.expenses = map[string]*models.ExpenseLedger{}
	}
	s.expenses[key] = next
	return cloneExpenseLedger(next), nil
}
