package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf16"

	"fare-brain/models"
	"fare-brain/store"
	"fare-brain/tools"

	"github.com/google/uuid"
)

type ExpenseView struct {
	GroupID             string                  `json:"group_id"`
	SessionID           string                  `json:"session_id"`
	Revision            int64                   `json:"revision"`
	Writable            bool                    `json:"writable"`
	Members             []models.ExpenseMember  `json:"members"`
	Expenses            []models.Expense        `json:"expenses"`
	Balances            []models.ExpenseBalance `json:"balances"`
	NotificationWarning string                  `json:"notification_warning,omitempty"`
}

func expenseMember(members []models.ExpenseMember, id string) *models.ExpenseMember {
	for i := range members {
		if members[i].ID == id {
			return &members[i]
		}
	}
	return nil
}

func expenseMembers(trip *models.Trip) []models.ExpenseMember {
	members := []models.ExpenseMember{}
	if trip == nil {
		return members
	}
	add := func(id, name string) {
		if id != "" && name != "" && expenseMember(members, id) == nil {
			members = append(members, models.ExpenseMember{ID: id, Name: name})
		}
	}
	for _, p := range trip.Participants {
		if p.Intake != nil && p.Intake.Attendance == "not_coming" {
			continue
		}
		id := p.WaID
		if id == "" {
			matches := 0
			for _, member := range trip.Roster {
				if !member.IsAgent && strings.EqualFold(member.Name, p.WhatsAppName) {
					matches++
					if matches > 1 {
						id = ""
						break
					}
					id = member.ID
				}
			}
		}
		add(id, p.WhatsAppName)
	}
	if len(trip.Participants) == 0 {
		for _, member := range trip.Roster {
			if !member.IsAgent {
				add(member.ID, member.Name)
			}
		}
	}
	return members
}

func (b *Brain) expenseLedger(ctx context.Context, groupID, sessionID string) (*models.ExpenseLedger, bool, error) {
	groupID = models.CanonicalGroupID(groupID)
	if _, err := uuid.Parse(sessionID); groupID == "" || err != nil {
		return nil, false, &DashboardError{Status: 400, Msg: "a group and trip session are required"}
	}
	ledger, err := b.Store.GetExpenseLedger(ctx, groupID, sessionID)
	if err != nil {
		return nil, false, err
	}
	if ledger == nil {
		ledger = &models.ExpenseLedger{GroupID: groupID, SessionID: sessionID, Members: []models.ExpenseMember{}, Expenses: []models.Expense{}, Drafts: []models.ExpenseDraft{}, Operations: []models.ExpenseOperation{}}
	}
	current := ""
	if b.Dashboard != nil {
		current, err = b.Dashboard.CurrentID(ctx, groupID)
		if err != nil {
			return nil, false, err
		}
	}
	trip, err := b.Store.GetTrip(ctx, groupID)
	if err != nil {
		return nil, false, err
	}
	writable := trip != nil && current == sessionID && trip.State != models.Cancelled
	if writable {
		for _, member := range expenseMembers(trip) {
			if expenseMember(ledger.Members, member.ID) == nil {
				ledger.Members = append(ledger.Members, member)
			}
		}
	}
	return ledger, writable, nil
}

func expenseView(ledger *models.ExpenseLedger, writable bool) *ExpenseView {
	expenses := []models.Expense{}
	for _, expense := range ledger.Expenses {
		if !expense.Deleted {
			expenses = append(expenses, expense)
		}
	}
	return &ExpenseView{GroupID: ledger.GroupID, SessionID: ledger.SessionID, Revision: ledger.Revision, Writable: writable, Members: append([]models.ExpenseMember{}, ledger.Members...), Expenses: expenses, Balances: tools.ExpenseBalances(ledger)}
}

func (b *Brain) ExpenseView(ctx context.Context, groupID, sessionID string) (*ExpenseView, error) {
	ledger, writable, err := b.expenseLedger(ctx, groupID, sessionID)
	if err != nil {
		return nil, err
	}
	return expenseView(ledger, writable), nil
}

func (b *Brain) saveExpenseLedger(ctx context.Context, ledger *models.ExpenseLedger, operationID, reply string) (*models.ExpenseLedger, error) {
	_, writable, err := b.expenseLedger(ctx, ledger.GroupID, ledger.SessionID)
	if err != nil {
		return nil, err
	}
	if !writable {
		return nil, &DashboardError{Status: 409, Msg: "only the active trip's expenses can be changed"}
	}
	if operationID == "" {
		return nil, &DashboardError{Status: 400, Msg: "an operation ID is required"}
	}
	for _, operation := range ledger.Operations {
		if operation.MessageID == operationID {
			return nil, &DashboardError{Status: 409, Msg: "expense operation already processed"}
		}
	}
	ledger.Operations = append(ledger.Operations, models.ExpenseOperation{MessageID: operationID, Reply: reply})
	updated, err := b.Store.SaveExpenseLedger(ctx, ledger, ledger.Revision)
	if errors.Is(err, store.ErrExpenseConflict) {
		return nil, &DashboardError{Status: 409, Msg: "expenses changed — refresh and try again"}
	}
	return updated, err
}

