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
		intro = "Here are a few options that fit:"
	}
	lines := []string{intro, ""}
	for _, o := range options {
		lines = append(lines, fmt.Sprintf("%d. %s — %s — about %s each",
			o.Position, o.Destination, Dates(o.EmbarkingDate, o.ReturningDate), Money(o.CostPerPerson)))
		why := strings.TrimSpace(o.WhyItWorks)
		if why != "" {
			lines = append(lines, "   "+why)
		}
		if trade := strings.TrimSpace(o.Tradeoffs); trade != "" {
			lines = append(lines, "   Tradeoff: "+trade)
		}
		lines = append(lines, "")
	}
	lines = append(lines, "Reply 1, 2, or 3 to pick one.")
	if dashboardURL != "" {
		lines = append(lines, "Details: "+DashboardLink(dashboardURL, tripID))
	}
	return strings.Join(lines, "\n")
}

func SummaryMessage(option models.Option, itinerary map[string]any, people []models.Participant, tripID, dashboardURL string) string {
	hotel, _ := itinerary["hotel"].(map[string]any)
	lines := []string{fmt.Sprintf("Plan for %s, %s", option.Destination,
		Dates(option.EmbarkingDate, option.ReturningDate)), ""}

	if flights, ok := itinerary["flights"].(map[string]any); ok {
		if out, ok := flights["embarking"].(map[string]any); ok {
			price := toFloatPtr(out["price"])
			lines = append(lines, fmt.Sprintf("Out: %v to %v on %v, %s", out["origin"], out["destination"], out["airline"], Money(price)))
		}
		if ret, ok := flights["returning"].(map[string]any); ok {
			price := toFloatPtr(ret["price"])
			lines = append(lines, fmt.Sprintf("Back: %v to %v on %v, %s", ret["origin"], ret["destination"], ret["airline"], Money(price)))
		}
	}

	nights := option.DurationNights
	var hotelName any
	var hotelTotal *float64
	if hotel != nil {
		hotelName = hotel["name"]
		hotelTotal = toFloatPtr(hotel["total_price"])
	}
	lines = append(lines, fmt.Sprintf("Hotel: %v, %d nights, %s total", hotelName, nights, Money(hotelTotal)))
	lines = append(lines, "")

	if perPerson, ok := itinerary["per_person"].(map[string]any); ok {
		for _, p := range people {
			total := toFloatPtr(perPerson[p.WhatsAppName])
			lines = append(lines, fmt.Sprintf("%s: %s", p.WhatsAppName, Money(total)))
		}
	}
	lines = append(lines, fmt.Sprintf("Group total: %s", Money(toFloatPtr(itinerary["group_total"]))))
	lines = append(lines, "")
	lines = append(lines, "Reply yes to book, or no to go back to the options.")
	if dashboardURL != "" {
		lines = append(lines, "Details: "+DashboardLink(dashboardURL, tripID))
	}
	return strings.Join(lines, "\n")
}

func ConfirmationMessage(destination, embarkingPNR, returningPNR, hotelRef string, split tools.Split) string {
	lines := []string{fmt.Sprintf("Booked. You're going to %s.", destination), ""}
	lines = append(lines, fmt.Sprintf("Outbound: %s", embarkingPNR))
	lines = append(lines, fmt.Sprintf("Return: %s", returningPNR))
	lines = append(lines, fmt.Sprintf("Hotel: %s", hotelRef))
	if len(split.Owes) > 0 {
		lines = append(lines, "")
		lines = append(lines, "Who owes whom:")
		for _, o := range split.Owes {
			amount := o.Amount
			lines = append(lines, fmt.Sprintf("  %s -> %s: %s", o.From, o.To, Money(&amount)))
		}
	}
	lines = append(lines, "")
	lines = append(lines, "(Sandbox booking. No real money moved.)")
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
