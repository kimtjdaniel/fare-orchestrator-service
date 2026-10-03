package orchestrator

import (
	"strings"
	"testing"
	"time"

	"fare-brain/models"
)

func TestLooksLikeDashboardAsk(t *testing.T) {
	if !looksLikeDashboardAsk("send the link again") {
		t.Fatal("expected dashboard ask")
	}
	if looksLikeDashboardAsk("what should we do in lisbon") {
		t.Fatal("did not expect dashboard ask")
	}
}

func TestLooksLikeStuck(t *testing.T) {
	if !looksLikeStuck("we can't decide on the budget") {
		t.Fatal("expected stuck")
	}
	if looksLikeStuck("lisbon sounds good") {
		t.Fatal("did not expect stuck")
	}
}

func TestMatchOptionFromLabel(t *testing.T) {
	opts := []models.Option{
		{Position: 1, Destination: "Lisbon"},
		{Position: 2, Destination: "Mexico City"},
	}
	got := matchOptionFromLabel("Lisbon · Jan 4–9", opts)
	if got == nil || got.Position != 1 {
		t.Fatalf("got %+v", got)
	}
}

func TestHarvestParenAirport(t *testing.T) {
	h := harvestText("Vancouver (YVR)", time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC))
	if h.Airport != "YVR" {
		t.Fatalf("airport %q", h.Airport)
	}
}

func TestLooksLikeHotelAsk(t *testing.T) {
	if !looksLikeHotelAsk("show us the hotel") {
		t.Fatal("expected hotel ask")
	}
	if looksLikeHotelAsk("what should we eat") {
		t.Fatal("did not expect hotel ask")
	}
}

func TestReplanItalyApril(t *testing.T) {
	today := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
	if parseDestination("plan italy southern next April instead") != "Southern Italy" {
		t.Fatal(parseDestination("plan italy southern next April instead"))
	}
	h := harvestText("Help us plan southern italy now", today)
	if h.Destination != "Southern Italy" {
		t.Fatalf("dest %q", h.Destination)
	}
	h = harvestText("next April instead", today)
	if len(h.Dates) == 0 || !strings.HasPrefix(h.Dates[0], "2027-04") {
		t.Fatalf("april dates %v", h.Dates)
	}
	if !looksLikeCancelBooking("Yea cancel all tel aviv") {
		t.Fatal("expected cancel")
	}
	if !looksLikeReplan("change if plan, plan italy southern next April instead") {
		t.Fatal("expected replan")
	}
}
