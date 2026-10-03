package tools

import "math"

type Owes struct {
	From   string  `json:"from"`
	To     string  `json:"to"`
	Amount float64 `json:"amount"`
}

type Split struct {
	PerPerson  map[string]float64 `json:"per_person"`
	Owes       []Owes             `json:"owes"`
	GroupTotal float64            `json:"group_total"`
}

func round2(x float64) float64 {
	return math.Round(x*100) / 100
}

// ComputeSplit: the group books one shared flight itinerary (same origin, same dates) and one
// shared hotel. Everyone pays an equal share of the flights (priced per person) plus an equal
// share of the hotel. The agent charged everything to payer's card, so everyone else owes payer.
func ComputeSplit(names []string, flightPricePerPerson, hotelTotal float64, payer string) Split {
	if len(names) == 0 {
		return Split{PerPerson: map[string]float64{}, Owes: nil, GroupTotal: 0}
	}
	hotelShare := round2(hotelTotal / float64(len(names)))
	perPerson := make(map[string]float64, len(names))
	var sumPerPerson float64
	for _, n := range names {
		perPerson[n] = round2(flightPricePerPerson + hotelShare)
		sumPerPerson += perPerson[n]
	}
	groupTotal := round2(flightPricePerPerson*float64(len(names)) + hotelTotal)
	// rounding remainder (pennies) goes to the payer so the totals match exactly
	if _, ok := perPerson[payer]; ok {
		perPerson[payer] = round2(perPerson[payer] + groupTotal - sumPerPerson)
	}
	var owes []Owes
	for _, n := range names {
		if n != payer {
			owes = append(owes, Owes{From: n, To: payer, Amount: perPerson[n]})
		}
	}
	return Split{PerPerson: perPerson, Owes: owes, GroupTotal: groupTotal}
}
