package models

import "time"

type ExpenseMember struct {
	ID   string `json:"id" bson:"id"`
	Name string `json:"name" bson:"name"`
}

type ExpenseShare struct {
	MemberID    string `json:"member_id" bson:"member_id"`
	AmountCents int64  `json:"amount_cents" bson:"amount_cents"`
}

type Expense struct {
	ID          string         `json:"id" bson:"id"`
	Description string         `json:"description" bson:"description"`
	AmountCents int64          `json:"amount_cents" bson:"amount_cents"`
	Currency    string         `json:"currency" bson:"currency"`
	PayerID     string         `json:"payer_id" bson:"payer_id"`
	Shares      []ExpenseShare `json:"shares" bson:"shares"`
	CreatedBy   string         `json:"created_by" bson:"created_by"`
	CreatedAt   time.Time      `json:"created_at" bson:"created_at"`
	Deleted     bool           `json:"deleted" bson:"deleted"`
}

type ExpenseDraft struct {
	Request         string    `json:"request,omitempty" bson:"request,omitempty"`
	Question        string    `json:"question,omitempty" bson:"question,omitempty"`
	ID              string    `json:"id" bson:"id"`
	ActorID         string    `json:"actor_id" bson:"actor_id"`
	Action          string    `json:"action" bson:"action"`
	Expense         Expense   `json:"expense" bson:"expense"`
	DeleteExpenseID string    `json:"delete_expense_id" bson:"delete_expense_id"`
	SourceMessageID string    `json:"source_message_id" bson:"source_message_id"`
	CreatedAt       time.Time `json:"created_at" bson:"created_at"`
}

type ExpenseOperation struct {
	MessageID string `json:"message_id" bson:"message_id"`
	Reply     string `json:"reply" bson:"reply"`
}

type ExpenseLedger struct {
	GroupID    string             `json:"group_id" bson:"group_id"`
	SessionID  string             `json:"session_id" bson:"session_id"`
	Revision   int64              `json:"revision" bson:"revision"`
	Members    []ExpenseMember    `json:"members" bson:"members"`
	Expenses   []Expense          `json:"expenses" bson:"expenses"`
	Drafts     []ExpenseDraft     `json:"drafts" bson:"drafts"`
	Operations []ExpenseOperation `json:"operations" bson:"operations"`
	UpdatedAt  time.Time          `json:"updated_at" bson:"updated_at"`
}

type ExpenseBalance struct {
	MemberID     string `json:"member_id" bson:"member_id"`
	Name         string `json:"name" bson:"name"`
	BalanceCents int64  `json:"balance_cents" bson:"balance_cents"`
}
