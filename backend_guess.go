package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

type ModelBackendGuesser interface {
	IsLlamaCppModel(modelID string) (isLlamaCpp bool, resolved bool)
}

type BackendGuesser struct {
	upstream string
	client   *http.Client
	ttl      time.Duration

	mu               sync.RWMutex
	expiresAt        time.Time
	modelTypeByID    map[string]string
	runningProxyByID map[string]string
}

func NewBackendGuesser(upstream string) *BackendGuesser {
	return &BackendGuesser{
		upstream:         upstream,
		client:           &http.Client{Timeout: 5 * time.Second},
		ttl:              2 * time.Second,
		modelTypeByID:    map[string]string{},
		runningProxyByID: map[string]string{},
	}
}

func (g *BackendGuesser) IsLlamaCppModel(modelID string) (bool, bool) {
	if strings.TrimSpace(modelID) == "" {
		return false, false
	}
	if err := g.ensureFresh(); err != nil {
		return false, false
	}

	g.mu.RLock()
	modelType, ok := g.modelTypeByID[modelID]
	g.mu.RUnlock()
	if !ok {
		return false, false
	}
	return classifyLlamaCppModelType(modelType)
}

func (g *BackendGuesser) FindRunningSDProxy() (modelID, proxyURL string, err error) {
	if err := g.ensureFresh(); err != nil {
		return "", "", err
	}

	g.mu.RLock()
	defer g.mu.RUnlock()
	for id, modelType := range g.modelTypeByID {
		if !strings.EqualFold(strings.TrimSpace(modelType), "sd") {
			continue
		}
		proxy, ok := g.runningProxyByID[id]
		if !ok {
			continue
		}
		return id, proxy, nil
	}

	return "", "", fmt.Errorf("no running sd model found")
}

func classifyLlamaCppModelType(modelType string) (isLlamaCpp bool, resolved bool) {
	mt := strings.ToLower(strings.TrimSpace(modelType))
	if mt == "" {
		return false, false
	}

	switch mt {
	case "sd", "stable-diffusion", "stable_diffusion", "sdcpp", "ollama", "vllm", "openai", "tgi":
		return false, true
	case "llm", "reranking", "embedding":
		return true, true
	default:
		// Keep compatibility with existing non-sd behavior for unknown future
		// llama-swap model_type values.
		return true, true
	}
}

func (g *BackendGuesser) ensureFresh() error {
	now := time.Now()

	g.mu.RLock()
	isFresh := now.Before(g.expiresAt)
	g.mu.RUnlock()
	if isFresh {
		return nil
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	if now.Before(g.expiresAt) {
		return nil
	}

	modelTypeByID, err := g.fetchModelTypes()
	if err != nil {
		return err
	}
	runningProxyByID, err := g.fetchRunningProxies()
	if err != nil {
		return err
	}

	g.modelTypeByID = modelTypeByID
	g.runningProxyByID = runningProxyByID
	g.expiresAt = time.Now().Add(g.ttl)
	return nil
}

func (g *BackendGuesser) fetchModelTypes() (map[string]string, error) {
	resp, err := g.client.Get(g.upstream + "/v1/models")
	if err != nil {
		return nil, fmt.Errorf("fetching /v1/models: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading /v1/models: %w", err)
	}

	var models ModelsResponse
	if err := json.Unmarshal(body, &models); err != nil {
		return nil, fmt.Errorf("parsing /v1/models: %w", err)
	}

	byID := make(map[string]string, len(models.Data))
	for _, entry := range models.Data {
		id := strings.TrimSpace(entry.ID)
		if id == "" {
			continue
		}
		byID[id] = entry.Meta.LlamaSwap.ModelType
	}
	return byID, nil
}

func (g *BackendGuesser) fetchRunningProxies() (map[string]string, error) {
	resp, err := g.client.Get(g.upstream + "/running")
	if err != nil {
		return nil, fmt.Errorf("fetching /running: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading /running: %w", err)
	}

	var running RunningResponse
	if err := json.Unmarshal(body, &running); err != nil {
		return nil, fmt.Errorf("parsing /running: %w", err)
	}

	byModel := make(map[string]string, len(running.Running))
	for _, m := range running.Running {
		id := strings.TrimSpace(m.Model)
		if id == "" {
			continue
		}
		byModel[id] = m.Proxy
	}
	return byModel, nil
}
