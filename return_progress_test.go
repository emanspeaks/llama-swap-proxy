package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"testing"
)

type staticGuesser struct {
	isLlama  bool
	resolved bool
}

func (g staticGuesser) IsLlamaCppModel(modelID string) (bool, bool) {
	return g.isLlama, g.resolved
}

// makeStreamingRequest builds a POST request with the given JSON body.
func makeStreamingRequest(body string) *http.Request {
	r, _ := http.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	r.Header.Set("Content-Type", "application/json")
	r.ContentLength = int64(len(body))
	return r
}

// decodeBody reads and JSON-decodes the request body. The body is consumed.
func decodeBody(r *http.Request) map[string]interface{} {
	if r.Body == nil {
		return nil
	}
	b, _ := io.ReadAll(r.Body)
	var m map[string]interface{}
	_ = json.Unmarshal(b, &m)
	return m
}

// TestInjectReturnProgress_StreamTrue_NoKey verifies that return_progress is
// injected when stream:true is present and return_progress is absent.
func TestInjectReturnProgress_StreamTrue_NoKey(t *testing.T) {
	r := makeStreamingRequest(`{"model":"test","stream":true}`)
	maybeInjectReturnProgress(r, staticGuesser{isLlama: true, resolved: true})
	m := decodeBody(r)
	if m["return_progress"] != true {
		t.Errorf("want return_progress=true, got %v", m["return_progress"])
	}
}

// TestInjectReturnProgress_StreamTrue_ReturnProgressFalse verifies that an
// explicit return_progress:false set by the client is left unchanged.
func TestInjectReturnProgress_StreamTrue_ReturnProgressFalse(t *testing.T) {
	r := makeStreamingRequest(`{"model":"test","stream":true,"return_progress":false}`)
	maybeInjectReturnProgress(r, staticGuesser{isLlama: true, resolved: true})
	m := decodeBody(r)
	if m["return_progress"] != false {
		t.Errorf("want return_progress=false (unchanged), got %v", m["return_progress"])
	}
}

// TestInjectReturnProgress_StreamTrue_ReturnProgressTrue verifies that an
// existing return_progress:true is left unchanged (no double-injection).
func TestInjectReturnProgress_StreamTrue_ReturnProgressTrue(t *testing.T) {
	r := makeStreamingRequest(`{"model":"test","stream":true,"return_progress":true}`)
	maybeInjectReturnProgress(r, staticGuesser{isLlama: true, resolved: true})
	m := decodeBody(r)
	if m["return_progress"] != true {
		t.Errorf("want return_progress=true (unchanged), got %v", m["return_progress"])
	}
}

// TestInjectReturnProgress_StreamFalse verifies that non-streaming requests
// are never modified.
func TestInjectReturnProgress_StreamFalse(t *testing.T) {
	r := makeStreamingRequest(`{"model":"test","stream":false}`)
	maybeInjectReturnProgress(r, staticGuesser{isLlama: true, resolved: true})
	m := decodeBody(r)
	if _, ok := m["return_progress"]; ok {
		t.Error("expected no return_progress field when stream=false")
	}
}

// TestInjectReturnProgress_NoStreamField verifies that requests without a
// stream field are not modified (OpenAI spec defaults stream to false).
func TestInjectReturnProgress_NoStreamField(t *testing.T) {
	r := makeStreamingRequest(`{"model":"test","messages":[]}`)
	maybeInjectReturnProgress(r, staticGuesser{isLlama: true, resolved: true})
	m := decodeBody(r)
	if _, ok := m["return_progress"]; ok {
		t.Error("expected no return_progress field when stream key is absent")
	}
}

// TestInjectReturnProgress_MalformedJSON verifies that a malformed body is
// passed through unchanged without panicking.
func TestInjectReturnProgress_MalformedJSON(t *testing.T) {
	r := makeStreamingRequest(`{invalid json`)
	maybeInjectReturnProgress(r, staticGuesser{isLlama: true, resolved: true}) // must not panic
	if r.Body == nil {
		t.Fatal("expected body to be non-nil after malformed JSON")
	}
	raw, _ := io.ReadAll(r.Body)
	if string(raw) != `{invalid json` {
		t.Errorf("body was altered for malformed JSON: got %q", raw)
	}
}

// TestInjectReturnProgress_EmptyBody verifies that an empty body does not panic.
func TestInjectReturnProgress_EmptyBody(t *testing.T) {
	r, _ := http.NewRequest(http.MethodPost, "/v1/chat/completions", http.NoBody)
	maybeInjectReturnProgress(r, staticGuesser{isLlama: true, resolved: true}) // must not panic
}

