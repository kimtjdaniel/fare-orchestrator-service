package tools

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
)

type pendingSearch struct {
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

func registerSearchCallback(agent, sessionID string) (string, <-chan map[string]any) {
	searchCallbacks.Lock()
	defer searchCallbacks.Unlock()
	for id, pending := range searchCallbacks.pending {
		if time.Now().After(pending.expires) {
			delete(searchCallbacks.pending, id)
		}
	}
	id := uuid.NewString()
	pending := &pendingSearch{agent: agent, sessionID: sessionID, results: make(chan map[string]any, 1), expires: time.Now().Add(15 * time.Minute)}
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
	searchCallbacks.Lock()
	defer searchCallbacks.Unlock()
	pending := searchCallbacks.pending[r.PathValue("requestID")]
	if pending == nil || time.Now().After(pending.expires) {
		http.Error(w, "Search callback expired or unknown", http.StatusNotFound)
		return
	}
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
	if pending.searchID == "" {
		pending.searchID = id
		pending.results <- result
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"accepted": true, "search_id": id})
}
