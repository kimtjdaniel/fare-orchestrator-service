// Package formatting builds chat message templates. Built by code (not Gemini) so they're
// instant, cheap, and never malformed.
package formatting

import (
	"fmt"
	"strings"

	"fare-brain/models"
	"fare-brain/tools"
)

func Money(x *float64) string {
	if x == nil {
		return "?"
	}
	return fmt.Sprintf("$%s", commas(*x))
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
	lines = append(lines, "Which one — 1, 2, or 3?")
	if dashboardURL != "" {
		lines = append(lines, "Details: "+DashboardLink(dashboardURL, tripID))
	}
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
	lines = append(lines, "Yes to book it, or no to look at the other options.")
	if dashboardURL != "" {
		lines = append(lines, "Details: "+DashboardLink(dashboardURL, tripID))
	}
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