func buildExpense(ledger *models.ExpenseLedger, actorID, description, amount, payerID string, memberIDs []string) (models.Expense, error) {
	description = strings.TrimSpace(description)
	if description == "" || len(utf16.Encode([]rune(description))) > 200 {
		return models.Expense{}, &DashboardError{Status: 400, Msg: "describe the expense in 1–200 characters"}
	}
	if expenseMember(ledger.Members, actorID) == nil || expenseMember(ledger.Members, payerID) == nil {
		return models.Expense{}, &DashboardError{Status: 400, Msg: "choose a known participant as recorder and payer"}
	}
	for _, id := range memberIDs {
		if expenseMember(ledger.Members, id) == nil {
			return models.Expense{}, &DashboardError{Status: 400, Msg: "choose known participants for the split"}
		}
	}
	cents, err := tools.ParseExpenseAmount(amount)
	if err != nil {
		return models.Expense{}, &DashboardError{Status: 400, Msg: err.Error()}
	}
	shares, err := tools.AllocateExpenseShares(cents, memberIDs)
	if err != nil {
		return models.Expense{}, &DashboardError{Status: 400, Msg: err.Error()}
	}
	return models.Expense{ID: uuid.NewString(), Description: description, AmountCents: cents, Currency: "CAD", PayerID: payerID, Shares: shares, CreatedBy: actorID, CreatedAt: models.Now()}, nil
}

func expenseDescription(ledger *models.ExpenseLedger, expense models.Expense) string {
	name := expense.PayerID
	if member := expenseMember(ledger.Members, name); member != nil {
		name = member.Name
	}
	shares := []string{}
	for _, share := range expense.Shares {
		member := expenseMember(ledger.Members, share.MemberID)
		if member != nil {
			shares = append(shares, member.Name+": "+tools.ExpenseMoney(share.AmountCents))
		}
	}
	return fmt.Sprintf("%s paid %s for %s.\nSplit: %s.", name, tools.ExpenseMoney(expense.AmountCents), expense.Description, strings.Join(shares, "; "))
}

func deleteExpense(ledger *models.ExpenseLedger, id string) (string, error) {
	for i := range ledger.Expenses {
		if ledger.Expenses[i].ID == id && !ledger.Expenses[i].Deleted {
			ledger.Expenses[i].Deleted = true
			return "Removed the recorded expense: " + ledger.Expenses[i].Description + ".", nil
		}
	}
	return "", &DashboardError{Status: 404, Msg: "recorded expense not found"}
}

func (b *Brain) ExpenseAct(ctx context.Context, groupID, sessionID string, body map[string]any) (*ExpenseView, error) {
	groupID = models.CanonicalGroupID(groupID)
	lock := b.lockFor(groupID)
	lock.Lock()
	defer lock.Unlock()
	opID := strAny(body["operation_id"])
	if _, err := uuid.Parse(opID); err != nil {
		return nil, &DashboardError{Status: 400, Msg: "an operation ID is required"}
	}
	opID = "dashboard:" + opID
	previous, err := b.Store.FindExpenseLedgerForOperation(ctx, groupID, opID)
	if err != nil {
		return nil, err
	}
	if previous != nil {
		if previous.SessionID != sessionID {
			return nil, &DashboardError{Status: 409, Msg: "that operation belongs to another trip"}
		}
		_, writable, err := b.expenseLedger(ctx, groupID, sessionID)
		if err != nil {
			return nil, err
		}
		return expenseView(previous, writable), nil
	}
	ledger, writable, err := b.expenseLedger(ctx, groupID, sessionID)
	if err != nil {
		return nil, err
	}
	if !writable {
		return nil, &DashboardError{Status: 409, Msg: "only the active trip's expenses can be changed"}
	}
	revision := numAny(body["expected_revision"])
	if revision == nil || *revision != float64(ledger.Revision) {
		return nil, &DashboardError{Status: 409, Msg: "expenses changed — refresh and try again"}
	}
	actor := strAny(body["actor_id"])
	if expenseMember(ledger.Members, actor) == nil {
		return nil, &DashboardError{Status: 400, Msg: "choose who is recording this expense"}
	}
	var reply string
	switch strAny(body["action"]) {
	case "add":
		ids := []string{}
		for _, raw := range sliceAny(body["member_ids"]) {
			ids = append(ids, strAny(raw))
		}
		expense, err := buildExpense(ledger, actor, strAny(body["description"]), strAny(body["amount"]), strAny(body["payer_id"]), ids)
		if err != nil {
			return nil, err
		}
		ledger.Expenses = append(ledger.Expenses, expense)
		reply = "Recorded expense: " + expenseDescription(ledger, expense)
	case "delete":
		reply, err = deleteExpense(ledger, strAny(body["expense_id"]))
		if err != nil {
			return nil, err
		}
	default:
		return nil, &DashboardError{Status: 400, Msg: "unknown expense action"}
	}
	updated, err := b.saveExpenseLedger(ctx, ledger, opID, reply)
	if err != nil {
		return nil, err
	}
	view := expenseView(updated, true)
	if err := b.say(ctx, groupID, reply, nil); err != nil {
		view.NotificationWarning = "Saved, but the WhatsApp confirmation could not be completed."
	}
	return view, nil
}
