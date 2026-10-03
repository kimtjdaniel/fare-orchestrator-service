package orchestrator

import (
	"testing"
	"time"

	"fare-brain/models"
)

func TestHarvestTextDatesAndOrigin(t *testing.T) {
	today := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
	h := harvestText("we're all free Nov 20-23 and flying from YVR", today)
	if h.Airport != "YVR" {
		t.Fatalf("airport=%q", h.Airport)
	}
	want := map[string]bool{"2026-11-20": true, "2026-11-23": true}
	for _, d := range h.Dates {
		delete(want, d)
	}
	if len(want) != 0 {
		t.Fatalf("dates=%v leftover=%v", h.Dates, want)
	}
}

func TestApplyHarvestFillsEmptyPeople(t *testing.T) {
	h := harvestedFacts{Dates: []string{"2026-11-20"}, Airport: "YVR"}
	roster := []models.GroupMember{{Name: "Tom Chen"}, {Name: "Priya", IsAgent: false}}
	got := applyHarvest(nil, h, roster)
	if !enoughToPlan(got) {
		t.Fatalf("not enough: %+v", got)
	}
}
