package orchestrator

import (
	"testing"

	"fare-brain/models"
)

func TestMergeParticipantsKeepsStoredOrigin(t *testing.T) {
	existing := []models.Participant{{
		WhatsAppName:  "Tom Chen",
		OriginAirport: "YVR",
		Origin:        "YVR",
		GeneralPreferences: models.GeneralPreferences{
			Availability: []string{"2026-11-20"},
		},
	}}
	extracted := []models.Participant{{
		WhatsAppName: "Tom",
		GeneralPreferences: models.GeneralPreferences{
			Availability: []string{"2026-11-21"},
		},
	}}
	got := mergeParticipants(existing, extracted)
	if len(got) != 1 {
		t.Fatalf("len=%d", len(got))
	}
	if got[0].OriginAirport != "YVR" {
		t.Fatalf("origin=%q", got[0].OriginAirport)
	}
	if len(got[0].GeneralPreferences.Availability) != 2 {
		t.Fatalf("dates=%v", got[0].GeneralPreferences.Availability)
	}
}

func TestHasSharedOrigin(t *testing.T) {
	if hasSharedOrigin(nil) {
		t.Fatal("empty")
	}
	if !hasSharedOrigin([]models.Participant{{OriginAirport: "YYZ"}}) {
		t.Fatal("expected origin")
	}
}
