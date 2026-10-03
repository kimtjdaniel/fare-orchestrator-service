package models

import (
	"encoding/json"
	"testing"
)

func TestIncomingMessageFlat(t *testing.T) {
	raw := []byte(`{
		"group_id": "1203@g.us",
		"group_name": "Lisbon trip",
		"sender_id": "14165551234@c.us",
		"sender_name": "Priya",
		"text": "@Fare hello",
		"tagged": true,
		"timestamp": 1759400000,
		"message_id": "abc"
	}`)
	var m IncomingMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.GroupID != "1203@g.us" || m.SenderID != "14165551234@c.us" || m.SenderName != "Priya" {
		t.Fatalf("flat payload: %+v", m)
	}
	if !m.Tagged || m.Text != "@Fare hello" {
		t.Fatalf("flat payload fields: %+v", m)
	}
}

func TestIncomingMessageNestedWhatsApp(t *testing.T) {
	raw := []byte(`{
		"event": "message",
		"channel": "whatsapp",
		"message_id": "false_1203@g.us_ABC",
		"timestamp": 1759400000,
		"chat": {"id": "1203@g.us", "name": "Lisbon trip", "is_group": true},
		"sender": {"id": "14165551234@c.us", "name": "Priya", "phone": "14165551234"},
		"text": "@Fare hello",
		"tagged": true
	}`)
	var m IncomingMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.GroupID != "1203@g.us" {
		t.Fatalf("group_id=%q", m.GroupID)
	}
	if m.GroupName != "Lisbon trip" {
		t.Fatalf("group_name=%q", m.GroupName)
	}
	if m.SenderID != "14165551234@c.us" {
		t.Fatalf("sender_id=%q", m.SenderID)
	}
	if m.SenderName != "Priya" {
		t.Fatalf("sender_name=%q", m.SenderName)
	}
	if m.MessageID != "false_1203@g.us_ABC" || !m.Tagged {
		t.Fatalf("nested payload: %+v", m)
	}
}

func TestIncomingMessageParticipants(t *testing.T) {
	raw := []byte(`{
		"group_id": "1203@g.us",
		"group_name": "Lisbon trip",
		"sender_id": "14165551234@c.us",
		"sender_name": "Priya",
		"text": "@Fare hello",
		"tagged": true,
		"participants": [
			{"id": "14165551234@c.us", "name": "Priya", "is_agent": false},
			{"id": "1555@c.us", "name": "Fare", "is_agent": true}
		]
	}`)
	var m IncomingMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Participants) != 2 {
		t.Fatalf("participants=%+v", m.Participants)
	}
	if m.Participants[0].Name != "Priya" || m.Participants[1].IsAgent != true {
		t.Fatalf("participants=%+v", m.Participants)
	}
}

func TestIncomingMessageQuoted(t *testing.T) {
	raw := []byte(`{
		"group_id": "1203@g.us",
		"sender_id": "14165551234@c.us",
		"sender_name": "Priya",
		"text": "send it!",
		"tagged": true,
		"quoted": {"id": "bot_msg", "sender_id": "1555@c.us", "text": "Casa Linda. About C$149 a night, CAD.", "from_me": true}
	}`)
	var m IncomingMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.Quoted == nil || !m.Quoted.FromMe || m.Quoted.Text == "" {
		t.Fatalf("quoted=%+v", m.Quoted)
	}
}
