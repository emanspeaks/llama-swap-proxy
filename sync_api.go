package main

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/websocket"
)

type SyncServer struct {
	store                 *SessionStore
	defaultUser           string
	isolateModelUserState bool
	notifier              *SessionNotifier
}

type SessionNotifier struct {
	mu        sync.Mutex
	listeners map[string]map[chan struct{}]struct{}
}

func NewSessionNotifier() *SessionNotifier {
	return &SessionNotifier{listeners: map[string]map[chan struct{}]struct{}{}}
}

func (n *SessionNotifier) Subscribe(key string) chan struct{} {
	ch := make(chan struct{}, 1)
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.listeners[key] == nil {
		n.listeners[key] = map[chan struct{}]struct{}{}
	}
	n.listeners[key][ch] = struct{}{}
	return ch
}

func (n *SessionNotifier) Unsubscribe(key string, ch chan struct{}) {
	n.mu.Lock()
	defer n.mu.Unlock()
	listeners := n.listeners[key]
	if listeners == nil {
		return
	}
	delete(listeners, ch)
	close(ch)
	if len(listeners) == 0 {
		delete(n.listeners, key)
	}
}

func (n *SessionNotifier) Broadcast(key string) {
	n.mu.Lock()
	listeners := n.listeners[key]
	channels := make([]chan struct{}, 0, len(listeners))
	for ch := range listeners {
		channels = append(channels, ch)
	}
	n.mu.Unlock()

	for _, ch := range channels {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func NewSyncServer(store *SessionStore, defaultUser string, isolateModelUserState bool) *SyncServer {
	return &SyncServer{
		store:                 store,
		defaultUser:           defaultUser,
		isolateModelUserState: isolateModelUserState,
		notifier:              NewSessionNotifier(),
	}
}

func (s *SyncServer) HandleSessions(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/sessions/")
	path = strings.Trim(path, "/")
	if path == "" {
		http.NotFound(w, r)
		return
	}

	parts := strings.Split(path, "/")
	if len(parts) < 2 {
		http.NotFound(w, r)
		return
	}

	requestedUser := strings.TrimSpace(parts[0])
	endpoint := strings.TrimSpace(parts[1])
	user := s.resolveUser(r, requestedUser)
	scope := s.resolveScope(r)

	switch endpoint {
	case "snapshot":
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleSnapshot(w, user, scope)
	case "sync":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleSync(w, r, user, scope)
	case "ws":
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		websocket.Handler(func(ws *websocket.Conn) {
			s.handleWS(ws, user, scope)
		}).ServeHTTP(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *SyncServer) resolveUser(_ *http.Request, requestedUser string) string {
	if requestedUser == "" || requestedUser == "default" {
		return s.defaultUser
	}
	// Placeholder for future auth hooks. Today we trust a single private user setup.
	return requestedUser
}

func (s *SyncServer) resolveScope(r *http.Request) string {
	scope := strings.TrimSpace(r.URL.Query().Get("scope"))
	if scope == "" {
		return "global"
	}
	return scope
}

func (s *SyncServer) handleSnapshot(w http.ResponseWriter, user, scope string) {
	snapshot, err := s.store.GetSnapshot(user, scope)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, snapshot)
}

func (s *SyncServer) handleSync(w http.ResponseWriter, r *http.Request, user, scope string) {
	defer r.Body.Close()

	var payload SessionSyncRequest
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid json payload", http.StatusBadRequest)
		return
	}

	merged, err := s.store.MergeAndSave(user, scope, payload)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.notifier.Broadcast(sessionChannelKey(user, scope))
	writeJSON(w, http.StatusOK, merged)
}

func (s *SyncServer) handleWS(ws *websocket.Conn, user, scope string) {
	defer func() {
		_ = ws.Close()
	}()

	channelKey := sessionChannelKey(user, scope)
	updates := s.notifier.Subscribe(channelKey)
	defer s.notifier.Unsubscribe(channelKey, updates)

	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()

	for {
		select {
		case <-updates:
			if err := websocket.Message.Send(ws, `{"type":"updated"}`); err != nil {
				return
			}
		case <-ping.C:
			if err := websocket.Message.Send(ws, `{"type":"ping"}`); err != nil {
				return
			}
		}
	}
}

func sessionChannelKey(user, scope string) string {
	return user + "|" + scope
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		log.Printf("sync api encode response failed: %v", err)
	}
}
