package main

import (
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed VERSION
var version string

func init() { version = strings.TrimSpace(version) }

// llama-swap /running response
type RunningResponse struct {
	Running []RunningModel `json:"running"`
}

type RunningModel struct {
	Model string `json:"model"`
	Proxy string `json:"proxy"`
	State string `json:"state"`
}

// llama-swap /v1/models response
type ModelsResponse struct {
	Data []ModelEntry `json:"data"`
}

type ModelEntry struct {
	ID   string    `json:"id"`
	Meta ModelMeta `json:"meta"`
}

type ModelMeta struct {
	LlamaSwap LlamaSwapMeta `json:"llamaswap"`
}

type LlamaSwapMeta struct {
	ModelType    string          `json:"model_type"`
	Context      int             `json:"context"`
	MaxContext   int             `json:"max_context"`
	Output       int             `json:"output"`
	Capabilities json.RawMessage `json:"capabilities"`
}

// transformed /v1/models response for opencode / openai-compatible clients
type ProxyModelEntry struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	OwnedBy string `json:"owned_by"`
	// context_length  = loaded context (-c flag); effective request limit.
	// max_context_length = architectural max (metadata.max_context); falls back to context_length if not set.
	ContextLength    int             `json:"context_length,omitempty"`
	MaxContextLength int             `json:"max_context_length,omitempty"`
	Capabilities     json.RawMessage `json:"capabilities,omitempty"`
}

type ProxyModelsResponse struct {
	Object string           `json:"object"`
	Data   []ProxyModelEntry `json:"data"`
}

// llama-swap config.yaml structures
type LlamaSwapConfigFile struct {
	Macros map[string]string            `yaml:"macros"`
	Models map[string]LlamaSwapModelDef `yaml:"models"`
}

type LlamaSwapModelDef struct {
	Cmd      string                 `yaml:"cmd"`
	Metadata map[string]interface{} `yaml:"metadata"`
}

// llama-server /props response (minimal)
type LlamaServerProps struct {
	DefaultGenerationSettings struct {
		NCtx int `json:"n_ctx"`
	} `json:"default_generation_settings"`
}

// /opencode config response
type OpenCodeConfig struct {
	Schema   string                      `json:"$schema"`
	Provider map[string]OpenCodeProvider `json:"provider"`
}

type OpenCodeProvider struct {
	NPM     string                   `json:"npm"`
	Name    string                   `json:"name"`
	Options OpenCodeProviderOptions  `json:"options"`
	Models  map[string]OpenCodeModel `json:"models,omitempty"`
}

type OpenCodeProviderOptions struct {
	BaseURL string `json:"baseURL"`
}

type OpenCodeModel struct {
	Name       string              `json:"name,omitempty"`
	Reasoning  bool                `json:"reasoning,omitempty"`
	ToolCall   bool                `json:"tool_call,omitempty"`
	Attachment bool                `json:"attachment,omitempty"`
	Limit      *OpenCodeLimit      `json:"limit,omitempty"`
	Modalities *OpenCodeModalities `json:"modalities,omitempty"`
}

type OpenCodeLimit struct {
	Context int `json:"context"`
	Output  int `json:"output"`
}

type OpenCodeModalities struct {
	Input  []string `json:"input"`
	Output []string `json:"output"`
}

var reFlagC = regexp.MustCompile(`-c\s+(\d+)`)
var reFlagM = regexp.MustCompile(`-m\s+(\S+)`)

func expandMacros(s string, macros map[string]string) string {
	for i := 0; i < 5; i++ {
		prev := s
		for k, v := range macros {
			s = strings.ReplaceAll(s, "${"+k+"}", v)
		}
		if s == prev {
			break
		}
	}
	return s
}

