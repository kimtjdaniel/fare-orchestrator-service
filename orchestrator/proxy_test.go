package orchestrator

import (
	"strings"
	"testing"

	"fare-brain/models"
)

func TestLooksLikePrefUpdate(t *testing.T) {
	people := []models.Participant{{WhatsAppName: "Tom Chen"}}
	roster := []models.GroupMember{{Name: "Tom Chen"}, {Name: "Priya"}}

	cases := []struct {
		text string
		want bool
	}{
		{"I know toms schedule, he's not available that date", true},
		{"he's flying from YVR", true},
		{"Tom can't do those dates", true},
		{"Priya is flying from YYZ", true},
		{"lets get pizza later", false},
		{"can't make dinner tonight", false},
		{"2", false},
	}
	for _, c := range cases {
		got := looksLikePrefUpdate(c.text, people, roster)
		if got != c.want {
			t.Errorf("%q: got %v want %v", c.text, got, c.want)
		}
	}
}

func TestScrubWhatsAppIDs(t *testing.T) {
	got := scrubWhatsAppIDs("Hey @Priya and @14165551234@c.us, dates?")
	if strings.Contains(got, "@") || strings.Contains(got, "c.us") || strings.Contains(strings.ToLower(got), "priya") {
		t.Fatalf("still had a tag: %q", got)
	}
}
