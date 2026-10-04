package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/google/uuid"
)

type pendingSearch struct {
	mu        sync.Mutex
	forward   func(map[string]any)
	closed    bool
	delivered bool
	agent     string
	sessionID string
	results   chan map[string]any
	expires   time.Time
	searchID  string
}

// Retain correlation IDs briefly so duplicate deliveries can be acknowledged.
// Active searches and callback delivery must reach the same orchestrator process.
var searchCallbacks = struct {
	sync.Mutex
	pending map[string]*pendingSearch
}{pending: map[string]*pendingSearch{}}

func registerSearchCallback(ctx context.Context, agent, sessionID string) (string, <-chan map[string]any) {
	searchCallbacks.Lock()
	defer searchCallbacks.Unlock()
	for id, pending := range searchCallbacks.pending {
		if time.Now().After(pending.expires) {
			delete(searchCallbacks.pending, id)
		}
	}
	id := uuid.NewString()
	pending := &pendingSearch{agent: agent, sessionID: sessionID, results: make(chan map[string]any, 1), expires: time.Now().Add(15 * time.Minute), forward: func(event map[string]any) { forwardSearch(ctx, agent, event) }}
	searchCallbacks.pending[id] = pending
	return id, pending.results
}

// SearchResultsHandler receives the full saved Lambda result after recording finalization.
func SearchResultsHandler(w http.ResponseWriter, r *http.Request) {
	var result map[string]any
	r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&result); err != nil {
		http.Error(w, "Expected a JSON result", http.StatusBadRequest)
		return
	}
	pending := lookupSearchCallback(r.PathValue("requestID"))
	if pending == nil || time.Now().After(pending.expires) {
		http.Error(w, "Search callback expired or unknown", http.StatusNotFound)
		return
	}
	pending.mu.Lock()
	defer pending.mu.Unlock()
	id := searchRecordText(result["search_id"])
	status := searchRecordText(result["status"])
	rows := "flights"
	if pending.agent == "hotel" {
		rows = "hotels"
	}
	_, hasRows := result[rows].([]any)
	if id == "" || searchRecordText(result["session_id"]) != pending.sessionID || !hasRows || (status != "complete" && status != "partially_complete" && status != "failed") {
		http.Error(w, "Result does not match the pending search", http.StatusBadRequest)
		return
	}
	if pending.searchID != "" && pending.searchID != id {
		http.Error(w, "A different result was already delivered", http.StatusConflict)
		return
	}
	if !pending.delivered {
		pending.searchID = id
		pending.delivered = true
		pending.closed = true
		pending.forward = nil
		pending.results <- result
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"accepted": true, "search_id": id})
}

func lookupSearchCallback(id string) *pendingSearch {
	searchCallbacks.Lock()
	defer searchCallbacks.Unlock()
	return searchCallbacks.pending[id]
}

// Leave final delivery correlation in place for retries, but release the live
// listener as soon as this search returns, fails, or reaches its timeout.
func stopSearchUpdates(id string) {
	if pending := lookupSearchCallback(id); pending != nil {
		pending.mu.Lock()
		defer pending.mu.Unlock()
		pending.closed = true
		pending.forward = nil
	}
}

// SearchEventsHandler accepts browser progress only for an existing opaque search
// callback. Lambda does not receive a general-purpose dashboard publishing URL.
func SearchEventsHandler(w http.ResponseWriter, r *http.Request) {
	pending := lookupSearchCallback(r.PathValue("requestID"))
	if pending == nil || time.Now().After(pending.expires) {
		http.Error(w, "Search callback expired or unknown", http.StatusNotFound)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	var event map[string]any
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&event); err != nil || !validSearchEvent(event) {
		http.Error(w, "Invalid search event", http.StatusBadRequest)
		return
	}
	pending.mu.Lock()
	defer pending.mu.Unlock()
	id := searchRecordText(event["search_id"])
	if searchRecordText(event["session_id"]) != pending.sessionID || (pending.searchID != "" && pending.searchID != id) {
		http.Error(w, "Event does not match the pending search", http.StatusConflict)
		return
	}
	if pending.closed {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	pending.searchID = id
	// Discard unknown fields before the wire event reaches dashboard subscribers.
	clean := map[string]any{}
	for _, key := range []string{"version", "type", "session_id", "search_id", "timestamp", "website", "origin", "browser_session_id", "status", "url", "mime_type", "data", "error", "warning", "results_complete"} {
		if value, ok := event[key]; ok {
			clean[key] = value
		}
	}
	if pending.forward != nil {
		pending.forward(clean)
	}
	w.WriteHeader(http.StatusNoContent)
}

func validSearchEvent(event map[string]any) bool {
	if event["version"] != float64(1) || searchRecordText(event["session_id"]) == "" || searchRecordText(event["search_id"]) == "" {
		return false
	}
	for _, key := range []string{"session_id", "search_id", "origin", "website", "browser_session_id", "timestamp", "status", "warning"} {
		if value, exists := event[key]; exists && value != nil {
			text, ok := value.(string)
			if !ok || len(text) > 1024 {
				return false
			}
		}
	}
	if value, exists := event["results_complete"]; exists && value != nil {
		if _, ok := value.(bool); !ok {
			return false
		}
	}
	if value := event["error"]; value != nil {
		err, ok := value.(map[string]any)
		if !ok || len(err) > 2 {
			return false
		}
		for key, value := range err {
			text, ok := value.(string)
			if (key != "code" && key != "message") || !ok || len(text) > 1024 {
				return false
			}
		}
	}
	kind := searchRecordText(event["type"])
	if kind == "search.status" {
		return searchRecordText(event["status"]) != ""
	}
	if searchRecordText(event["browser_session_id"]) == "" || (searchRecordText(event["website"]) == "" && searchRecordText(event["origin"]) == "") {
		return false
	}
	switch kind {
	case "browser.stream":
		switch event["status"] {
		case "starting", "live", "ended", "unavailable":
			return true
		}
	case "browser.live_view":
		if event["url"] == nil {
			return true
		}
		text := searchRecordText(event["url"])
		parsed, err := url.Parse(text)
		return len(text) <= 2048 && err == nil && parsed.Host != "" && (parsed.Scheme == "https" || parsed.Scheme == "http")
	case "browser.frame":
		data := searchRecordText(event["data"])
		if event["mime_type"] != "image/jpeg" || len(data) == 0 || len(data) > 1<<20 {
			return false
		}
		jpeg, err := base64.StdEncoding.DecodeString(data)
		return err == nil && len(jpeg) >= 3 && jpeg[0] == 0xff && jpeg[1] == 0xd8 && jpeg[2] == 0xff
	}
	return false
}
