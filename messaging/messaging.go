// Package messaging is the only way the brain reaches a chat. P2 owns the channels; switching
// channels is one env var (MESSAGING_BACKEND), no orchestrator code changes.
//
//	console  — prints and keeps an outbox slice (dev + tests)
//	robot    — POSTs to the whatsapp-web.js robot's /send mailbox
//	telegram — calls the Bot API directly; buttons become an inline ✅/❌ keyboard
package messaging

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// Button is (label, payload). The payload comes back to /telegram/webhook as if the user typed it.
type Button struct {
	Label   string
	Payload string
}

type Messenger interface {
	Send(ctx context.Context, groupID, text string, buttons []Button) error
}

// ---------- console ----------

type OutboxEntry struct {
	GroupID string
	Text    string
	Buttons []Button
}

type ConsoleMessenger struct {
	Outbox []OutboxEntry
}

func (m *ConsoleMessenger) Send(ctx context.Context, groupID, text string, buttons []Button) error {
	m.Outbox = append(m.Outbox, OutboxEntry{GroupID: groupID, Text: text, Buttons: buttons})
	suffix := ""
	if len(buttons) > 0 {
		labels := make([]string, len(buttons))
		for i, b := range buttons {
			labels[i] = b.Label
		}
		suffix = fmt.Sprintf("  [buttons: %s]", joinStrings(labels, " | "))
	}
	fmt.Printf("\n🤖 → %s:\n%s%s\n", groupID, text, suffix)
	return nil
}

func joinStrings(ss []string, sep string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += sep
		}
		out += s
	}
	return out
}

// ---------- robot (WhatsApp) ----------

// RobotMessenger talks to the whatsapp-web.js robot. WhatsApp has no inline buttons, so they're
// rendered as a text hint.
type RobotMessenger struct {
	RobotURL   string
	Token      string
	HTTPClient *http.Client
}

func NewRobotMessenger(robotURL, token string) *RobotMessenger {
	return &RobotMessenger{RobotURL: robotURL, Token: token, HTTPClient: &http.Client{Timeout: 15 * time.Second}}
}

func (m *RobotMessenger) Send(ctx context.Context, groupID, text string, buttons []Button) error {
	// WhatsApp has no inline buttons. Do not append "1 reply 1" hints — they clutter the chat.
	body, err := json.Marshal(map[string]string{
		"group_id": groupID,
		"chat_id":  groupID,
		"text":     text,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.RobotURL+"/send", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	if m.Token != "" {
		req.Header.Set("Authorization", "Bearer "+m.Token)
	}
	resp, err := m.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("robot /send: status %d", resp.StatusCode)
	}
	return nil
}

// ---------- telegram ----------

type TelegramMessenger struct {
	BotToken   string
	HTTPClient *http.Client
}

func NewTelegramMessenger(botToken string) *TelegramMessenger {
	return &TelegramMessenger{BotToken: botToken, HTTPClient: &http.Client{Timeout: 15 * time.Second}}
}

func (m *TelegramMessenger) Send(ctx context.Context, groupID, text string, buttons []Button) error {
	body := map[string]any{"chat_id": groupID, "text": text}
	if len(buttons) > 0 {
		row := make([]map[string]string, len(buttons))
		for i, b := range buttons {
			row[i] = map[string]string{"text": b.Label, "callback_data": b.Payload}
		}
		body["reply_markup"] = map[string]any{"inline_keyboard": [][]map[string]string{row}}
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", m.BotToken)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	resp, err := m.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		slog.Error("telegram sendMessage failed", "status", resp.StatusCode)
		return fmt.Errorf("telegram sendMessage: status %d", resp.StatusCode)
	}
	return nil
}

// ---------- factory ----------

func MakeMessenger(backend, robotURL, robotToken, telegramBotToken string) (Messenger, error) {
	switch backend {
	case "console":
		return &ConsoleMessenger{}, nil
	case "robot":
		return NewRobotMessenger(robotURL, robotToken), nil
	case "telegram":
		return NewTelegramMessenger(telegramBotToken), nil
	default:
		return nil, fmt.Errorf("unknown messaging backend: %s", backend)
	}
}
