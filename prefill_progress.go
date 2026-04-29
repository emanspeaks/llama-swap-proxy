package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	prefillDebugEnvName = "LLAMA_SWAP_PROXY_PREFILL_DEBUG"
	prefillDefaultTTL   = 180 * time.Second
	prefillDoneTTL      = 30 * time.Second
)

type prefillProgressKey struct {
	SessionID string
	MessageID string
}

type prefillProgressRecord struct {
	Total     int64
	Cache     int64
	Processed int64
	TimeMS    int64
	Done      bool
	UpdatedAt int64 // unix ms
}

type PrefillProgressResponse struct {
	Found     bool  `json:"found"`
	Total     int64 `json:"total"`
	Cache     int64 `json:"cache"`
	Processed int64 `json:"processed"`
	TimeMS    int64 `json:"time_ms"`
	Done      bool  `json:"done"`
	UpdatedAt int64 `json:"updated_at"`
}

type PrefillProgressTracker struct {
	mu      sync.RWMutex
	records map[prefillProgressKey]prefillProgressRecord

	ttl     time.Duration
	doneTTL time.Duration
	debug   bool

	stopJanitor chan struct{}
	janitorDone chan struct{}
}

type prefillTrackingContext struct {
	Key      prefillProgressKey
	TrackSSE bool
}

type prefillTrackingContextKey struct{}

func NewPrefillProgressTracker(ttl, doneTTL time.Duration, debug bool) *PrefillProgressTracker {
	t := &PrefillProgressTracker{
		records:     make(map[prefillProgressKey]prefillProgressRecord),
		ttl:         ttl,
		doneTTL:     doneTTL,
		debug:       debug,
		stopJanitor: make(chan struct{}),
		janitorDone: make(chan struct{}),
	}
	go t.runJanitor()
	return t
}

func (t *PrefillProgressTracker) Close() {
	close(t.stopJanitor)
	<-t.janitorDone
}

func (t *PrefillProgressTracker) runJanitor() {
	defer close(t.janitorDone)
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-t.stopJanitor:
			return
		case <-ticker.C:
			t.evictExpired()
		}
	}
}

func (t *PrefillProgressTracker) evictExpired() {
	now := time.Now()
	nowMS := now.UnixMilli()

	t.mu.Lock()
	defer t.mu.Unlock()

	for key, rec := range t.records {
		age := now.Sub(time.UnixMilli(rec.UpdatedAt))
		evict := age > t.ttl
		if rec.Done && age > t.doneTTL {
			evict = true
		}
		if !evict {
			continue
		}
		delete(t.records, key)
		if t.debug {
			log.Printf("prefill-progress: evict session=%s message=%s done=%t age_ms=%d", key.SessionID, key.MessageID, rec.Done, nowMS-rec.UpdatedAt)
		}
	}
}

func (t *PrefillProgressTracker) Update(key prefillProgressKey, total, cache, processed, timeMS int64) {
	if key.SessionID == "" || key.MessageID == "" {
		return
	}

	nowMS := time.Now().UnixMilli()
	done := total > 0 && processed >= total

	t.mu.Lock()
	rec := t.records[key]
	rec.Total = total
	rec.Cache = cache
	rec.Processed = processed
	rec.TimeMS = timeMS
	rec.Done = rec.Done || done
	rec.UpdatedAt = nowMS
	t.records[key] = rec
	t.mu.Unlock()

	if t.debug {
		log.Printf("prefill-progress: update session=%s message=%s total=%d cache=%d processed=%d time_ms=%d done=%t", key.SessionID, key.MessageID, total, cache, processed, timeMS, rec.Done)
	}
}

func (t *PrefillProgressTracker) MarkDone(key prefillProgressKey) {
	if key.SessionID == "" || key.MessageID == "" {
		return
	}

	nowMS := time.Now().UnixMilli()
	t.mu.Lock()
	rec := t.records[key]
	rec.Done = true
	rec.UpdatedAt = nowMS
	t.records[key] = rec
	t.mu.Unlock()

	if t.debug {
		log.Printf("prefill-progress: done session=%s message=%s", key.SessionID, key.MessageID)
	}
}

