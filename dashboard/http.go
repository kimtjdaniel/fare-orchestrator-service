package dashboard

import (
	"context"
	"encoding/json"
	"github.com/gorilla/websocket"
	"net/http"
	"strings"
	"time"
)

func respond(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func (m *Manager) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /groups/{gid}/events", m.socket)
	mux.HandleFunc("GET /groups/{gid}/sessions", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		g, err := m.load(r.Context(), r.PathValue("gid"))
		if err != nil {
			respond(w, 503, map[string]string{"error": "Could not load group sessions"})
			return
		}
		respond(w, 200, g.Sessions)
	})
	mux.HandleFunc("GET /groups/{gid}/sessions/{sid}", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		g, err := m.load(r.Context(), r.PathValue("gid"))
		if err != nil {
			respond(w, 503, map[string]string{"error": "Could not load the session"})
			return
		}
		for _, s := range g.Sessions {
			if s.Session.ID == r.PathValue("sid") {
				respond(w, 200, s)
				return
			}
		}
		respond(w, 404, map[string]string{"error": "Session not found in this group"})
	})
}
func (m *Manager) socket(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("gid"))
	if id == "" {
		respond(w, 400, map[string]string{"error": "Group ID is required"})
		return
	}
	// This hackathon has no authentication; each connection is explicitly scoped to one group.
	upgrade := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	conn, err := upgrade.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	ch := make(chan []byte, 32)
	m.mu.Lock()
	g, err := m.load(r.Context(), id)
	if err != nil {
		m.mu.Unlock()
		_ = conn.WriteJSON(map[string]any{"version": 1, "type": "connection.error", "groupId": id, "message": "Could not load group sessions"})
		return
	}
	raw, err := json.Marshal(map[string]any{"version": 1, "type": "group.snapshot", "groupId": id, "sessions": g.Sessions, "revision": g.Revision})
	if err != nil {
		m.mu.Unlock()
		return
	}
	g.subscribers[ch] = true
	m.mu.Unlock()
	defer func() { m.mu.Lock(); delete(g.subscribers, ch); m.mu.Unlock() }()
	conn.SetReadLimit(1024)
	_ = conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	conn.SetPongHandler(func(string) error { return conn.SetReadDeadline(time.Now().Add(60 * time.Second)) })
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go func() {
		defer cancel()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err = conn.WriteMessage(websocket.TextMessage, raw); err != nil {
		return
	}
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case data, ok := <-ch:
			if !ok {
				return
			}
			_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err = conn.WriteMessage(websocket.TextMessage, data); err != nil {
				return
			}
		case <-ticker.C:
			if err = conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)); err != nil {
				return
			}
		}
	}
}
