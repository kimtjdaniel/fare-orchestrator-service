package orchestrator

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"fare-brain/llm"
	"fare-brain/models"
	"fare-brain/prompts"
	"fare-brain/tools"

	"github.com/google/uuid"
)

var expenseMessageRe = regexp.MustCompile(`(?i)\b(expenses?|balances?|paid|spent|covered|record)\b|who owes|owe what|split (?:this|that|it|with|between|equally|among)\b`)
var expenseReplyRe = regexp.MustCompile(`(?i)\b(yes|yeah|yep|sure|save|record|delete|remove|no|nah|nope|cancel|keep|correct|actually|instead|amount|split)\b`)
var expenseMutationRe = regexp.MustCompile(`(?i)\b(paid|spent|covered|remove|delete|record|add)\b|split (?:this|that|it|with|between|equally|among)\b`)
var expenseListRe = regexp.MustCompile(`(?i)\b(show|list)\b.*\bexpenses\b|who owes|owe what|\bbalances?\b`)

// Pending expense replies must retain their original sender and message ID.
func (b *Brain) hasPendingExpense(ctx context.Context, groupID string) bool {
	if b.Dashboard == nil {
		return false
	}
	sid, err := b.Dashboard.CurrentID(ctx, groupID)
	if err != nil || sid == "" {
		return false
	}
	ledger, err := b.Store.GetExpenseLedger(ctx, groupID, sid)
	return err == nil && ledger != nil && len(ledger.Drafts) > 0
}

func expenseActor(members []models.ExpenseMember, m models.IncomingMessage) string {
	if member := expenseMember(members, m.SenderID); member != nil {
		return member.ID
	}
	id := ""
	for _, member := range members {
		if strings.EqualFold(member.Name, strings.TrimSpace(m.SenderName)) {
			if id != "" {
				return ""
			}
			id = member.ID
		}
	}
	return id
}

func expenseDraft(ledger *models.ExpenseLedger, actor string) *models.ExpenseDraft {
	for i := range ledger.Drafts {
		if ledger.Drafts[i].ActorID == actor {
			return &ledger.Drafts[i]
		}
	}
	return nil
}

func removeExpenseDraft(ledger *models.ExpenseLedger, actor string) {
	drafts := []models.ExpenseDraft{}
	for _, draft := range ledger.Drafts {
		if draft.ActorID != actor {
			drafts = append(drafts, draft)
		}
	}
	ledger.Drafts = drafts
}

func expenseDraftMessage(ledger *models.ExpenseLedger, draft models.ExpenseDraft) string {
	if draft.Action == "incomplete" {
		return "Expense details needed: " + draft.Question + "\nExpense ref: " + draft.ID
	}
	if draft.Action == "delete" {
		for _, expense := range ledger.Expenses {
			if expense.ID == draft.DeleteExpenseID {
				return "Expense to remove: " + expenseDescription(ledger, expense) + "\nRemove this recorded expense?\nExpense ref: " + draft.ID
			}
		}
	}
	return "Expense to save: " + expenseDescription(ledger, draft.Expense) + "\nSave this expense? You can reply naturally, or tell me what needs correcting.\nExpense ref: " + draft.ID
}