func (t *PrefillProgressTracker) Get(key prefillProgressKey) PrefillProgressResponse {
	if key.SessionID == "" || key.MessageID == "" {
		return PrefillProgressResponse{}
	}

	t.mu.RLock()
	rec, ok := t.records[key]
	t.mu.RUnlock()
	if !ok {
		return PrefillProgressResponse{}
	}

	return PrefillProgressResponse{
		Found:     true,
		Total:     rec.Total,
		Cache:     rec.Cache,
		Processed: rec.Processed,
		TimeMS:    rec.TimeMS,
		Done:      rec.Done,
		UpdatedAt: rec.UpdatedAt,
	}
}

func (t *PrefillProgressTracker) HandleGetPrefillProgress(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	key := prefillProgressKey{
		SessionID: strings.TrimSpace(r.URL.Query().Get("session_id")),
		MessageID: strings.TrimSpace(r.URL.Query().Get("message_id")),
	}
	resp := t.Get(key)
	if t.debug {
		log.Printf("prefill-progress: endpoint hit session=%s message=%s found=%t", key.SessionID, key.MessageID, resp.Found)
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("prefill-progress: encode error: %v", err)
	}
}

func attachPrefillProgressTracking(proxy *httputil.ReverseProxy, tracker *PrefillProgressTracker) {
	if proxy == nil || tracker == nil {
		return
	}

	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalDirector(req)

		key, trackSSE, ok := derivePrefillCorrelationFromRequest(req)
		if !ok || !trackSSE {
			return
		}

		if tracker.debug {
			log.Printf("prefill-progress: key created session=%s message=%s", key.SessionID, key.MessageID)
		}

		ctx := context.WithValue(req.Context(), prefillTrackingContextKey{}, prefillTrackingContext{Key: key, TrackSSE: true})
		*req = *req.WithContext(ctx)
	}

	originalModifyResponse := proxy.ModifyResponse
	proxy.ModifyResponse = func(resp *http.Response) error {
		if originalModifyResponse != nil {
			if err := originalModifyResponse(resp); err != nil {
				return err
			}
		}

		if resp == nil || resp.Request == nil || resp.Body == nil {
			return nil
		}

		ctxInfo, ok := resp.Request.Context().Value(prefillTrackingContextKey{}).(prefillTrackingContext)
		if !ok || !ctxInfo.TrackSSE {
			return nil
		}

		if !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
			return nil
		}

		resp.Body = newPrefillTrackingReadCloser(resp.Body, tracker, ctxInfo.Key)
		return nil
	}
}

func derivePrefillCorrelationFromRequest(r *http.Request) (key prefillProgressKey, trackSSE bool, ok bool) {
	if r == nil {
		return prefillProgressKey{}, false, false
	}

	// Prefer headers when present.
	sessionID := strings.TrimSpace(r.Header.Get("x-opencode-session-id"))
	messageID := strings.TrimSpace(r.Header.Get("x-opencode-message-id"))

	payload, parsed := readJSONRequestBodyAsMap(r)
	if parsed {
		trackSSE = mapBool(payload, "stream")
		if sessionID == "" {
			sessionID = mapString(payload, "session_id", "sessionId")
		}
		if messageID == "" {
			messageID = mapString(payload, "message_id", "messageId")
		}
	}

	if sessionID == "" || messageID == "" {
		return prefillProgressKey{}, trackSSE, false
	}

	return prefillProgressKey{SessionID: sessionID, MessageID: messageID}, trackSSE, true
}

func readJSONRequestBodyAsMap(r *http.Request) (map[string]interface{}, bool) {
	if r == nil || r.Body == nil {
		return nil, false
	}
	bodyBytes, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	r.ContentLength = int64(len(bodyBytes))
	r.Header.Set("Content-Length", strconv.Itoa(len(bodyBytes)))
	if err != nil || len(bodyBytes) == 0 {
		return nil, false
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &payload); err != nil {
		return nil, false
	}
	return payload, true
}

