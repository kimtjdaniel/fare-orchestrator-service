// Package formatting builds chat message templates. Built by code (not Gemini) so they're
// instant, cheap, and never malformed.
package formatting

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"fare-brain/models"
	"fare-brain/tools"
)

func Money(x *float64) string {
	if x == nil {
		return "?"
	}
	return fmt.Sprintf("C$%s", commas(*x))
}

func commas(x float64) string {
	s := fmt.Sprintf("%.0f", x)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	if neg {
		s = "-" + s
	}
	return s
}

// Dates formats a start/end range ("YYYY-MM-DD" strings) like "Jan 4–9" or "Jan 28 – Feb 2".
func Dates(startStr, endStr string) string {
	start, err := models.ParseDate(startStr)
	if err != nil {
		return startStr + " – " + endStr
	}
	end, err := models.ParseDate(endStr)
	if err != nil {
		return startStr + " – " + endStr
	}
	if start.Month() == end.Month() {
		return fmt.Sprintf("%s %d–%d", start.Format("Jan"), start.Day(), end.Day())
	}
	return fmt.Sprintf("%s %d – %s %d", start.Format("Jan"), start.Day(), end.Format("Jan"), end.Day())
}

func DashboardLink(dashboardURL, tripID string) string {
	return fmt.Sprintf("%s/trip/%s", dashboardURL, tripID)
}

func OptionsMessage(intro string, options []models.Option, tripID, dashboardURL string) string {
	intro = strings.TrimSpace(intro)
	if intro == "" {
		intro = "These line up with the dates you've got."
	}
	lines := []string{intro, ""}
	for _, o := range options {
		lines = append(lines, fmt.Sprintf("%d. %s, %s, about %s each",
			o.Position, o.Destination, Dates(o.EmbarkingDate, o.ReturningDate), Money(o.CostPerPerson)))
		why := strings.TrimSpace(o.WhyItWorks)
		if why != "" {
			lines = append(lines, why)
		}
		if trade := strings.TrimSpace(o.Tradeoffs); trade != "" {
			lines = append(lines, trade)
		}
		lines = append(lines, "")
	}
	lines = append(lines, "Tap the poll below, or reply 1, 2, or 3.")
	lines = append(lines, "Those prices are rough guesses until I search real fares.")
	return strings.Join(lines, "\n")
}

func SummaryMessage(option models.Option, itinerary map[string]any, people []models.Participant, tripID, dashboardURL string) string {
	hotel, _ := itinerary["hotel"].(map[string]any)
	lines := []string{fmt.Sprintf("%s, %s.", option.Destination,
		Dates(option.EmbarkingDate, option.ReturningDate)), ""}

	if flights, ok := itinerary["flights"].(map[string]any); ok {
		if out, ok := flights["embarking"].(map[string]any); ok {
			price := toFloatPtr(out["price"])
			lines = append(lines, fmt.Sprintf("Out on %v, %v to %v, %s.", out["airline"], out["origin"], out["destination"], Money(price)))
		}
		if ret, ok := flights["returning"].(map[string]any); ok {
			price := toFloatPtr(ret["price"])
			lines = append(lines, fmt.Sprintf("Back on %v, %v to %v, %s.", ret["airline"], ret["origin"], ret["destination"], Money(price)))
		}
	}

	nights := option.DurationNights
	var hotelName any
	var hotelTotal *float64
	if hotel != nil {
		hotelName = hotel["name"]
		hotelTotal = toFloatPtr(hotel["total_price"])
	}
	lines = append(lines, fmt.Sprintf("%v for %d nights, %s total.", hotelName, nights, Money(hotelTotal)))
	lines = append(lines, "")

	if perPerson, ok := itinerary["per_person"].(map[string]any); ok && len(people) > 0 {
		total := toFloatPtr(perPerson[people[0].WhatsAppName])
		if total == nil {
			total = toFloatPtr(itinerary["group_total"])
			if total != nil && len(people) > 0 {
				each := *total / float64(len(people))
				total = &each
			}
		}
		if total != nil {
			lines = append(lines, fmt.Sprintf("About %s each.", Money(total)))
		}
	}
	if gt := toFloatPtr(itinerary["group_total"]); gt != nil {
		lines = append(lines, fmt.Sprintf("Group total %s.", Money(gt)))
	}
	lines = append(lines, "")
	lines = append(lines, "Yes to book it, or no to look at the other options. There's a poll for that too.")
	return strings.Join(lines, "\n")
}

