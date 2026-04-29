package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type nopCloseReader struct {
	*bytes.Reader
}

func (n nopCloseReader) Close() error { return nil }

func TestDerivePrefillCorrelationFromRequest_HTTPHeadersPreferred(t *testing.T) {
	r, _ := http.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"stream":true,"headers":{"x-opencode-session-id":"json-s","x-opencode-message-id":"json-m"}}`))
	r.Header.Set("x-opencode-session-id", "http-s")
	r.Header.Set("x-opencode-message-id", "http-m")

	key, trackSSE, ok := derivePrefillCorrelationFromRequest(r)
	if !ok {
		t.Fatal("expected correlation key")
	}
	if !trackSSE {
		t.Fatal("expected trackSSE=true")
	}
	if key.SessionID != "http-s" || key.MessageID != "http-m" {
		t.Fatalf("unexpected key: %+v", key)
	}
}

func TestDerivePrefillCorrelationFromRequest_PayloadHeadersFallback(t *testing.T) {
	r, _ := http.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"stream":true,"headers":{"x-opencode-session-id":"json-s","x-opencode-message-id":"json-m"}}`))

	key, trackSSE, ok := derivePrefillCorrelationFromRequest(r)
	if !ok {
		t.Fatal("expected correlation key from payload headers")
	}
	if !trackSSE {
		t.Fatal("expected trackSSE=true")
	}
	if key.SessionID != "json-s" || key.MessageID != "json-m" {
		t.Fatalf("unexpected key: %+v", key)
	}
}

func TestDerivePrefillCorrelationFromRequest_RequiresHTTPOrPayloadHeaders(t *testing.T) {
	r, _ := http.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"stream":true,"session_id":"s1","message_id":"m1"}`))

	key, trackSSE, ok := derivePrefillCorrelationFromRequest(r)
	if !trackSSE {
		t.Fatal("expected trackSSE=true")
	}
	if ok {
		t.Fatalf("expected no correlation key, got %+v", key)
	}
}

func TestPrefillTrackingReadCloser_UpdatesProgress(t *testing.T) {
	tracker := NewPrefillProgressTracker(time.Minute, time.Minute, false)
	defer tracker.Close()

	key := prefillProgressKey{SessionID: "s", MessageID: "m"}
	payload := "data: {\"prompt_progress\":{\"total\":100,\"cache\":10,\"processed\":40,\"time_ms\":1200}}\n\n"
	inner := nopCloseReader{Reader: bytes.NewReader([]byte(payload))}
	rc := newPrefillTrackingReadCloser(inner, tracker, key)
	_, _ = io.Copy(io.Discard, rc)
	_ = rc.Close()

	resp := tracker.Get(key)
	if !resp.Found {
		t.Fatal("expected found=true")
	}
	if resp.Total != 100 || resp.Cache != 10 || resp.Processed != 40 || resp.TimeMS != 1200 {
		t.Fatalf("unexpected progress: %+v", resp)
	}
	if !resp.Done {
		t.Fatal("expected done=true when stream closes")
	}
}

func TestPrefillTrackingReadCloser_DoneWhenProcessedReachesTotal(t *testing.T) {
	tracker := NewPrefillProgressTracker(time.Minute, time.Minute, false)
	defer tracker.Close()

	key := prefillProgressKey{SessionID: "s2", MessageID: "m2"}
	payload := "data: {\"prompt_progress\":{\"total\":5,\"cache\":1,\"processed\":5,\"time_ms\":200}}\n\n"
	inner := nopCloseReader{Reader: bytes.NewReader([]byte(payload))}
	rc := newPrefillTrackingReadCloser(inner, tracker, key)
	_, _ = io.Copy(io.Discard, rc)
	_ = rc.Close()

	resp := tracker.Get(key)
	if !resp.Done {
		t.Fatal("expected done=true from prompt_progress threshold")
	}
}

func TestPrefillEndpoint_DefaultAndFound(t *testing.T) {
	tracker := NewPrefillProgressTracker(time.Minute, time.Minute, false)
	defer tracker.Close()

	// Missing record -> defaults
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/prefill-progress?session_id=a&message_id=b", nil)
	tracker.HandleGetPrefillProgress(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d", rr.Code)
	}
	var resp PrefillProgressResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Found || resp.Total != 0 || resp.Cache != 0 || resp.Processed != 0 || resp.TimeMS != 0 || resp.Done || resp.UpdatedAt != 0 {
		t.Fatalf("unexpected default response: %+v", resp)
	}

	// Existing record -> found
	key := prefillProgressKey{SessionID: "a", MessageID: "b"}
	tracker.Update(key, 99, 3, 20, 555)
	rr2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/prefill-progress?session_id=a&message_id=b", nil)
	tracker.HandleGetPrefillProgress(rr2, req2)
	var resp2 PrefillProgressResponse
	if err := json.Unmarshal(rr2.Body.Bytes(), &resp2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp2.Found || resp2.Total != 99 || resp2.Cache != 3 || resp2.Processed != 20 || resp2.TimeMS != 555 {
		t.Fatalf("unexpected found response: %+v", resp2)
	}
}

func TestPrefillEvictionDoneRecord(t *testing.T) {
	tracker := NewPrefillProgressTracker(time.Minute, 5*time.Millisecond, false)
	defer tracker.Close()

	key := prefillProgressKey{SessionID: "evict-s", MessageID: "evict-m"}
	tracker.Update(key, 1, 0, 1, 1)
	tracker.MarkDone(key)
	time.Sleep(10 * time.Millisecond)
	tracker.evictExpired()

	resp := tracker.Get(key)
	if resp.Found {
		t.Fatal("expected done record eviction")
	}
}
