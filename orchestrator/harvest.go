package orchestrator

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"fare-brain/models"
)

var (
	isoDateRe = regexp.MustCompile(`\b(20\d{2}-\d{2}-\d{2})\b`)
	fromIATA  = regexp.MustCompile(`(?i)\b(?:from|out of)\s+([A-Z]{3})\b`)
	parenIATA = regexp.MustCompile(`\(([A-Za-z]{3})\)`)
	monthDate = regexp.MustCompile(`(?i)\b(jan(?:uary)?|feb(?:ruary)?|mar(?:ch)?|apr(?:il)?|may|jun(?:e)?|jul(?:y)?|aug(?:ust)?|sep(?:t(?:ember)?)?|oct(?:ober)?|nov(?:ember)?|dec(?:ember)?)\s+(\d{1,2})(?:st|nd|rd|th)?(?:\s*[–\-to]+\s*(\d{1,2})(?:st|nd|rd|th)?)?`)
	dateSpanRe = regexp.MustCompile(`(?i)\b(jan(?:uary)?|feb(?:ruary)?|mar(?:ch)?|apr(?:il)?|may|jun(?:e)?|jul(?:y)?|aug(?:ust)?|sep(?:t(?:ember)?)?|oct(?:ober)?|nov(?:ember)?|dec(?:ember)?)\s+(\d{1,2})(?:st|nd|rd|th)?\s*(?:to|through|until|–|-)\s*(jan(?:uary)?|feb(?:ruary)?|mar(?:ch)?|apr(?:il)?|may|jun(?:e)?|jul(?:y)?|aug(?:ust)?|sep(?:t(?:ember)?)?|oct(?:ober)?|nov(?:ember)?|dec(?:ember)?)\s+(\d{1,2})(?:st|nd|rd|th)?`)
	nightsRe  = regexp.MustCompile(`(?i)\b(\d{1,2})\s*-?\s*(?:days?|nights?)\b`)
	destToRe  = regexp.MustCompile(`(?i)\b(?:going to|go to|fly(?:ing)? to|change (?:the )?destination to)\s+([A-Za-z][A-Za-z]+(?:\s+[A-Za-z][A-Za-z]+)?)`)
	tripPlaceRe = regexp.MustCompile(`(?i)\b(?:plan|book|arrange)\s+(?:a\s+|an\s+)?(?:\d{1,2}\s*-?\s*(?:night|day)s?\s+)?([A-Za-z][A-Za-z]+(?:\s+[A-Za-z][A-Za-z]+)?)\s+trip\b`)
	tripToPlaceRe = regexp.MustCompile(`(?i)\b(?:trip to|visit)\s+([A-Za-z][A-Za-z]+(?:\s+[A-Za-z][A-Za-z]+)?)`)
	insteadToRe = regexp.MustCompile(`(?i)\b(?:to|for)\s+([A-Za-z][A-Za-z]+(?:\s+[A-Za-z][A-Za-z]+)?)\s+instead\b`)
	budgetRe  = regexp.MustCompile(`(?i)(?:c\$|cad\s*\$?|\$)\s*([\d,]+)|budget[^\d]{0,12}([\d,]+)`)
	monthOnlyRe = regexp.MustCompile(`(?i)\b(?:next\s+)?(jan(?:uary)?|feb(?:ruary)?|mar(?:ch)?|apr(?:il)?|may|jun(?:e)?|jul(?:y)?|aug(?:ust)?|sep(?:t(?:ember)?)?|oct(?:ober)?|nov(?:ember)?|dec(?:ember)?)\b`)
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
	Dates       []string
	Airport     string
	City        string
	Nights      int
	Destination string
	Budget      string
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
	if h.Airport == "" {
		if m := parenIATA.FindStringSubmatch(text); len(m) == 2 {
			h.Airport = strings.ToUpper(m[1])
		}
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
	for _, m := range dateSpanRe.FindAllStringSubmatch(text, -1) {
		mon1 := monthNum[strings.ToLower(m[1])]
		mon2 := monthNum[strings.ToLower(m[3])]
		d1, _ := strconv.Atoi(m[2])
		d2, _ := strconv.Atoi(m[4])
		if mon1 > 0 && mon2 > 0 && d1 > 0 && d2 > 0 {
			start := time.Date(year, mon1, d1, 0, 0, 0, 0, time.UTC)
			end := time.Date(year, mon2, d2, 0, 0, 0, 0, time.UTC)
			if end.Before(start) {
				end = end.AddDate(1, 0, 0)
			}
			h.Dates = unionDates(h.Dates, dateList(start, end))
		}
	}
	for _, m := range monthDate.FindAllStringSubmatch(text, -1) {
		mon := monthNum[strings.ToLower(m[1])]
		if mon == 0 {
			continue
		}
		d1, _ := strconv.Atoi(m[2])
		start := time.Date(year, mon, d1, 0, 0, 0, 0, time.UTC)
		h.Dates = append(h.Dates, start.Format("2006-01-02"))
		if m[3] != "" {
			d2, _ := strconv.Atoi(m[3])
			end := time.Date(year, mon, d2, 0, 0, 0, 0, time.UTC)
			if !end.Before(start) {
				h.Dates = unionDates(h.Dates, dateList(start, end))
			}
		}
	}
	h.Dates = fillDateRange(h.Dates)
	if len(h.Dates) == 0 && strings.Contains(low, "weekend") {
		start, end := comingWeekend(today)
		if strings.Contains(low, "next weekend") {
			start = start.AddDate(0, 0, 7)
			end = end.AddDate(0, 0, 7)
		}
		h.Dates = dateList(start, end)
	}
	if len(h.Dates) == 0 {
		h.Dates = monthOnlyDates(text, today)
	}
	if dest := parseDestination(text); dest != "" {
		h.Destination = dest
	}
	if bgt := parseBudget(text); bgt != "" {
		h.Budget = bgt
	}
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
	if b.Destination != "" {
		a.Destination = b.Destination
	}
	if b.Budget != "" {
		a.Budget = b.Budget
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
			if len(people[i].GeneralPreferences.Availability) > 0 {
				continue
			}
			people[i].GeneralPreferences.Availability = unionDates(nil, h.Dates)
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

func applyHarvestForced(people []models.Participant, h harvestedFacts, roster []models.GroupMember) []models.Participant {
	if len(people) == 0 {
		people = applyHarvest(nil, harvestedFacts{}, roster)
	}
	if h.Airport != "" || h.City != "" {
		for i := range people {
			if h.Airport != "" {
				people[i].OriginAirport = h.Airport
			}
			if h.City != "" {
				people[i].OriginCity = h.City
				people[i].Origin = h.City
			} else if h.Airport != "" {
				people[i].Origin = h.Airport
			}
		}
	}
	if len(h.Dates) > 0 {
		for i := range people {
			people[i].GeneralPreferences.Availability = append([]string(nil), h.Dates...)
		}
	}
	if h.Nights > 0 {
		for i := range people {
			people[i].GeneralPreferences.DurationNights = h.Nights
		}
	}
	return applySharedOrigin(people)
}

func fillDateRange(dates []string) []string {
	dates = unionDates(nil, dates)
	if len(dates) < 2 {
		return dates
	}
	sort.Strings(dates)
	start, err1 := models.ParseDate(dates[0])
	end, err2 := models.ParseDate(dates[len(dates)-1])
	if err1 != nil || err2 != nil {
		return dates
	}
	return dateList(start, end)
}

func parseDestination(text string) string {
	if m := tripPlaceRe.FindStringSubmatch(text); len(m) == 2 {
		if dest := cleanPlaceName(m[1]); dest != "" {
			return dest
		}
	}
	low := strings.ToLower(text)
	if strings.Contains(low, "italy") || strings.Contains(low, "italia") {
		if strings.Contains(low, "south") {
			return "Southern Italy"
		}
		return "Italy"
	}
	if m := destToRe.FindStringSubmatch(text); len(m) == 2 {
		if dest := cleanPlaceName(m[1]); dest != "" {
			return dest
		}
	}
	if m := insteadToRe.FindStringSubmatch(text); len(m) == 2 {
		if dest := cleanPlaceName(m[1]); dest != "" {
			return dest
		}
	}
	if m := tripToPlaceRe.FindStringSubmatch(text); len(m) == 2 {
		if dest := cleanPlaceName(m[1]); dest != "" {
			return dest
		}
	}
	for _, city := range []string{"paris", "lisbon", "london", "rome", "barcelona", "madrid", "berlin", "amsterdam", "prague", "tokyo", "seoul", "tel aviv"} {
		if strings.Contains(low, city) {
			return strings.ToUpper(city[:1]) + city[1:]
		}
	}
	return ""
}

func monthOnlyDates(text string, today time.Time) []string {
	low := strings.ToLower(text)
	m := monthOnlyRe.FindStringSubmatch(text)
	if len(m) < 2 {
		return nil
	}
	mon := monthNum[strings.ToLower(m[1])]
	if mon == 0 {
		return nil
	}
	year := today.Year()
	if today.Month() > mon || strings.Contains(low, "next") {
		year++
	}
	start := time.Date(year, mon, 1, 0, 0, 0, 0, time.UTC)
	return dateList(start, start.AddDate(0, 0, 9))
}

func parseBudget(text string) string {
	m := budgetRe.FindStringSubmatch(text)
	if len(m) == 0 {
		return ""
	}
	n := strings.TrimSpace(m[1])
	if n == "" && len(m) > 2 {
		n = strings.TrimSpace(m[2])
	}
	n = strings.ReplaceAll(n, ",", "")
	if n == "" {
		return ""
	}
	return "C$" + n
}

func cleanPlaceName(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	low := strings.ToLower(s)
	if monthNum[low] != 0 {
		return ""
	}
	if fields := strings.Fields(low); len(fields) > 0 && monthNum[fields[0]] != 0 {
		return ""
	}
	skip := map[string]bool{
		"the": true, "our": true, "this": true, "that": true, "instead": true,
		"dates": true, "date": true, "budget": true, "origin": true, "flight": true, "flights": true,
		"option": true, "options": true, "search": true, "stay": true, "hotel": true, "hotels": true,
		"vibe": true, "trip": true, "city": true, "cities": true, "group": true, "everyone": true,
		"pricing": true, "price": true, "fare": true, "fares": true, "place": true, "places": true,
	}
	if skip[low] {
		return ""
	}
	return s
}

func datesEqual(a, b []string) bool {
	a = unionDates(nil, a)
	b = unionDates(nil, b)
	if len(a) != len(b) {
		return false
	}
	sort.Strings(a)
	sort.Strings(b)
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