func ConfirmationMessage(destination, embarkingPNR, returningPNR, hotelRef string, split tools.Split) string {
	lines := []string{fmt.Sprintf("You're booked for %s.", destination), ""}
	lines = append(lines, fmt.Sprintf("Outbound %s, return %s.", embarkingPNR, returningPNR))
	lines = append(lines, fmt.Sprintf("Hotel %s.", hotelRef))
	if len(split.Owes) > 0 {
		lines = append(lines, "")
		for _, o := range split.Owes {
			amount := o.Amount
			lines = append(lines, fmt.Sprintf("%s covers %s %s.", o.From, o.To, Money(&amount)))
		}
	}
	lines = append(lines, "")
	lines = append(lines, "That's a sandbox booking — nothing actually charged.")
	return strings.Join(lines, "\n")
}

func IntroMessage(botName string) string {
	if strings.TrimSpace(botName) == "" {
		botName = "Fare"
	}
	return botName + ` here — I sit in this chat and help friends actually pick a trip.

I can:
- gather dates, who flies from where, and a budget
- propose 2-3 destinations, then quote real fares and a stay
- write a day-by-day itinerary
- pick restaurants that match how you eat, with map pins
- send a Google Maps pin for the hotel
- keep a locked quote so I don't invent prices
- ask who's putting the card down and split the rest

Tag me or reply to me. Anytime say "introduce yourself" if you want this recap, "what's locked" for the current quote, or "where should we eat" for dinner spots.`
}

func RestaurantPlan(planned map[string]any, city string) (string, string) {
	intro, _ := planned["intro"].(string)
	var lines []string
	if strings.TrimSpace(intro) != "" {
		lines = append(lines, strings.TrimSpace(intro), "")
	}
	var mapLines []string
	places, _ := planned["places"].([]any)
	for i, raw := range places {
		p, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name, _ := p["name"].(string)
		hood, _ := p["neighborhood"].(string)
		why, _ := p["why"].(string)
		dish, _ := p["dish"].(string)
		if strings.TrimSpace(name) == "" {
			continue
		}
		head := fmt.Sprintf("%d. %s", i+1, strings.TrimSpace(name))
		if strings.TrimSpace(hood) != "" {
			head += " — " + strings.TrimSpace(hood)
		}
		lines = append(lines, head)
		if strings.TrimSpace(why) != "" {
			lines = append(lines, strings.TrimSpace(why))
		}
		if strings.TrimSpace(dish) != "" {
			lines = append(lines, "Try the "+strings.TrimSpace(dish)+".")
		}
		if n := toFloatPtr(p["est_cad"]); n != nil {
			lines = append(lines, "About "+Money(n)+" a person (rough).")
		}
		lines = append(lines, "")
		q := strings.TrimSpace(name + " " + hood + " " + city)
		mapLines = append(mapLines, name+": https://www.google.com/maps/search/?api=1&query="+url.QueryEscape(q))
	}
	maps := ""
	if len(mapLines) > 0 {
		maps = "Maps:\n" + strings.Join(mapLines, "\n")
	}
	return strings.TrimSpace(strings.Join(lines, "\n")), maps
}

