package orchestrator

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"fare-brain/models"
)

var (
	isoDateRe = regexp.MustCompile(`\b(20\d{2}-\d{2}-\d{2})\b`)
	fromIATA  = regexp.MustCompile(`(?i)\b(?:from|out of)\s+([A-Z]{3})\b`)
	monthDate = regexp.MustCompile(`(?i)\b(jan(?:uary)?|feb(?:ruary)?|mar(?:ch)?|apr(?:il)?|may|jun(?:e)?|jul(?:y)?|aug(?:ust)?|sep(?:t(?:ember)?)?|oct(?:ober)?|nov(?:ember)?|dec(?:ember)?)\s+(\d{1,2})(?:st|nd|rd|th)?(?:\s*[–\-to]+\s*(\d{1,2})(?:st|nd|rd|th)?)?`)
	nightsRe  = regexp.MustCompile(`(?i)\b(\d{1,2})\s*-?\s*days?\b`)
)

var monthNum = map[string]time.Month{
	"jan": 1, "january": 1, "feb": 2, "february": 2, "mar": 3, "march": 3,
	"apr": 4, "april": 4, "may": 5, "jun": 6, "june": 6, "jul": 7, "july": 7,
	"aug": 8, "august": 8, "sep": 9, "sept": 9, "september": 9,
	"oct": 10, "october": 10, "nov": 11, "november": 11, "dec": 12, "december": 12,
}

var cityAirport = map[string]string{
	"vancouver": "YVR", "toronto": "YYZ", "calgary": "YYC", "edmonton": "YEG",
	"montreal": "YUL", "ottawa": "YOW", "winnipeg": "YWG", "halifax": "YHZ",
	"victoria": "YYJ", "kelowna": "YLW", "seattle": "SEA", "portland": "PDX",
}

type harvestedFacts struct {
	Dates   []string
	Airport string
	City    string
	Nights  int
}

func harvestFacts(messages []models.Message, today time.Time) harvestedFacts {
	var h harvestedFacts
	for _, msg := range messages {
		if msg.IsBot {
			continue
		}
		h = mergeHarvest(h, harvestText(msg.Text, today))
	}
	return h
}

func harvestText(text string, today time.Time) harvestedFacts {
	var h harvestedFacts
	h.Dates = isoDateRe.FindAllString(text, -1)
	if m := fromIATA.FindStringSubmatch(text); len(m) == 2 {
		h.Airport = strings.ToUpper(m[1])
	}
	if n := durationFromText(text); n > 0 {
		h.Nights = n
	}
	low := strings.ToLower(text)
	for city, code := range cityAirport {
		if strings.Contains(low, city) {
			h.City = city
			h.City = strings.ToUpper(h.City[:1]) + h.City[1:]
			if h.Airport == "" {
				h.Airport = code
			}
			break
		}
	}
	year := today.Year()
	for _, m := range monthDate.FindAllStringSubmatch(text, -1) {
		mon := monthNum[strings.ToLower(m[1])]
		if mon == 0 {
			continue
		}
		d1, _ := strconv.Atoi(m[2])
		h.Dates = append(h.Dates, fmt.Sprintf("%d-%02d-%02d", year, int(mon), d1))
		if m[3] != "" {
			d2, _ := strconv.Atoi(m[3])
			h.Dates = append(h.Dates, fmt.Sprintf("%d-%02d-%02d", year, int(mon), d2))
		}
	}
	h.Dates = unionDates(nil, h.Dates)
	return h
}

func mergeHarvest(a, b harvestedFacts) harvestedFacts {
	a.Dates = unionDates(a.Dates, b.Dates)
	if b.Airport != "" {
		a.Airport = b.Airport
	}
	if b.City != "" {
		a.City = b.City
	}
	if b.Nights > 0 {
		a.Nights = b.Nights
	}
	return a
}

func durationFromText(text string) int {
	m := nightsRe.FindStringSubmatch(text)
	if len(m) < 2 {
		return 0
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n < 2 || n > 21 {
		return 0
	}
	return n
}

func applyHarvest(people []models.Participant, h harvestedFacts, roster []models.GroupMember) []models.Participant {
	if len(people) == 0 {
		for _, m := range roster {
			name := strings.TrimSpace(m.Name)
			if name == "" || m.IsAgent || looksLikeWhatsAppID(name) {
				continue
			}
			people = append(people, models.Participant{WhatsAppName: name})
		}
	}
	if h.Airport != "" || h.City != "" {
		for i := range people {
			if people[i].OriginAirport == "" {
				people[i].OriginAirport = h.Airport
			}
			if people[i].OriginCity == "" {
				people[i].OriginCity = h.City
			}
			if people[i].Origin == "" {
				people[i].Origin = h.City
				if people[i].Origin == "" {
					people[i].Origin = h.Airport
				}
			}
		}
	}
	if len(h.Dates) > 0 {
		for i := range people {
			people[i].GeneralPreferences.Availability = unionDates(people[i].GeneralPreferences.Availability, h.Dates)
		}
	}
	if h.Nights > 0 {
		for i := range people {
			if people[i].GeneralPreferences.DurationNights == 0 {
				people[i].GeneralPreferences.DurationNights = h.Nights
			}
		}
	}
	return applySharedOrigin(people)
}

func allStoredDates(people []models.Participant) []string {
	var out []string
	for _, p := range people {
		out = unionDates(out, p.GeneralPreferences.Availability)
	}
	return out
}

func enoughToPlan(people []models.Participant) bool {
	return hasSharedOrigin(people) && hasAnyDates(people)
}
