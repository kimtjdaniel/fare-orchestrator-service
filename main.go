// Command fare-brain is the FastAPI-equivalent HTTP entry point.   Run:  go run .
//
// Endpoints
//
//	POST /webhook             WhatsApp robot contract (see CONTRACTS.md). Replies go out via Messenger.Send.
//	POST /telegram/webhook    Raw Telegram updates, converted to the same IncomingMessage.
//	GET  /trips/{id}          Full trip JSON for the dashboard.
//	GET  /groups/{gid}/trip   Active trip for a group (dashboard / debugging).
//	GET  /health
package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"yate-brain/config"
	"yate-brain/llm"
	"yate-brain/messaging"
	"yate-brain/models"
	"yate-brain/orchestrator"
	"yate-brain/store"
)

func main() {
	cfg := config.Load()
	ctx := context.Background()

	var st store.Store
	if cfg.MongoURI != "" {
		st = store.NewMongoStore(cfg.MongoURI, cfg.MongoDB)
	} else {
		st = store.NewMemoryStore()
	}
	if err := st.Connect(ctx); err != nil {
		slog.Error("store connect failed", "err", err)
		os.Exit(1)
	}
	defer st.Close(ctx)

	var llmClient llm.LLM
	switch {
	case cfg.MockLLM:
		llmClient = &llm.MockLLM{}
	case cfg.GeminiAPIKey != "":
		llmClient = llm.NewGeminiLLM(cfg.GeminiAPIKey, cfg.GeminiModel, cfg.LLMCacheDir)
	default:
		llmClient = llm.NewClaudeLLM(cfg.AnthropicAPIKey, cfg.AnthropicModel, cfg.LLMCacheDir)
	}

	messenger, err := messaging.MakeMessenger(cfg.MessagingBackend, cfg.RobotURL, cfg.RobotToken, cfg.TelegramBotToken)
	if err != nil {
		slog.Error("messaging setup failed", "err", err)
		os.Exit(1)
	}

	brain := orchestrator.NewBrain(cfg, st, llmClient, messenger)

	storeKind := "memory"
	if cfg.MongoURI != "" {
		storeKind = "mongo"
	}
	llmKind := cfg.AnthropicModel
	switch {
	case cfg.MockLLM:
		llmKind = "mock"
	case cfg.GeminiAPIKey != "":
		llmKind = cfg.GeminiModel
	}
	travelKind := "real"
	if cfg.MockTravel {
		travelKind = "mock"
	}
	browserKind := "skyvern"
	if cfg.MockBrowser {
		browserKind = "mock"
	}
	slog.Info("brain up", "store", storeKind, "llm", llmKind, "travel", travelKind,
		"browser", browserKind, "messaging", cfg.MessagingBackend, "robot_auth", cfg.RobotToken != "")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", healthHandler(cfg))
	mux.HandleFunc("POST /webhook", webhookHandler(brain))
	mux.HandleFunc("POST /telegram/webhook", telegramWebhookHandler(cfg, brain))
	mux.HandleFunc("GET /trips/{id}", getTripHandler(st))
	mux.HandleFunc("GET /groups/{gid}/trip", getGroupTripHandler(st))

	addr := ":" + getenv("PORT", "8000")
	slog.Info("listening", "addr", addr)
	if err := http.ListenAndServe(addr, withCORS(mux)); err != nil {
		slog.Error("server stopped", "err", err)
		os.Exit(1)
	}
}

func getenv(name, def string) string {
	if v, ok := os.LookupEnv(name); ok {
		return v
	}
	return def
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "*")
		w.Header().Set("Access-Control-Allow-Headers", "*")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func healthHandler(cfg *config.Settings) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		storeKind := "memory"
		if cfg.MongoURI != "" {
			storeKind = "mongo"
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "mock_llm": cfg.MockLLM, "mock_travel": cfg.MockTravel,
			"mock_browser": cfg.MockBrowser, "messaging": cfg.MessagingBackend, "store": storeKind,
		})
	}
}

// webhookHandler responds instantly; the work (Claude, searches, Skyvern) happens in a
// background goroutine, and replies are delivered through Messenger.Send. Keeps the
// robot/Telegram from timing out.
func webhookHandler(brain *orchestrator.Brain) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var m models.IncomingMessage
		if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		go brain.Handle(context.Background(), m)
		writeJSON(w, http.StatusOK, map[string]any{"reply": nil, "accepted": true})
	}
}

// ------------------------------------------------------------------ Telegram adapter (P2 to verify)