func AdvisorItinerary(planned map[string]any) string {
	intro, _ := planned["intro"].(string)
	var lines []string
	if strings.TrimSpace(intro) != "" {
		lines = append(lines, strings.TrimSpace(intro), "")
	}
	days, _ := planned["days"].([]any)
	for i, raw := range days {
		d, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		title, _ := d["title"].(string)
		body, _ := d["body"].(string)
		if strings.TrimSpace(title) == "" {
			title = fmt.Sprintf("Day %d", i+1)
		}
		lines = append(lines, strings.TrimSpace(title))
		if strings.TrimSpace(body) != "" {
			lines = append(lines, strings.TrimSpace(body))
		}
		if n := toFloatPtr(d["food_cad"]); n != nil {
			lines = append(lines, "Meals about "+Money(n)+" each (rough, not booked).")
		}
		lines = append(lines, "")
	}
	foodNote, _ := planned["food_note"].(string)
	dayFood := toFloatPtr(planned["food_per_day_cad"])
	tripFood := toFloatPtr(planned["food_trip_cad"])
	if strings.TrimSpace(foodNote) != "" || dayFood != nil || tripFood != nil {
		lines = append(lines, "Food (rough, not booked):")
		if strings.TrimSpace(foodNote) != "" {
			lines = append(lines, strings.TrimSpace(foodNote))
		}
		if dayFood != nil {
			lines = append(lines, "About "+Money(dayFood)+" per person per day.")
		}
		if tripFood != nil {
			lines = append(lines, "About "+Money(tripFood)+" per person for the whole trip's meals.")
		}
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func LockedTravel(trip *models.Trip) string {
	return LockedStatus(trip)
}

func LockedStatus(trip *models.Trip) string {
	if trip == nil {
		return ""
	}
	var lines []string
	dest := trip.Destination
	embark, retDate := trip.EmbarkingDate, trip.ReturningDate
	if o := trip.ChosenOption(); o != nil {
		if dest == "" {
			dest = o.Destination
		}
		if embark == "" {
			embark, retDate = o.EmbarkingDate, o.ReturningDate
		}
	}
	head := strings.TrimSpace(dest)
	if embark != "" {
		if head != "" {
			head += ", "
		}
		head += Dates(embark, retDate)
	}
	if head != "" {
		lines = append(lines, "Here's what's locked:", head+".")
	} else {
		lines = append(lines, "Here's what's locked:")
	}
	if origin := strings.TrimSpace(trip.Origin); origin != "" {
		lines = append(lines, "Flying out of "+origin+".")
	}

	itin := asMap(trip.Itinerary)
	flights := asMap(itin["flights"])
	out := asMap(flights["embarking"])
	ret := asMap(flights["returning"])
	if out == nil && ret == nil {
		lines = append(lines, "Flights: not searched yet.")
	}
	if out != nil {
		lines = append(lines, fmt.Sprintf("Out: %v, %v → %v, %s.", out["airline"], out["origin"], out["destination"], Money(toFloatPtr(out["price"]))))
	}
	if ret != nil {
		lines = append(lines, fmt.Sprintf("Back: %v, %v → %v, %s.", ret["airline"], ret["origin"], ret["destination"], Money(toFloatPtr(ret["price"]))))
	}

	hotel := asMap(itin["hotel"])
	if hotel != nil && hotel["name"] != nil {
		lines = append(lines, fmt.Sprintf("Stay: %v, %s total (%s a night).",
			hotel["name"], Money(toFloatPtr(hotel["total_price"])), Money(toFloatPtr(hotel["price_per_night"]))))
	}

	if trip.CostPerPerson != nil {
		lines = append(lines, fmt.Sprintf("About %s each.", Money(trip.CostPerPerson)))
	} else if gt := toFloatPtr(itin["group_total"]); gt != nil && len(trip.Participants) > 0 {
		each := *gt / float64(len(trip.Participants))
		lines = append(lines, fmt.Sprintf("About %s each.", Money(&each)))
	}

	switch trip.State {
	case models.AwaitingApproval:
		lines = append(lines, "Waiting on yes/no to book.")
	case models.AwaitingChoice:
		lines = append(lines, "Waiting on a 1, 2, or 3 for the destination.")
	case models.Booked:
		lines = append(lines, "Already booked (sandbox).")
	}
	return strings.Join(lines, "\n")
}

func asMap(v any) map[string]any {
	if v == nil {
		return map[string]any{}
	}
	if m, ok := v.(map[string]any); ok {
		return m
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return map[string]any{}
	}
	return m
}

func OptionPollChoices(options []models.Option) []string {
	out := make([]string, 0, len(options))
	for _, o := range options {
		label := fmt.Sprintf("%s · %s", o.Destination, Dates(o.EmbarkingDate, o.ReturningDate))
		if len(label) > 90 {
			label = o.Destination
		}
		if strings.TrimSpace(label) == "" {
			continue
		}
		out = append(out, label)
	}
	return out
}

func toFloatPtr(v any) *float64 {
	switch n := v.(type) {
	case float64:
		return &n
	case *float64:
		return n
	case int:
		f := float64(n)
		return &f
	default:
		return nil
	}
}
