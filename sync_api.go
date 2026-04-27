package main

import (
	"encoding/json"
	"fmt"
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

type wsInboundMessage struct {
	Type     string              `json:"type"`
	ClientID string              `json:"clientId,omitempty"`
	Payload  *SessionSyncRequest `json:"payload,omitempty"`
}

type wsOutboundMessage struct {
	Type     string           `json:"type"`
	Sender   string           `json:"sender,omitempty"`
	Snapshot *SessionSnapshot `json:"snapshot,omitempty"`
	Error    string           `json:"error,omitempty"`
}

type SessionNotifier struct {
	mu        sync.Mutex
	listeners map[string]map[string]chan wsOutboundMessage
}

func NewSessionNotifier() *SessionNotifier {
	return &SessionNotifier{listeners: map[string]map[string]chan wsOutboundMessage{}}
}

func (n *SessionNotifier) Subscribe(key, clientID string) chan wsOutboundMessage {
	ch := make(chan wsOutboundMessage, 8)
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.listeners[key] == nil {
		n.listeners[key] = map[string]chan wsOutboundMessage{}
	}
	n.listeners[key][clientID] = ch
	return ch
}

func (n *SessionNotifier) Unsubscribe(key, clientID string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	listeners := n.listeners[key]
	if listeners == nil {
		return
	}
	ch, ok := listeners[clientID]
	if ok {
		delete(listeners, clientID)
		close(ch)
	}
	if len(listeners) == 0 {
		delete(n.listeners, key)
	}
}

func (n *SessionNotifier) BroadcastSnapshot(key, senderClientID string, snapshot SessionSnapshot) {
	n.mu.Lock()
	listeners := n.listeners[key]
	channels := make([]chan wsOutboundMessage, 0, len(listeners))
	for clientID, ch := range listeners {
		if clientID == senderClientID {
			continue
		}
		channels = append(channels, ch)
	}
	n.mu.Unlock()

	message := wsOutboundMessage{
		Type:     "snapshot",
		Sender:   senderClientID,
		Snapshot: &snapshot,
	}
	for _, ch := range channels {
		select {
		case ch <- message:
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

	s.notifier.BroadcastSnapshot(sessionChannelKey(user, scope), payload.ClientID, merged)
	writeJSON(w, http.StatusOK, merged)
}

func (s *SyncServer) handleWS(ws *websocket.Conn, user, scope string) {
	clientID := strings.TrimSpace(ws.Request().URL.Query().Get("clientId"))
	if clientID == "" {
		clientID = fmt.Sprintf("ws-%d", time.Now().UnixNano())
	}

	log.Printf("sessions ws connected user=%s scope=%s client_id=%s", user, scope, clientID)
	defer func() {
		log.Printf("sessions ws disconnected user=%s scope=%s client_id=%s", user, scope, clientID)
		_ = ws.Close()
	}()

	channelKey := sessionChannelKey(user, scope)
	outgoing := s.notifier.Subscribe(channelKey, clientID)
	defer s.notifier.Unsubscribe(channelKey, clientID)

	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for message := range outgoing {
			if err := websocket.JSON.Send(ws, message); err != nil {
				return
			}
		}
	}()

	snapshot, err := s.store.GetSnapshot(user, scope)
	if err != nil {
		select {
		case outgoing <- wsOutboundMessage{Type: "error", Error: err.Error()}:
		default:
		}
	} else {
		select {
		case outgoing <- wsOutboundMessage{Type: "snapshot", Snapshot: &snapshot}:
		default:
		}
	}

	for {
		var incoming wsInboundMessage
		if err := websocket.JSON.Receive(ws, &incoming); err != nil {
			break
		}

		switch incoming.Type {
		case "sync":
			if incoming.Payload == nil {
				select {
				case outgoing <- wsOutboundMessage{Type: "error", Error: "missing payload for sync"}:
				default:
				}
				continue
			}

			payload := *incoming.Payload
			if payload.ClientID == "" {
				payload.ClientID = clientID
			}

			merged, err := s.store.MergeAndSave(user, scope, payload)
			if err != nil {
				select {
				case outgoing <- wsOutboundMessage{Type: "error", Error: err.Error()}:
				default:
				}
				continue
			}

			// Send merged snapshot back to sender and fan out to other clients.
			select {
			case outgoing <- wsOutboundMessage{Type: "snapshot", Sender: clientID, Snapshot: &merged}:
			default:
			}
			s.notifier.BroadcastSnapshot(channelKey, clientID, merged)

		case "get-snapshot", "snapshot":
			snapshot, err := s.store.GetSnapshot(user, scope)
			if err != nil {
				select {
				case outgoing <- wsOutboundMessage{Type: "error", Error: err.Error()}:
				default:
				}
				continue
			}
			select {
			case outgoing <- wsOutboundMessage{Type: "snapshot", Snapshot: &snapshot}:
			default:
			}

		case "ping":
			select {
			case outgoing <- wsOutboundMessage{Type: "pong"}:
			default:
			}
		}
	}

	<-writerDone
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