func telegramToIncoming(cfg *config.Settings, update map[string]any) *models.IncomingMessage {
	username := strings.ToLower(strings.TrimPrefix(cfg.TelegramBotUsername, "@"))

	if cq, ok := update["callback_query"].(map[string]any); ok {
		msg, _ := cq["message"].(map[string]any)
		chat, _ := msg["chat"].(map[string]any)
		from, _ := cq["from"].(map[string]any)
		senderName := stringField(from, "first_name")
		if senderName == "" {
			senderName = stringField(from, "username")
		}
		if senderName == "" {
			senderName = "?"
		}
		return &models.IncomingMessage{
			GroupID: stringifyID(chat["id"]), GroupName: stringField(chat, "title"),
			SenderID: stringifyID(from["id"]), SenderName: senderName,
			Text: stringField(cq, "data"), Tagged: true,
			MessageID: "cb_" + stringifyID(cq["id"]),
		}
	}

	msg, ok := update["message"].(map[string]any)
	if !ok {
		return nil
	}
	text, hasText := msg["text"].(string)
	chat, _ := msg["chat"].(map[string]any)
	chatType := stringField(chat, "type")
	if !hasText || (chatType != "group" && chatType != "supergroup") {
		return nil
	}
	from, _ := msg["from"].(map[string]any)

	var mentions []string
	if entities, ok := msg["entities"].([]any); ok {
		for _, eRaw := range entities {
			e, ok := eRaw.(map[string]any)
			if !ok || stringField(e, "type") != "mention" {
				continue
			}
			offset := intField(e, "offset")
			length := intField(e, "length")
			if offset >= 0 && length > 0 && offset+length <= len(text) {
				mention := strings.ToLower(strings.TrimPrefix(text[offset:offset+length], "@"))
				mentions = append(mentions, mention)
			}
		}
	}
	replyingToBot := false
	if username != "" {
		if replyTo, ok := msg["reply_to_message"].(map[string]any); ok {
			if replyFrom, ok := replyTo["from"].(map[string]any); ok {
				replyingToBot = strings.ToLower(stringField(replyFrom, "username")) == username
			}
		}
	}
	tagged := replyingToBot
	for _, mention := range mentions {
		if mention == username {
			tagged = true
		}
	}

	senderName := stringField(from, "first_name")
	if senderName == "" {
		senderName = stringField(from, "username")
	}
	if senderName == "" {
		senderName = "?"
	}
	return &models.IncomingMessage{
		GroupID: stringifyID(chat["id"]), GroupName: stringField(chat, "title"),
		SenderID: stringifyID(from["id"]), SenderName: senderName,
		Text: text, Tagged: tagged,
		Timestamp: int64(intField(msg, "date")), MessageID: stringifyID(msg["message_id"]),
	}
}

func stringField(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

func intField(m map[string]any, key string) int {
	if m == nil {
		return 0
	}
	if f, ok := m[key].(float64); ok {
		return int(f)
	}
	return 0
}

func stringifyID(v any) string {
	switch n := v.(type) {
	case string:
		return n
	case float64:
		return strconv.FormatInt(int64(n), 10)
	default:
		return ""
	}
}

func ackCallback(cfg *config.Settings, callbackID string) {
	url := "https://api.telegram.org/bot" + cfg.TelegramBotToken + "/answerCallbackQuery"
	body, _ := json.Marshal(map[string]string{"callback_query_id": callbackID})
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		return
	}
	req.Header.Set("content-type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
}

func telegramWebhookHandler(cfg *config.Settings, brain *orchestrator.Brain) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if cfg.TelegramWebhookSecret != "" && r.Header.Get("X-Telegram-Bot-Api-Secret-Token") != cfg.TelegramWebhookSecret {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var update map[string]any
		if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if m := telegramToIncoming(cfg, update); m != nil {
			go brain.Handle(context.Background(), *m)
		}
		if cq, ok := update["callback_query"].(map[string]any); ok && cfg.TelegramBotToken != "" {
			go ackCallback(cfg, stringifyID(cq["id"]))
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

// ------------------------------------------------------------------ dashboard reads

func getTripHandler(st store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		view, err := store.TripView(r.Context(), st, r.PathValue("id"))
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if view == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, view)
	}
}

func getGroupTripHandler(st store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		trip, err := st.GetTrip(r.Context(), r.PathValue("gid"))
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if trip == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"detail": "no active trip"})
			return
		}
		view, err := store.TripView(r.Context(), st, trip.ID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if view == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, view)
	}
}
