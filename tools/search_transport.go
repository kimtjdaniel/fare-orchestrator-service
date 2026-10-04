package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fare-brain/config"
	"fmt"
	"github.com/gorilla/websocket"
	"io"
	"net/http"
	"time"
)

type searchEvents struct {
	sessionID string
	emit      func(string, map[string]any)
}
type searchEventsKey struct{}

func WithSearchEvents(ctx context.Context, sessionID string, emit func(string, map[string]any)) context.Context {
	return context.WithValue(ctx, searchEventsKey{}, searchEvents{sessionID, emit})
}
func searchSession(ctx context.Context) string {
	if events, ok := ctx.Value(searchEventsKey{}).(searchEvents); ok {
		return events.sessionID
	}
	return "fare-search"
}
func forwardSearch(ctx context.Context, agent string, event map[string]any) {
	events, ok := ctx.Value(searchEventsKey{}).(searchEvents)
	if !ok || events.emit == nil {
		return
	}
	kind, _ := event["type"].(string)
	switch kind {
	case "browser.frame", "browser.stream", "browser.live_view":
		events.emit("agent."+kind, map[string]any{"agentType": agent, "event": event})
	case "search.status":
		status, _ := event["status"].(string)
		message := fmt.Sprintf("%s agent: %s", agent, status)
		if status == "complete" || status == "partially_complete" {
			message = "Search finished; waiting for saved results"
		}
		events.emit(agent+"_search.progress", map[string]any{"message": message})
	}
}
func runSearchService(ctx context.Context, cfg *config.Settings, agent string, request map[string]any) (result map[string]any, resultErr error) {
	defer func() {
		if result != nil {
			if events, ok := ctx.Value(searchEventsKey{}).(searchEvents); ok && events.emit != nil {
				events.emit(agent+"_search.recording.completed", searchRecordingMetadata(agent, result))
			}
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, 12*time.Minute)
	defer cancel()
	endpoint, socketURL := cfg.FlightServiceURL, cfg.FlightServiceWSURL
	if agent == "hotel" {
		endpoint, socketURL = cfg.HotelServiceURL, cfg.HotelServiceWSURL
	}
	if socketURL != "" {
		return runSearchSocket(ctx, cfg, agent, socketURL, request)
	}
	var callback <-chan map[string]any
	if cfg.OrchestratorPublicURL != "" {
		id, results := registerSearchCallback(agent, searchRecordText(request["session_id"]))
		callback = results
		request["callback_url"] = cfg.OrchestratorPublicURL + "/travel-search/results/" + id
	}
	type response struct {
		result map[string]any
		err    error
	}
	done := make(chan response, 1)
	go func() {
		result, err := runSearchHTTP(ctx, agent, endpoint, request)
		done <- response{result, err}
	}()
	select {
	case result := <-callback:
		return validateSearchResult(agent, result)
	case response := <-done:
		// A callback can arrive just before the HTTP response (or its connection error).
		select {
		case result := <-callback:
			return validateSearchResult(agent, result)
		default:
		}
		if response.err == nil || response.result != nil || callback == nil {
			return response.result, response.err
		}
		// A dropped synchronous connection does not imply Lambda stopped running.
		select {
		case result := <-callback:
			return validateSearchResult(agent, result)
		case <-ctx.Done():
			return nil, fmt.Errorf("%s search callback did not arrive: %w", agent, response.err)
		}
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func runSearchHTTP(ctx context.Context, agent, endpoint string, request map[string]any) (map[string]any, error) {
	raw, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Timeout: 12 * time.Minute}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s search request failed: %w", agent, err)
	}
	defer response.Body.Close()
	var result map[string]any
	if err = json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(&result); err != nil {
		return nil, fmt.Errorf("%s service returned invalid JSON", agent)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return result, serviceError(agent, result)
	}
	return validateSearchResult(agent, result)
}

func runSearchSocket(ctx context.Context, cfg *config.Settings, agent, url string, request map[string]any) (map[string]any, error) {
	headers := http.Header{}
	headers.Set("Origin", cfg.SearchFrontendOrigin)
	dialer := *websocket.DefaultDialer
	dialer.HandshakeTimeout = 15 * time.Second
	conn, _, err := dialer.DialContext(ctx, url, headers)
	if err != nil {
		return nil, fmt.Errorf("%s search stream connection failed: %w", agent, err)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	conn.SetReadLimit(8 << 20)
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetReadDeadline(deadline)
	}
	_ = conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
	if err = conn.WriteJSON(map[string]any{"action": "search", "request": request}); err != nil {
		return nil, err
	}
	searchID := ""
	for {
		var event map[string]any
		if err = conn.ReadJSON(&event); err != nil {
			return nil, fmt.Errorf("%s stream ended before saved results arrived: %w", agent, err)
		}
		if version, ok := event["version"].(float64); !ok || version != 1 {
			continue
		}
		if sid, ok := event["session_id"].(string); ok && sid != "" && sid != request["session_id"] {
			continue
		}
		if id, ok := event["search_id"].(string); ok && id != "" {
			if searchID != "" && id != searchID {
				continue
			}
			searchID = id
		}
		kind, _ := event["type"].(string)
		if kind == "search.error" {
			return nil, serviceError(agent, event)
		}
		if kind == "search.result" {
			result, ok := event["result"].(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%s service returned an invalid result", agent)
			}
			return validateSearchResult(agent, result)
		}
		forwardSearch(ctx, agent, event)
	}
}
func serviceError(agent string, result map[string]any) error {
	if value, ok := result["error"].(map[string]any); ok {
		if message, ok := value["message"].(string); ok && message != "" {
			return fmt.Errorf("%s search: %s", agent, message)
		}
	}
	return fmt.Errorf("%s search failed", agent)
}
func validateSearchResult(agent string, result map[string]any) (map[string]any, error) {
	status, _ := result["status"].(string)
	if status != "complete" && status != "partially_complete" {
		return result, serviceError(agent, result)
	}
	return result, nil
}
func searchReplayURL(result map[string]any) string {
	if replay := searchRecordText(result["replay_url"]); replay != "" {
		return replay
	}
	if origins, ok := result["origins"].([]any); ok {
		for _, value := range origins {
			if origin, ok := value.(map[string]any); ok {
				if replay := searchRecordText(origin["replay_url"]); replay != "" {
					return replay
				}
			}
		}
	}
	return ""
}

func searchRecordText(value any) string    { s, _ := value.(string); return s }
func searchRecordNumber(value any) float64 { n, _ := value.(float64); return n }

// Keep recording delivery separate from the offers used by Gemini. Each source can
// finish or fail independently; its links and errors survive in dashboard snapshots.
func searchRecordingMetadata(agent string, result map[string]any) map[string]any {
	sources := make([]map[string]any, 0)
	if origins, ok := result["origins"].([]any); ok {
		for _, value := range origins {
			origin, ok := value.(map[string]any)
			if !ok {
				continue
			}
			sources = append(sources, map[string]any{
				"website": origin["website"], "origin": origin["origin"],
				"status": origin["status"], "error": origin["error"],
				"recordingUrl": origin["recording_url"], "replayUrl": origin["replay_url"],
				"recordingError":   origin["recording_error"],
				"browserSessionId": origin["skyvern_browser_session_id"], "recordings": origin["recordings"],
			})
		}
	}
	return map[string]any{
		"agentType": agent, "searchId": result["search_id"], "status": result["status"],
		"recordingUrl": result["recording_url"], "replayUrl": searchReplayURL(result),
		"recordingError": result["recording_error"], "deliveryError": result["delivery_error"],
		"sources": sources,
	}
}

func searchRecordOptionalNumber(value any) *float64 {
	n, ok := value.(float64)
	if !ok {
		return nil
	}
	return &n
}
