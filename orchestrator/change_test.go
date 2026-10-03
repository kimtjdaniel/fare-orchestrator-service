package orchestrator

import (
	"testing"

	"fare-brain/config"
	"fare-brain/models"
)

func TestFirstPopulateNotGated(t *testing.T) {
	b := &Brain{Config: &config.Settings{Timezone: "UTC"}}
	trip := &models.Trip{}
	if pc := b.proposedLockedChange(trip, "we are going from dec 20th to 24th"); pc != nil {
		t.Fatalf("first dates should apply freely, got %+v", pc)
	}
}

func TestDateChangeIsGated(t *testing.T) {
	b := &Brain{Config: &config.Settings{Timezone: "UTC"}}
	trip := &models.Trip{Participants: []models.Participant{{
		GeneralPreferences: models.GeneralPreferences{Availability: []string{"2026-12-20", "2026-12-21", "2026-12-22", "2026-12-23", "2026-12-24"}},
	}}}
	pc := b.proposedLockedChange(trip, "update date to dec 20th to dec 21st instead")
	if pc == nil || pc.Kind != "dates" {
		t.Fatalf("expected dates change, got %+v", pc)
	}
}

func TestAllVotedYes(t *testing.T) {
	pc := &models.PendingChange{
		Needed: []string{"a", "b"},
		Votes:  map[string]string{"a": "yes", "b": "yes"},
	}
	if !allVotedYes(pc) {
		t.Fatal("expected all yes")
	}
	pc.Votes["b"] = "no"
	if !anyoneVotedNo(pc) {
		t.Fatal("expected a no")
	}
}

func TestApplyHarvestDoesNotWidenDates(t *testing.T) {
	people := []models.Participant{{
		WhatsAppName: "Tom",
		GeneralPreferences: models.GeneralPreferences{Availability: []string{"2026-12-20"}},
	}}
	got := applyHarvest(people, harvestedFacts{Dates: []string{"2026-12-21"}}, nil)
	if !datesEqual(got[0].GeneralPreferences.Availability, []string{"2026-12-20"}) {
		t.Fatalf("dates mutated: %v", got[0].GeneralPreferences.Availability)
	}
}
