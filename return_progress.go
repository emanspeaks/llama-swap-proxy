package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
)

// maybeInjectReturnProgress reads the request body and, when the request is a
// streaming JSON body without an explicit return_progress key, injects
// return_progress: true.  The body is always restored so the request can
// continue to the upstream regardless of whether injection occurred.
//
// Conditions for injection (all must be true):
//   - Body is non-empty and parses as a JSON object.
//   - Body contains "stream": true.
//   - Body does NOT already contain a "return_progress" key.
//
// If any condition is not met the original body bytes are restored unchanged.
// Panics are not possible; a JSON parse failure simply restores the original body.
func maybeInjectReturnProgress(r *http.Request, guesser ModelBackendGuesser) {
	if r.Body == nil {
		return
	}

	bodyBytes, err := io.ReadAll(r.Body)
	_ = r.Body.Close()

	restore := func(b []byte) {
		r.Body = io.NopCloser(bytes.NewReader(b))
	}

	if err != nil || len(bodyBytes) == 0 {
		restore(bodyBytes)
		return
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &payload); err != nil {
		// Not valid JSON — pass through unchanged, let the backend reject it.
		restore(bodyBytes)
		return
	}

	// Only inject for streaming requests.
	streamVal, hasStream := payload["stream"]
	if !hasStream {
		restore(bodyBytes)
		return
	}
	streaming, ok := streamVal.(bool)
	if !ok || !streaming {
		restore(bodyBytes)
		return
	}

	// Respect an explicit client value (true or false).
	if _, hasRP := payload["return_progress"]; hasRP {
		restore(bodyBytes)
		return
	}

	// Only inject when we can resolve the request model to a llama.cpp backend.
	modelID := resolveRequestModelID(r, payload)
	if modelID == "" {
		restore(bodyBytes)
		return
	}
	if guesser == nil {
		restore(bodyBytes)
		return
	}
	isLlamaCpp, resolved := guesser.IsLlamaCppModel(modelID)
	if !resolved || !isLlamaCpp {
		restore(bodyBytes)
		return
	}

	payload["return_progress"] = true
	newBody, err := json.Marshal(payload)
	if err != nil {
		restore(bodyBytes)
		return
	}

	r.Body = io.NopCloser(bytes.NewReader(newBody))
	r.ContentLength = int64(len(newBody))
	r.Header.Set("Content-Length", strconv.Itoa(len(newBody)))
}