func mapString(m map[string]interface{}, keys ...string) string {
	if m == nil {
		return ""
	}
	for _, key := range keys {
		v, ok := m[key]
		if !ok {
			continue
		}
		s, ok := v.(string)
		if !ok {
			continue
		}
		s = strings.TrimSpace(s)
		if s != "" {
			return s
		}
	}
	return ""
}

func mapBool(m map[string]interface{}, key string) bool {
	if m == nil {
		return false
	}
	v, ok := m[key]
	if !ok {
		return false
	}
	b, ok := v.(bool)
	return ok && b
}

type prefillTrackingReadCloser struct {
	inner   io.ReadCloser
	tracker *PrefillProgressTracker
	key     prefillProgressKey

	lineBuf      []byte
	eventDataBuf []string
	closed       bool
}

func newPrefillTrackingReadCloser(inner io.ReadCloser, tracker *PrefillProgressTracker, key prefillProgressKey) io.ReadCloser {
	return &prefillTrackingReadCloser{
		inner:   inner,
		tracker: tracker,
		key:     key,
	}
}

func (t *prefillTrackingReadCloser) Read(p []byte) (int, error) {
	n, err := t.inner.Read(p)
	if n > 0 {
		t.feed(p[:n])
	}
	if err == io.EOF {
		t.markDone()
	}
	return n, err
}

func (t *prefillTrackingReadCloser) Close() error {
	if t.closed {
		return nil
	}
	t.closed = true
	t.markDone()
	return t.inner.Close()
}

func (t *prefillTrackingReadCloser) markDone() {
	if t.tracker == nil {
		return
	}
	t.tracker.MarkDone(t.key)
}

func (t *prefillTrackingReadCloser) feed(chunk []byte) {
	t.lineBuf = append(t.lineBuf, chunk...)
	for {
		nl := bytes.IndexByte(t.lineBuf, '\n')
		if nl < 0 {
			break
		}
		line := string(bytes.TrimSuffix(t.lineBuf[:nl], []byte{'\r'}))
		t.lineBuf = t.lineBuf[nl+1:]
		t.handleLine(line)
	}
}

func (t *prefillTrackingReadCloser) handleLine(line string) {
	if line == "" {
		t.finalizeEvent()
		return
	}
	if !strings.HasPrefix(line, "data:") {
		return
	}
	payload := strings.TrimPrefix(line, "data:")
	payload = strings.TrimPrefix(payload, " ")
	t.eventDataBuf = append(t.eventDataBuf, payload)
}

func (t *prefillTrackingReadCloser) finalizeEvent() {
	if len(t.eventDataBuf) == 0 {
		return
	}
	raw := strings.Join(t.eventDataBuf, "\n")
	t.eventDataBuf = t.eventDataBuf[:0]

	if raw == "[DONE]" {
		t.markDone()
		return
	}

	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		return
	}
	ppRaw, ok := obj["prompt_progress"]
	if !ok {
		return
	}
	pp, ok := ppRaw.(map[string]interface{})
	if !ok {
		return
	}

	total, okTotal := numToInt64(pp["total"])
	cache, okCache := numToInt64(pp["cache"])
	processed, okProcessed := numToInt64(pp["processed"])
	timeMS, okTime := numToInt64(pp["time_ms"])
	if !(okTotal && okCache && okProcessed && okTime) {
		return
	}

	if t.tracker != nil {
		t.tracker.Update(t.key, total, cache, processed, timeMS)
	}
}

func numToInt64(v interface{}) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case float32:
		return int64(n), true
	case int:
		return int64(n), true
	case int32:
		return int64(n), true
	case int64:
		return n, true
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0, false
		}
		return i, true
	default:
		return 0, false
	}
}

func envBool(name string) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	switch v {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
