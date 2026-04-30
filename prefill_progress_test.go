package main

import (
	"bytes"
	"io"
	"net/http"
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
	if resp.Started {
		t.Fatal("expected started=false when done=true")
	}
}

func TestPrefillEvictionDoneRecord(t *testing.T) {
	tracker := NewPrefillProgressTracker(time.Minute, 5*time.Millisecond, false)
	defer tracker.Close()

	key := prefillProgressKey{SessionID: "evict-s", MessageID: "evict-m"}
	tracker.Update(key, 1, 0, 1, 1, nil)
	tracker.MarkDone(key)
	time.Sleep(10 * time.Millisecond)
	tracker.evictExpired()

	resp := tracker.Get(key)
	if resp.Found {
		t.Fatal("expected done record eviction")
	}
}

func TestPrefillMarkDoneSetsStartedFalse(t *testing.T) {
	tracker := NewPrefillProgressTracker(time.Minute, time.Minute, false)
	defer tracker.Close()

	key := prefillProgressKey{SessionID: "done-s", MessageID: "done-m"}
	raw := map[string]interface{}{"total": float64(10), "cache": float64(0), "processed": float64(4), "time_ms": float64(100)}
	tracker.Update(key, 10, 0, 4, 100, raw)
	tracker.MarkDone(key)

	resp := tracker.Get(key)
	if !resp.Done {
		t.Fatal("expected done=true after MarkDone")
	}
	if resp.Started {
		t.Fatal("expected started=false after MarkDone")
	}
}

func TestPrefillUpdateReopensDoneRecord(t *testing.T) {
	tracker := NewPrefillProgressTracker(time.Minute, time.Minute, false)
	defer tracker.Close()

	key := prefillProgressKey{SessionID: "reopen-s", MessageID: "reopen-m"}
	rawDone := map[string]interface{}{"total": float64(10), "cache": float64(0), "processed": float64(10), "time_ms": float64(250)}
	tracker.Update(key, 10, 0, 10, 250, rawDone)

	first := tracker.Get(key)
	if !first.Done || first.Started {
		t.Fatalf("expected initial record done=true started=false: %+v", first)
	}

	rawRestart := map[string]interface{}{"total": float64(10), "cache": float64(0), "processed": float64(0), "time_ms": float64(1)}
	tracker.Update(key, 10, 0, 0, 1, rawRestart)

	second := tracker.Get(key)
	if !second.Found {
		t.Fatal("expected found=true after reopen update")
	}
	if second.Done {
		t.Fatalf("expected done=false after reopen update: %+v", second)
	}
	if !second.Started {
		t.Fatalf("expected started=true after reopen update: %+v", second)
	}
}

func TestPrefillUpdateRecreatesEvictedRecord(t *testing.T) {
	tracker := NewPrefillProgressTracker(time.Minute, 5*time.Millisecond, false)
	defer tracker.Close()

	key := prefillProgressKey{SessionID: "recreate-s", MessageID: "recreate-m"}
	rawDone := map[string]interface{}{"total": float64(8), "cache": float64(0), "processed": float64(8), "time_ms": float64(200)}
	tracker.Update(key, 8, 0, 8, 200, rawDone)
	time.Sleep(10 * time.Millisecond)
	tracker.evictExpired()

	missing := tracker.Get(key)
	if missing.Found {
		t.Fatalf("expected record evicted before recreate: %+v", missing)
	}

	rawNew := map[string]interface{}{"total": float64(8), "cache": float64(0), "processed": float64(2), "time_ms": float64(25)}
	tracker.Update(key, 8, 0, 2, 25, rawNew)

	recreated := tracker.Get(key)
	if !recreated.Found {
		t.Fatal("expected found=true after recreate update")
	}
	if recreated.Done {
		t.Fatalf("expected done=false for recreated in-progress record: %+v", recreated)
	}
	if !recreated.Started {
		t.Fatalf("expected started=true for recreated in-progress record: %+v", recreated)
	}
}
