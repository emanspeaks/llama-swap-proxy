package main

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
)

// PrefillWSPush is the JSON pushed to connected clients on every state change.
type PrefillWSPush struct {
	SessionID string `json:"session_id"`
	Total     int64  `json:"total"`
	Cache     int64  `json:"cache"`
	Processed int64  `json:"processed"`
	TimeMS    int64  `json:"time_ms"`
	Started   bool   `json:"started"`
	Done      bool   `json:"done"`
}

// PrefillWSHub manages all connected WebSocket clients and broadcasts pushes.
type PrefillWSHub struct {
	mu       sync.Mutex
	clients  map[*prefillWSClient]struct{}
	upgrader websocket.Upgrader
}

type prefillWSClient struct {
	hub  *PrefillWSHub
	conn *websocket.Conn
	send chan []byte
	done chan struct{}
}

func NewPrefillWSHub() *PrefillWSHub {
	return &PrefillWSHub{
		clients: make(map[*prefillWSClient]struct{}),
		upgrader: websocket.Upgrader{
			// Allow all origins; the proxy is a local service.
			CheckOrigin: func(r *http.Request) bool { return true },
		},
	}
}

// Broadcast encodes msg as JSON and sends it to every connected client.
// Slow clients that cannot accept the message before the send buffer fills
// are silently skipped — they will catch the next update.
func (h *PrefillWSHub) Broadcast(msg PrefillWSPush) {
	data, err := json.Marshal(msg)
	if err != nil {
		log.Printf("prefill-ws: marshal error: %v", err)
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		select {
		case c.send <- data:
		default:
			// send buffer full — drop this update for the slow client
		}
	}
}

func (h *PrefillWSHub) register(c *prefillWSClient) {
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
}

func (h *PrefillWSHub) unregister(c *prefillWSClient) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
}

// HandlePrefillWS upgrades the connection to WebSocket and serves server-push
// prefill-progress messages.  Clients never send anything; the handler simply
// drains incoming frames to detect close frames.
func (h *PrefillWSHub) HandlePrefillWS(w http.ResponseWriter, r *http.Request) {
	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("prefill-ws: upgrade error: %v", err)
		return
	}
	c := &prefillWSClient{
		hub:  h,
		conn: conn,
		send: make(chan []byte, 64),
		done: make(chan struct{}),
	}
	h.register(c)
	go c.writePump()
	c.readPump() // blocks; unregisters and signals writePump when client disconnects
}

// readPump drains incoming WebSocket frames (clients send nothing) and detects
// connection close.  It unregisters the client and signals writePump to exit.
func (c *prefillWSClient) readPump() {
	defer func() {
		c.hub.unregister(c)
		close(c.done)
		c.conn.Close()
	}()
	for {
		if _, _, err := c.conn.ReadMessage(); err != nil {
			return
		}
	}
}

// writePump sends queued messages to the WebSocket connection until the client
// disconnects or a write fails.
func (c *prefillWSClient) writePump() {
	for {
		select {
		case msg, ok := <-c.send:
			if !ok {
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-c.done:
			return
		}
	}
}
