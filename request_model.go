package main

import (
	"net/http"
	"strings"
)

func modelIDFromUpstreamPath(path string) string {
	trimmed := strings.TrimPrefix(path, "/upstream/")
	if trimmed == "" || trimmed == path {
		return ""
	}
	parts := strings.SplitN(trimmed, "/", 2)
	return strings.TrimSpace(parts[0])
}

func resolveRequestModelID(r *http.Request, payload map[string]interface{}) string {
	if payload != nil {
		if modelValue, ok := payload["model"]; ok {
			if modelID, ok := modelValue.(string); ok {
				modelID = strings.TrimSpace(modelID)
				if modelID != "" {
					return modelID
				}
			}
		}
	}

	if r == nil || r.URL == nil {
		return ""
	}

	return modelIDFromUpstreamPath(r.URL.Path)
}