func recordedExpenseSummary(ledger *models.ExpenseLedger) string {
	lines := []string{"Recorded expenses · CAD (repayments aren't tracked yet):"}
	count := 0
	for _, expense := range ledger.Expenses {
		if expense.Deleted {
			continue
		}
		count++
		lines = append(lines, expenseDescription(ledger, expense))
	}
	if count == 0 {
		return "No expenses have been recorded for this trip yet. Travel quotes are shown separately."
	}
	lines = append(lines, "", "Net balances:")
	for _, balance := range tools.ExpenseBalances(ledger) {
		line := balance.Name + ": " + tools.ExpenseMoney(0)
		if balance.BalanceCents > 0 {
			line = balance.Name + " gets back " + tools.ExpenseMoney(balance.BalanceCents)
		}
		if balance.BalanceCents < 0 {
			line = balance.Name + " owes " + tools.ExpenseMoney(-balance.BalanceCents)
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// Expense processing precedes the general message dedupe. Its atomic ledger operation
// receipt lets a failed save be retried and a completed save be replayed across sessions.
func (b *Brain) handleExpenseMessage(ctx context.Context, trip *models.Trip, m models.IncomingMessage) (bool, error) {
	text := strings.TrimSpace(b.mentionRe.ReplaceAllString(m.Text, ""))
	quote := quotedText(m)
	isQuote := m.Quoted != nil && m.Quoted.FromMe && (strings.HasPrefix(quote, "Expense to save:") || strings.HasPrefix(quote, "Expense to remove:") || strings.HasPrefix(quote, "Expense details needed:"))
	isRequest := expenseMessageRe.MatchString(text) && (m.Tagged || looksLikeDirectQuestion(text))
	opID := ""
	if m.MessageID != "" {
		opID = "whatsapp:" + m.MessageID
	}
	if opID != "" {
		previous, err := b.Store.FindExpenseLedgerForOperation(ctx, m.GroupID, opID)
		if err != nil {
			if !isRequest && !isQuote {
				return false, nil
			}
			return b.expenseChatReply(ctx, trip, m, "I couldn’t load the expense records. Please try again.")
		}
		if previous != nil {
			for _, operation := range previous.Operations {
				if operation.MessageID == opID {
					return b.expenseChatReply(ctx, trip, m, operation.Reply)
				}
			}
		}
	}
	if looksLikeNewTripRequest(text) && !isQuote {
		return false, nil
	}
	if trip == nil || b.Dashboard == nil {
		if isRequest || isQuote {
			return b.expenseChatReply(ctx, trip, m, "Start a trip first, then I can record its expenses.")
		}
		return false, nil
	}
	sid, err := b.Dashboard.CurrentID(ctx, trip.GroupID)
	if err != nil || sid == "" {
		if isRequest || isQuote {
			return b.expenseChatReply(ctx, trip, m, "I couldn’t find the active trip for these expenses. Please try again once a trip is started.")
		}
		return false, nil
	}
	ledger, writable, err := b.expenseLedger(ctx, trip.GroupID, sid)
	if err != nil {
		if !isRequest && !isQuote {
			return false, nil
		}
		return b.expenseChatReply(ctx, trip, m, "I couldn’t load the expense records. Please try again.")
	}
	if isRequest && expenseListRe.MatchString(text) && !expenseMutationRe.MatchString(text) {
		return b.expenseChatReply(ctx, trip, m, recordedExpenseSummary(ledger))
	}
	actor := expenseActor(ledger.Members, m)
	draft := expenseDraft(ledger, actor)
	if !isRequest && !isQuote && (draft == nil || m.Quoted != nil) {
		return false, nil
	}
	// A duplicate ordinary chat message cannot become a fresh expense confirmation.
	// Failed expense attempts remain retryable because their saved message is marked.
	if m.MessageID != "" {
		previous, err := b.Store.GetMessageByExternalID(ctx, m.GroupID, m.MessageID)
		if err != nil {
			return b.expenseChatReply(ctx, trip, m, "I couldn’t check that expense reply. Please try again.")
		}
		if previous != nil && !previous.ExpenseMessage {
			return true, nil
		}
	}
	if isQuote && (draft == nil || quote != scrubWhatsAppIDs(expenseDraftMessage(ledger, *draft))) {
		return b.expenseChatReply(ctx, trip, m, "That expense confirmation is no longer current for you. Ask me to record or remove the expense again.")
	}
	if !writable {
		return b.expenseChatReply(ctx, trip, m, "This trip's expense records are read-only. Start a new trip to record new expenses.")
	}
	if actor == "" {
		return b.expenseChatReply(ctx, trip, m, "I can’t match you to a unique trip participant yet. Check the trip participants on the dashboard before recording an expense.")
	}
	if m.Timestamp != 0 && trip.HistoryStart != nil && m.Timestamp < trip.HistoryStart.Unix() {
		return b.expenseChatReply(ctx, trip, m, "That message belongs to an earlier trip. Repeat the expense request for the current trip.")
	}
	history, err := b.Store.GetMessages(ctx, trip.GroupID, trip.HistoryStart, 8, true)
	if err != nil {
		return b.expenseChatReply(ctx, trip, m, "I couldn’t read the expense conversation. Please try again.")
	}
	activityPending := models.ScheduleItinerary(trip, trip.Itinerary, false)["pending_activity_replacement"] != nil
	if draft != nil && activityPending && !isQuote && !expenseMessageRe.MatchString(text) && expenseReplyRe.MatchString(text) {
		return b.expenseChatReply(ctx, trip, m, "Do you mean the expense draft or the activity suggestion? You can reply to the one you mean.")
	}
	input, err := json.Marshal(map[string]any{"latest_message": text, "sender_id": actor, "members": ledger.Members, "expenses": expenseView(ledger, writable).Expenses, "pending_draft": draft, "quoted_message": quote, "recent_chat": compactChat(history, 8), "activity_suggestion_pending": activityPending})
	if err != nil {
		return b.expenseChatReply(ctx, trip, m, "I couldn’t read that expense request. Please try again.")
	}
	out, err := b.LLM.Structured(ctx, prompts.ExpenseSystem, []llm.Message{{Role: "user", Content: string(input)}}, toSchema(prompts.ExpenseCommand))
	if err != nil {
		return b.expenseChatReply(ctx, trip, m, "I couldn’t understand that expense request. Please repeat who paid, how much, what for and who is splitting it.")
	}
	action := strAny(out["action"])
	if action == "unrelated" {
		return false, nil
	}
	if opID == "" {
		opID = "whatsapp:" + uuid.NewString()
	}
	var reply string
	switch action {
	case "clarify", "":
		question := orStr(strAny(out["clarification"]), "Who paid, how much in CAD, what for and who should share it?")
		if draft != nil && draft.Action != "incomplete" {
			return b.expenseChatReply(ctx, trip, m, question)
		}
		request := text
		if draft != nil {
			request = draft.Request + "\nFollow-up: " + text
		}
		if len(request) > 4000 {
			return b.expenseChatReply(ctx, trip, m, "Please repeat the expense in one message with who paid, the CAD amount, what for and who shares it.")
		}
		removeExpenseDraft(ledger, actor)
		pending := models.ExpenseDraft{ID: uuid.NewString(), ActorID: actor, Action: "incomplete", Request: request, Question: question, SourceMessageID: m.MessageID, CreatedAt: models.Now()}
		ledger.Drafts = append(ledger.Drafts, pending)
		reply = expenseDraftMessage(ledger, pending)
	case "list":
		reply = recordedExpenseSummary(ledger)
	case "add":
		ids := []string{}
		for _, id := range sliceAny(out["member_ids"]) {
			ids = append(ids, strAny(id))
		}
		expense, err := buildExpense(ledger, actor, strAny(out["description"]), strAny(out["amount"]), strAny(out["payer_id"]), ids)
		if err != nil {
			return b.expenseChatReply(ctx, trip, m, err.Error())
		}
		removeExpenseDraft(ledger, actor)
		pending := models.ExpenseDraft{ID: uuid.NewString(), ActorID: actor, Action: "add", Expense: expense, SourceMessageID: m.MessageID, CreatedAt: models.Now()}
		ledger.Drafts = append(ledger.Drafts, pending)
		reply = expenseDraftMessage(ledger, pending)
	case "delete":
		id := strAny(out["expense_id"])
		found := false
		for _, expense := range ledger.Expenses {
			if expense.ID == id && !expense.Deleted {
				found = true
			}
		}
		if !found {
			return b.expenseChatReply(ctx, trip, m, "Which recorded expense should I remove? Tell me its description and payer.")
		}
		removeExpenseDraft(ledger, actor)
		pending := models.ExpenseDraft{ID: uuid.NewString(), ActorID: actor, Action: "delete", DeleteExpenseID: id, SourceMessageID: m.MessageID, CreatedAt: models.Now()}
		ledger.Drafts = append(ledger.Drafts, pending)
		reply = expenseDraftMessage(ledger, pending)
	case "accept", "reject":
		if draft == nil || (action == "accept" && draft.Action == "incomplete") {
			return b.expenseChatReply(ctx, trip, m, "There isn't an expense waiting for your confirmation.")
		}
		if m.Timestamp != 0 && m.Timestamp < draft.CreatedAt.Unix() {
			return b.expenseChatReply(ctx, trip, m, "That reply predates this expense confirmation. Please answer the current confirmation.")
		}
		if action == "reject" {
			reply = "Dismissed your expense draft. No recorded expense was changed."
		} else if draft.Action == "delete" {
			reply, err = deleteExpense(ledger, draft.DeleteExpenseID)
			if err != nil {
				return b.expenseChatReply(ctx, trip, m, err.Error())
			}
		} else {
			ledger.Expenses = append(ledger.Expenses, draft.Expense)
			reply = "Recorded expense: " + expenseDescription(ledger, draft.Expense)
		}
		removeExpenseDraft(ledger, actor)
	default:
		return b.expenseChatReply(ctx, trip, m, "Did you want to add, list or remove a recorded expense?")
	}
	if _, err := b.saveExpenseLedger(ctx, ledger, opID, reply); err != nil {
		if dash, ok := err.(*DashboardError); ok {
			return b.expenseChatReply(ctx, trip, m, dash.Msg)
		}
		return b.expenseChatReply(ctx, trip, m, "I couldn’t save that expense change. Please try again; don't assume it was recorded yet.")
	}
	return b.expenseChatReply(ctx, trip, m, reply)
}

func (b *Brain) expenseChatReply(ctx context.Context, trip *models.Trip, m models.IncomingMessage, text string) (bool, error) {
	tripID := ""
	if trip != nil {
		tripID = trip.ID
	}
	sentAt := models.Now()
	if m.Timestamp != 0 {
		sentAt = time.Unix(m.Timestamp, 0).UTC()
	}
	_, _ = b.Store.SaveMessage(ctx, &models.Message{GroupID: m.GroupID, TripID: tripID, ExternalID: m.MessageID, ExpenseMessage: true, SenderID: m.SenderID, SenderName: m.SenderName, Text: m.Text, Tagged: m.Tagged, SentAt: sentAt})
	// The saved ledger, not message delivery, determines whether an expense succeeded.
	_ = b.say(ctx, m.GroupID, text, nil)
	return true, nil
}