func parseContext(cmd string) int {
	m := reFlagC.FindStringSubmatch(cmd)
	if len(m) < 2 {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

func parseModelPath(cmd string) string {
	m := reFlagM.FindStringSubmatch(cmd)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// queryProps fetches n_ctx from a running llama-server instance.
// Returns 0 on any error so callers can fall back to config-derived values.
func queryProps(proxyURL string) int {
	resp, err := http.Get(proxyURL + "/props")
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0
	}
	var props LlamaServerProps
	if err := json.Unmarshal(body, &props); err != nil {
		return 0
	}
	return props.DefaultGenerationSettings.NCtx
}

func findRunningSDProxy(upstream string) (modelID, proxyURL string, err error) {
	resp, err := http.Get(upstream + "/running")
	if err != nil {
		return "", "", fmt.Errorf("fetching /running: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", fmt.Errorf("reading /running: %w", err)
	}
	var running RunningResponse
	if err := json.Unmarshal(body, &running); err != nil {
		return "", "", fmt.Errorf("parsing /running: %w", err)
	}

	if len(running.Running) == 0 {
		return "", "", fmt.Errorf("no models currently running")
	}

	runningByModel := make(map[string]string)
	for _, m := range running.Running {
		runningByModel[m.Model] = m.Proxy
	}

	resp2, err := http.Get(upstream + "/v1/models")
	if err != nil {
		return "", "", fmt.Errorf("fetching /v1/models: %w", err)
	}
	defer resp2.Body.Close()
	body2, err := io.ReadAll(resp2.Body)
	if err != nil {
		return "", "", fmt.Errorf("reading /v1/models: %w", err)
	}
	var models ModelsResponse
	if err := json.Unmarshal(body2, &models); err != nil {
		return "", "", fmt.Errorf("parsing /v1/models: %w", err)
	}

	for _, entry := range models.Data {
		if entry.Meta.LlamaSwap.ModelType != "sd" {
			continue
		}
		proxy, ok := runningByModel[entry.ID]
		if !ok {
			continue
		}
		return entry.ID, proxy, nil
	}

	return "", "", fmt.Errorf("no running sd model found")
}

func newReverseProxy(target string) (*httputil.ReverseProxy, error) {
	u, err := url.Parse(target)
	if err != nil {
		return nil, err
	}
	return httputil.NewSingleHostReverseProxy(u), nil
}

func main() {
	listen     := flag.String("listen",     ":5900",                         "address to listen on (host:port)")
	upstream   := flag.String("upstream",   "http://127.0.0.1:9290",        "llama-swap base URL")
	configPath := flag.String("config",     "/ai/llama-swap/config.yaml",   "path to llama-swap config.yaml")
	flag.Parse()

	llamaSwapProxy, err := newReverseProxy(*upstream)
	if err != nil {
		log.Fatalf("failed to create llama-swap proxy: %v", err)
	}

	http.HandleFunc("/opencode", func(w http.ResponseWriter, r *http.Request) {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		host := r.Host
		hostname := host
		if i := strings.LastIndex(host, ":"); i != -1 {
			hostname = host[:i]
		}
		baseURL := scheme + "://" + host + "/v1"

		// Read and parse llama-swap config.yaml
		data, err := os.ReadFile(*configPath)
		if err != nil {
			log.Printf("opencode: read config: %v", err)
			http.Error(w, fmt.Sprintf("cannot read config: %v", err), http.StatusInternalServerError)
			return
		}
		var lsCfg LlamaSwapConfigFile
		if err := yaml.Unmarshal(data, &lsCfg); err != nil {
			log.Printf("opencode: parse config: %v", err)
			http.Error(w, fmt.Sprintf("cannot parse config: %v", err), http.StatusInternalServerError)
			return
		}

		// Get running models so we can query their live props
		runningProxies := map[string]string{}
		if resp, err := http.Get(*upstream + "/running"); err == nil {
			defer resp.Body.Close()
			if body, err := io.ReadAll(resp.Body); err == nil {
				var running RunningResponse
				if json.Unmarshal(body, &running) == nil {
					for _, m := range running.Running {
						runningProxies[m.Model] = m.Proxy
					}
				}
			}
		}

		ocModels := make(map[string]OpenCodeModel)
		for name, def := range lsCfg.Models {
			mt, _ := def.Metadata["model_type"].(string)
			if mt == "sd" {
				continue
			}

			cmd := expandMacros(def.Cmd, lsCfg.Macros)

			// Context priority: live n_ctx from running model > -c flag > GGUF native max
			ctx := parseContext(cmd)
			if proxyURL, ok := runningProxies[name]; ok {
				if live := queryProps(proxyURL); live > 0 {
					ctx = live
				}
			}

			// Read GGUF metadata for capabilities (tool_call, reasoning, native context)
			var gguf *GGUFMeta
			if modelPath := parseModelPath(cmd); modelPath != "" {
				if meta, err := readGGUFMeta(modelPath); err != nil {
					log.Printf("opencode: gguf %s: %v", name, err)
				} else {
					gguf = meta
					if ctx == 0 && gguf.ContextLength > 0 {
						ctx = int(gguf.ContextLength)
					}
				}
			}

			m := OpenCodeModel{Name: name}
			if ctx > 0 {
				m.Limit = &OpenCodeLimit{Context: ctx, Output: ctx}
			}

			// Capabilities from GGUF chat template (authoritative source)
			if gguf != nil {
				m.ToolCall = GGUFHasToolCall(gguf.ChatTemplate)
				m.Reasoning = GGUFHasReasoning(gguf.ChatTemplate)
			}

			// Vision: --mmproj flag is the reliable indicator for multimodal models
			vision := strings.Contains(cmd, "--mmproj")

			// metadata.capabilities can override any GGUF-derived value
			if caps, ok := def.Metadata["capabilities"].(map[string]interface{}); ok {
				if v, ok := caps["vision"].(bool); ok && v {
					vision = true
				}
				if v, ok := caps["tool_call"].(bool); ok {
					m.ToolCall = v
				}
				if v, ok := caps["reasoning"].(bool); ok {
					m.Reasoning = v
				}
			}

			if vision {
				m.Modalities = &OpenCodeModalities{
					Input:  []string{"text", "image"},
					Output: []string{"text"},
				}
				m.Attachment = true
			}

			ocModels[name] = m
		}

		cfg := OpenCodeConfig{
			Schema: "https://opencode.ai/config.json",
			Provider: map[string]OpenCodeProvider{
				hostname: {
					NPM:     "@ai-sdk/openai-compatible",
					Name:    hostname,
					Options: OpenCodeProviderOptions{BaseURL: baseURL},
					Models:  ocModels,
				},
			},
		}

		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(cfg); err != nil {
			log.Printf("opencode: encode error: %v", err)
		}
	})

	http.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		resp, err := http.Get(*upstream + "/v1/models")
		if err != nil {
			log.Printf("models: upstream error: %v", err)
			http.Error(w, fmt.Sprintf("upstream error: %v", err), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			log.Printf("models: read error: %v", err)
			http.Error(w, "failed to read upstream response", http.StatusBadGateway)
			return
		}
		var models ModelsResponse
		if err := json.Unmarshal(body, &models); err != nil {
			log.Printf("models: parse error: %v", err)
			http.Error(w, "failed to parse upstream response", http.StatusBadGateway)
			return
		}

		out := ProxyModelsResponse{Object: "list", Data: []ProxyModelEntry{}}
		for _, entry := range models.Data {
			if entry.Meta.LlamaSwap.ModelType == "sd" {
				continue
			}
			pe := ProxyModelEntry{
				ID:      entry.ID,
				Object:  "model",
				OwnedBy: "llamaswap",
			}
			if entry.Meta.LlamaSwap.Context > 0 {
				pe.ContextLength = entry.Meta.LlamaSwap.Context
				if entry.Meta.LlamaSwap.MaxContext > 0 {
					pe.MaxContextLength = entry.Meta.LlamaSwap.MaxContext
				} else {
					pe.MaxContextLength = entry.Meta.LlamaSwap.Context
				}
			}
			pe.Capabilities = entry.Meta.LlamaSwap.Capabilities
			out.Data = append(out.Data, pe)
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(out); err != nil {
			log.Printf("models: encode error: %v", err)
		}
	})

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/sdcpp") {
			llamaSwapProxy.ServeHTTP(w, r)
			return
		}

		modelID, proxyTarget, err := findRunningSDProxy(*upstream)
		if err != nil {
			log.Printf("sdcpp proxy lookup failed: %v", err)
			http.Error(w, fmt.Sprintf("no running sd model: %v", err), http.StatusServiceUnavailable)
			return
		}

		sdProxy, err := newReverseProxy(proxyTarget)
		if err != nil {
			log.Printf("failed to build sd proxy for %s: %v", proxyTarget, err)
			http.Error(w, "proxy error", http.StatusInternalServerError)
			return
		}

		log.Printf("sdcpp resolved model=%s -> %s%s", modelID, proxyTarget, r.URL.Path)
		sdProxy.ServeHTTP(w, r)
	})

	log.Printf("llama-swap-proxy v%s listening on %s", version, *listen)
	log.Printf("  default upstream: %s", *upstream)
	log.Printf("  config: %s", *configPath)
	log.Printf("  /sdcpp/* -> dynamically resolved sd model upstream")
	if err := http.ListenAndServe(*listen, nil); err != nil {
		log.Fatalf("listen: %v", err)
	}
}