// TestInjectReturnProgress_NilBody verifies that a nil body does not panic.
func TestInjectReturnProgress_NilBody(t *testing.T) {
	r, _ := http.NewRequest(http.MethodGet, "/health", nil)
	r.Body = nil
	maybeInjectReturnProgress(r, staticGuesser{isLlama: true, resolved: true}) // must not panic
}

// TestInjectReturnProgress_ContentLength verifies that Content-Length is
// updated to match the new (longer) body after injection.
func TestInjectReturnProgress_ContentLength(t *testing.T) {
	body := `{"model":"test","stream":true}`
	r := makeStreamingRequest(body)
	maybeInjectReturnProgress(r, staticGuesser{isLlama: true, resolved: true})

	newBody, _ := io.ReadAll(r.Body)
	expectedLen := len(newBody)

	if r.ContentLength != int64(expectedLen) {
		t.Errorf("r.ContentLength: got %d, want %d", r.ContentLength, expectedLen)
	}
	headerLen, _ := strconv.Atoi(r.Header.Get("Content-Length"))
	if headerLen != expectedLen {
		t.Errorf("Content-Length header: got %d, want %d", headerLen, expectedLen)
	}
	if expectedLen <= len(body) {
		t.Errorf("new body (%d bytes) should be longer than original (%d bytes)", expectedLen, len(body))
	}
}

// TestInjectReturnProgress_DisabledViaFlag simulates the handler-level guard:
// when the flag is set, maybeInjectReturnProgress is not called, so the body
// is never mutated even when stream:true and return_progress is absent.
func TestInjectReturnProgress_DisabledViaFlag(t *testing.T) {
	body := `{"model":"test","stream":true}`
	r := makeStreamingRequest(body)

	// Simulate flag=true: skip the call entirely, as the handler does.
	disableInjection := true
	if !disableInjection {
		maybeInjectReturnProgress(r, staticGuesser{isLlama: true, resolved: true})
	}

	m := decodeBody(r)
	if _, ok := m["return_progress"]; ok {
		t.Error("expected no return_progress when injection is disabled")
	}
}

// TestInjectReturnProgress_FieldsPreserved verifies unknown fields survive the
// body parse/mutate/marshal round-trip used for injection.
func TestInjectReturnProgress_FieldsPreserved(t *testing.T) {
	// Extra unknown fields must survive the round-trip.
	r := makeStreamingRequest(`{"model":"test","stream":true,"temperature":0.7,"custom_ext":42}`)
	maybeInjectReturnProgress(r, staticGuesser{isLlama: true, resolved: true})
	m := decodeBody(r)
	if m["return_progress"] != true {
		t.Errorf("expected return_progress=true, got %v", m["return_progress"])
	}
	if m["temperature"] != 0.7 {
		t.Errorf("expected temperature=0.7 preserved, got %v", m["temperature"])
	}
	if m["custom_ext"] != float64(42) {
		t.Errorf("expected custom_ext=42 preserved, got %v", m["custom_ext"])
	}
}

func TestInjectReturnProgress_NonLlamaCppBackend(t *testing.T) {
	r := makeStreamingRequest(`{"model":"test","stream":true}`)
	maybeInjectReturnProgress(r, staticGuesser{isLlama: false, resolved: true})
	m := decodeBody(r)
	if _, ok := m["return_progress"]; ok {
		t.Error("expected no return_progress for non-llama.cpp backend")
	}
}

func TestInjectReturnProgress_UnresolvedModel(t *testing.T) {
	r := makeStreamingRequest(`{"model":"test","stream":true}`)
	maybeInjectReturnProgress(r, staticGuesser{isLlama: false, resolved: false})
	m := decodeBody(r)
	if _, ok := m["return_progress"]; ok {
		t.Error("expected no return_progress when backend could not be resolved")
	}
}

func TestInjectReturnProgress_MissingModelField(t *testing.T) {
	r := makeStreamingRequest(`{"stream":true}`)
	maybeInjectReturnProgress(r, staticGuesser{isLlama: true, resolved: true})
	m := decodeBody(r)
	if _, ok := m["return_progress"]; ok {
		t.Error("expected no return_progress when model field is missing")
	}
}

func TestInjectReturnProgress_ModelFromUpstreamURL(t *testing.T) {
	r, _ := http.NewRequest(http.MethodPost, "/upstream/qwen3/v1/chat/completions", bytes.NewBufferString(`{"stream":true}`))
	r.Header.Set("Content-Type", "application/json")
	r.ContentLength = int64(len(`{"stream":true}`))

	maybeInjectReturnProgress(r, staticGuesser{isLlama: true, resolved: true})
	m := decodeBody(r)
	if m["return_progress"] != true {
		t.Errorf("expected return_progress=true when model derived from URL, got %v", m["return_progress"])
	}
}
