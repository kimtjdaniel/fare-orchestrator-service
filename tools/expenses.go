package tools

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"fare-brain/models"
)

// MaxExpenseAmountCents caps each v1 expense at CAD 1,000,000.
const MaxExpenseAmountCents int64 = 100000000

// ParseExpenseAmount accepts a plain positive CAD decimal, without currency symbols,
// grouping separators, exponents or rounding. Money never passes through a float.
func ParseExpenseAmount(raw string) (int64, error) {
	parts := strings.Split(strings.TrimSpace(raw), ".")
	if len(parts) > 2 || parts[0] == "" {
		return 0, fmt.Errorf("enter a positive CAD amount with at most two decimal places")
	}
	for _, part := range parts {
		if part == "" {
			return 0, fmt.Errorf("enter a positive CAD amount with at most two decimal places")
		}
		for _, digit := range part {
			if digit < '0' || digit > '9' {
				return 0, fmt.Errorf("enter a plain CAD amount, such as 84.50")
			}
		}
	}
	whole, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || whole > MaxExpenseAmountCents/100 {
		return 0, fmt.Errorf("expense must be at most CAD 1,000,000")
	}
	var fraction int64
	if len(parts) == 2 {
		if len(parts[1]) > 2 {
			return 0, fmt.Errorf("CAD amounts allow at most two decimal places")
		}
		fraction, _ = strconv.ParseInt(parts[1], 10, 64)
		if len(parts[1]) == 1 {
			fraction *= 10
		}
	}
	amount := whole*100 + fraction
	if amount <= 0 || amount > MaxExpenseAmountCents {
		return 0, fmt.Errorf("expense must be greater than zero and at most CAD 1,000,000")
	}
	return amount, nil
}

func AllocateExpenseShares(amount int64, memberIDs []string) ([]models.ExpenseShare, error) {
	if amount <= 0 || amount > MaxExpenseAmountCents || len(memberIDs) == 0 {
		return nil, fmt.Errorf("expense needs a positive CAD amount and at least one member")
	}
	ids := append([]string(nil), memberIDs...)
	sort.Strings(ids)
	for i, id := range ids {
		if strings.TrimSpace(id) == "" || id != strings.TrimSpace(id) || (i > 0 && id == ids[i-1]) {
			return nil, fmt.Errorf("expense members must have unique, nonempty IDs")
		}
	}
	count := int64(len(ids))
	shares := make([]models.ExpenseShare, len(ids))
	for i, id := range ids {
		cents := amount / count
		if int64(i) < amount%count {
			cents++
		}
		shares[i] = models.ExpenseShare{MemberID: id, AmountCents: cents}
	}
	return shares, nil
}

// Positive balances mean the member paid more than their share of recorded expenses.
// These balances do not account for repayments.
func ExpenseBalances(ledger *models.ExpenseLedger) []models.ExpenseBalance {
	balances := []models.ExpenseBalance{}
	if ledger == nil {
		return balances
	}
	byID := map[string]int{}
	ensure := func(id, name string) int {
		if i, ok := byID[id]; ok {
			return i
		}
		i := len(balances)
		byID[id] = i
		balances = append(balances, models.ExpenseBalance{MemberID: id, Name: name})
		return i
	}
	for _, member := range ledger.Members {
		ensure(member.ID, member.Name)
	}
	for _, expense := range ledger.Expenses {
		if expense.Deleted {
			continue
		}
		payer := ensure(expense.PayerID, expense.PayerID)
		balances[payer].BalanceCents += expense.AmountCents
		for _, share := range expense.Shares {
			member := ensure(share.MemberID, share.MemberID)
			balances[member].BalanceCents -= share.AmountCents
		}
	}
	return balances
}

func ExpenseMoney(cents int64) string {
	sign := ""
	var magnitude uint64
	if cents < 0 {
		sign = "-"
		magnitude = uint64(-(cents + 1)) + 1
	} else {
		magnitude = uint64(cents)
	}
	return fmt.Sprintf("%sC$%d.%02d", sign, magnitude/100, magnitude%100)
}
