package orchestrator

import (
	"encoding/json"
	"strings"

	"fare-brain/models"
)

func mergeParticipants(existing, extracted []models.Participant) []models.Participant {
	if len(existing) == 0 {
		return applySharedOrigin(extracted)
	}
	if len(extracted) == 0 {
		return applySharedOrigin(existing)
	}
	out := make([]models.Participant, 0, len(existing)+len(extracted))
	used := map[string]bool{}
	for _, neu := range extracted {
		if hit := matchName(neu.WhatsAppName, existing); hit != nil {
			used[strings.ToLower(strings.TrimSpace(hit.WhatsAppName))] = true
			out = append(out, overlayParticipant(*hit, neu))
			continue
		}
		out = append(out, neu)
	}
	for _, old := range existing {
		key := strings.ToLower(strings.TrimSpace(old.WhatsAppName))
		if key == "" || used[key] {
			continue
		}
		out = append(out, old)
	}
	return applySharedOrigin(out)
}

func overlayParticipant(base, neu models.Participant) models.Participant {
	if neu.PID != "" {
		base.PID = neu.PID
	}
	if strings.TrimSpace(neu.WhatsAppName) != "" {
		base.WhatsAppName = neu.WhatsAppName
	}
	if neu.Payer {
		base.Payer = true
	}
	if neu.UploadedPaymentInfo {
		base.UploadedPaymentInfo = true
	}
	if neu.UploadedPassportInfo {
		base.UploadedPassportInfo = true
	}
	if neu.OriginCity != "" {
		base.OriginCity = neu.OriginCity
	}
	if neu.OriginAirport != "" {
		base.OriginAirport = neu.OriginAirport
	}
	if neu.Origin != "" {
		base.Origin = neu.Origin
	}
	if neu.GeneralPreferences.DurationNights != 0 {
		base.GeneralPreferences.DurationNights = neu.GeneralPreferences.DurationNights
	}
	if neu.GeneralPreferences.Activity != "" {
		base.GeneralPreferences.Activity = neu.GeneralPreferences.Activity
	}
	if neu.GeneralPreferences.Culinary != "" {
		base.GeneralPreferences.Culinary = neu.GeneralPreferences.Culinary
	}
	base.GeneralPreferences.Availability = unionDates(base.GeneralPreferences.Availability, neu.GeneralPreferences.Availability)
	if neu.FlightPreferences.IsDirect {
		base.FlightPreferences.IsDirect = true
	}
	if neu.FlightPreferences.Class != "" {
		base.FlightPreferences.Class = neu.FlightPreferences.Class
	}
	if neu.AccommodationPreferences.Type != "" {
		base.AccommodationPreferences.Type = neu.AccommodationPreferences.Type
	}
	if neu.AccommodationPreferences.Rating != nil {
		base.AccommodationPreferences.Rating = neu.AccommodationPreferences.Rating
	}
	return base
}

func unionDates(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, list := range [][]string{a, b} {
		for _, d := range list {
			d = strings.TrimSpace(d)
			if d == "" || seen[d] {
				continue
			}
			seen[d] = true
			out = append(out, d)
		}
	}
	return out
}

func applySharedOrigin(people []models.Participant) []models.Participant {
	var city, airport, origin string
	for _, p := range people {
		if p.OriginCity != "" {
			city = p.OriginCity
		}
		if p.OriginAirport != "" {
			airport = p.OriginAirport
		}
		if p.Origin != "" {
			origin = p.Origin
		}
	}
	if origin == "" {
		origin = city
	}
	if origin == "" {
		origin = airport
	}
	for i := range people {
		if people[i].OriginCity == "" {
			people[i].OriginCity = city
		}
		if people[i].OriginAirport == "" {
			people[i].OriginAirport = airport
		}
		if people[i].Origin == "" {
			people[i].Origin = origin
		}
	}
	return people
}

func hasSharedOrigin(people []models.Participant) bool {
	for _, p := range people {
		if p.OriginAirport != "" || p.OriginCity != "" || p.Origin != "" {
			return true
		}
	}
	return false
}

func hasAnyDates(people []models.Participant) bool {
	for _, p := range people {
		if len(p.GeneralPreferences.Availability) > 0 {
			return true
		}
	}
	return false
}

func formatKnownPrefs(people []models.Participant) string {
	if len(people) == 0 {
		return ""
	}
	b, err := json.MarshalIndent(people, "", "  ")
	if err != nil {
		return ""
	}
	return string(b)
}
