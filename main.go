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
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"fare-brain/config"
	"fare-brain/llm"
	"fare-brain/messaging"
	"fare-brain/models"
	"fare-brain/orchestrator"
	"fare-brain/store"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
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

	if _, err := st.SaveWhatsAppSession(ctx, "robot", map[string]any{
		"status":    "brain_up",
		"messaging": cfg.MessagingBackend,
	}); err != nil {
		slog.Error("whatsapp session upsert failed", "err", err)
		os.Exit(1)
	}

	var llmClient llm.LLM
	switch {
	case cfg.MockLLM:
		llmClient = &llm.MockLLM{}
	case cfg.GeminiAPIKey == "":
		slog.Error("MOCK_LLM=false but GEMINI_API_KEY is empty — add a key from Google AI Studio")
		os.Exit(1)
	default:
		llmClient = llm.NewGeminiLLM(cfg.GeminiAPIKey, cfg.GeminiModel, cfg.LLMCacheDir)
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
	} else {
		slog.Warn("MONGODB_URI is empty — trip memory is in-process only and will be forgotten on restart")
	}
	llmKind := cfg.GeminiModel
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
	mux.HandleFunc("GET /groups/{gid}/events", groupEventsHandler(brain))
	mux.HandleFunc("GET /groups/{gid}/sessions", listGroupSessionsHandler(brain))
	mux.HandleFunc("GET /groups/{gid}/sessions/{sid}", getGroupSessionHandler(brain))
	mux.HandleFunc("GET /dashboard/trips", listDashboardTripsHandler(brain))
	mux.HandleFunc("GET /dashboard/trips/{gid}", getDashboardTripHandler(brain))
	mux.HandleFunc("POST /dashboard/trips/{gid}", postDashboardActHandler(brain))
	mux.HandleFunc("PUT /sessions/whatsapp", putWhatsAppSessionHandler(st))
	mux.HandleFunc("GET /sessions/whatsapp", getWhatsAppSessionHandler(st))

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

func putWhatsAppSessionHandler(st store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		sess, err := st.SaveWhatsAppSession(r.Context(), "robot", body)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, sess)
	}
}

func getWhatsAppSessionHandler(st store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, err := st.GetWhatsAppSession(r.Context(), "robot")
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if sess == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "no session"})
			return
		}
		writeJSON(w, http.StatusOK, sess)
	}
}

// webhookHandler responds instantly; the work (Gemini, searches, Skyvern) happens in a
// background goroutine, and replies are delivered through Messenger.Send. Keeps the
// robot/Telegram from timing out.
func webhookHandler(brain *orchestrator.Brain) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		var peek struct {
			Event string `json:"event"`
		}
		_ = json.Unmarshal(raw, &peek)
		if peek.Event == "poll_vote" {
			var vote models.PollVote
			if err := json.Unmarshal(raw, &vote); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			go brain.HandlePollVote(context.Background(), vote)
			writeJSON(w, http.StatusOK, map[string]any{"reply": nil, "accepted": true})
			return
		}
		var m models.IncomingMessage
		if err := json.Unmarshal(raw, &m); err != nil {
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

func writeDash(w http.ResponseWriter, err error, ok any) {
	var dash *orchestrator.DashboardError
	if errors.As(err, &dash) {
		writeJSON(w, dash.Status, map[string]string{"error": dash.Msg})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, ok)
}

func groupEventsHandler(brain *orchestrator.Brain) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		brain.ServeGroupEvents(w, r, r.PathValue("gid"))
	}
}

func listGroupSessionsHandler(brain *orchestrator.Brain) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := brain.ListGroupSnapshots(r.Context(), r.PathValue("gid"))
		writeDash(w, err, rows)
	}
}

func getGroupSessionHandler(brain *orchestrator.Brain) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		view, err := brain.GetSessionSnapshot(r.Context(), r.PathValue("gid"), r.PathValue("sid"))
		writeDash(w, err, view)
	}
}

func listDashboardTripsHandler(brain *orchestrator.Brain) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := brain.ListDashboardTrips(r.Context())
		writeDash(w, err, rows)
	}
}

func getDashboardTripHandler(brain *orchestrator.Brain) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		view, err := brain.DashboardView(r.Context(), r.PathValue("gid"))
		writeDash(w, err, view)
	}
}

func postDashboardActHandler(brain *orchestrator.Brain) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		view, err := brain.DashboardAct(r.Context(), r.PathValue("gid"), body)
		writeDash(w, err, view)
	}
}

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
