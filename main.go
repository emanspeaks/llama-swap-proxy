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
	"path/filepath"
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
	Object string            `json:"object"`
	Data   []ProxyModelEntry `json:"data"`
}

// llama-swap config.yaml structures
type LlamaSwapConfigFile struct {
	StartPort int                          `yaml:"startPort"`
	Macros    map[string]interface{}       `yaml:"macros"`
	Models    map[string]LlamaSwapModelDef `yaml:"models"`
}

type LlamaSwapModelDef struct {
	Cmd           string                 `yaml:"cmd"`
	Proxy         string                 `yaml:"proxy"`
	CheckEndpoint string                 `yaml:"checkEndpoint"`
	Macros        map[string]interface{} `yaml:"macros"`
	Aliases       []string               `yaml:"aliases"`
	Metadata      map[string]interface{} `yaml:"metadata"`
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
	Input   int `json:"input,omitempty"`
	Output  int `json:"output"`
}

type OpenCodeModalities struct {
	Input  []string `json:"input"`
	Output []string `json:"output"`
}

var reFlagContext = regexp.MustCompile(`(?:^|\s)(?:-c|--ctx-size)(?:\s+|=)(\d+)(?:\s|$)`)
var reFlagModelPath = regexp.MustCompile(`(?:^|\s)(?:-m|--model)(?:\s+|=)(\S+)(?:\s|$)`)
var reFlagChatTemplate = regexp.MustCompile(`(?:^|\s)--chat-template(?:\s+|=)`) // explicit template override
var reFlagChatTemplateFile = regexp.MustCompile(`(?:^|\s)--chat-template-file(?:\s+|=)`)
var reFlagSkipChatParsing = regexp.MustCompile(`(?:^|\s)--skip-chat-parsing(?:\s|$)`)
var reFlagNoJinja = regexp.MustCompile(`(?:^|\s)--no-jinja(?:\s|$)`)
var reFlagReasoningOff = regexp.MustCompile(`(?:^|\s)(?:-rea|--reasoning)(?:\s+|=)off(?:\s|$)`)
var reFlagReasoningOn = regexp.MustCompile(`(?:^|\s)(?:-rea|--reasoning)(?:\s+|=)on(?:\s|$)`)
var reFlagReasoningFormatNone = regexp.MustCompile(`(?:^|\s)--reasoning-format(?:\s+|=)none(?:\s|$)`)
var reEnvMacro = regexp.MustCompile(`\$\{env\.([A-Za-z_][A-Za-z0-9_]*)\}`)

func expandEnvMacros(s string) (string, error) {
	missing := ""
	out := reEnvMacro.ReplaceAllStringFunc(s, func(token string) string {
		m := reEnvMacro.FindStringSubmatch(token)
		if len(m) < 2 {
			return token
		}
		if value, ok := os.LookupEnv(m[1]); ok {
			return value
		}
		if missing == "" {
			missing = m[1]
		}
		return token
	})
	if missing != "" {
		return "", fmt.Errorf("missing env macro: %s", missing)
	}
	return out, nil
}

func expandMacros(s string, macros map[string]string) (string, error) {
	for i := 0; i < 8; i++ {
		expanded, err := expandEnvMacros(s)
		if err != nil {
			return "", err
		}
		s = expanded

		prev := s
		for k, v := range macros {
			s = strings.ReplaceAll(s, "${"+k+"}", v)
		}
		if s == prev {
			break
		}
	}
	return s, nil
}

func macroMapToStrings(raw map[string]interface{}) (map[string]string, error) {
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		switch t := v.(type) {
		case string:
			out[k] = t
		case int:
			out[k] = strconv.Itoa(t)
		case int64:
			out[k] = strconv.FormatInt(t, 10)
		case float64:
			out[k] = strconv.FormatFloat(t, 'f', -1, 64)
		case bool:
			if t {
				out[k] = "true"
			} else {
				out[k] = "false"
			}
		default:
			return nil, fmt.Errorf("macro %q has unsupported type %T", k, v)
		}
	}
	return out, nil
}

func stripCommentOnlyLines(s string) string {
	lines := strings.Split(s, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

func parseContext(cmd string) int {
	m := reFlagContext.FindStringSubmatch(cmd)
	if len(m) < 2 {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

func parseModelPath(cmd string) string {
	m := reFlagModelPath.FindStringSubmatch(cmd)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

func cmdHasFlag(cmd string, re *regexp.Regexp) bool {
	return re.FindStringIndex(cmd) != nil
}

func parseListFlag(raw string) map[string]struct{} {
	values := make(map[string]struct{})
	for _, part := range strings.Split(raw, ",") {
		value := strings.ToLower(strings.TrimSpace(part))
		if value == "" {
			continue
		}
		values[value] = struct{}{}
	}
	return values
}

func shouldIncludeOpenCodeModel(modelType string, includeTypes, excludeTypes map[string]struct{}) bool {
	modelType = strings.ToLower(strings.TrimSpace(modelType))
	_, included := includeTypes[modelType]
	_, excluded := excludeTypes[modelType]

	switch {
	case len(includeTypes) == 0 && len(excludeTypes) == 0:
		return true
	case len(includeTypes) > 0 && len(excludeTypes) == 0:
		return included
	case len(includeTypes) == 0 && len(excludeTypes) > 0:
		return !excluded
	default:
		if excluded {
			return false
		}
		if included {
			return true
		}
		return true
	}
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

func newInjectingUpstreamProxy(target string, cfg InjectionConfig) (*httputil.ReverseProxy, error) {
	p, err := newReverseProxy(target)
	if err != nil {
		return nil, err
	}
	originalDirector := p.Director
	p.Director = func(req *http.Request) {
		originalDirector(req)
		// Avoid compressed HTML payloads so middleware can inject bootstrap script safely.
		req.Header.Del("Accept-Encoding")
	}
	p.ModifyResponse = func(resp *http.Response) error {
		return injectWebUISync(resp, cfg)
	}
	return p, nil
}

func main() {
	listen := flag.String("listen", ":5900", "address to listen on (host:port)")
	upstream := flag.String("upstream", "http://127.0.0.1:9290", "llama-swap base URL")
	configPath := flag.String("config", "/ai/llama-swap/config.yaml", "path to llama-swap config.yaml")
	sessionsDir := flag.String("sessions-dir", "/ai/sessions", "directory used for centralized session storage")
	defaultUser := flag.String("default-user", "user", "default username used when auth is not configured")
	isolateModelUserStates := flag.Bool("isolate-model-user-states", false, "when true, isolate synchronized state per /upstream/<model>/ namespace")
	opencodeHostname := flag.String("opencode-hostname", "", "custom host (and optional port) for /opencode endpoint responses, e.g. myserver.local:5900 (overrides request Host header)")
	opencodeIncludeModelType := flag.String("opencode-include-model-type", "", "comma-separated metadata.model_type values to include in /opencode responses")
	opencodeExcludeModelType := flag.String("opencode-exclude-model-type", "", "comma-separated metadata.model_type values to exclude from /opencode responses")
	flag.Parse()

	includeModelTypes := parseListFlag(*opencodeIncludeModelType)
	excludeModelTypes := parseListFlag(*opencodeExcludeModelType)

	llamaSwapProxy, err := newReverseProxy(*upstream)
	if err != nil {
		log.Fatalf("failed to create llama-swap proxy: %v", err)
	}

	injectingUpstreamProxy, err := newInjectingUpstreamProxy(*upstream, InjectionConfig{
		DefaultUser:           *defaultUser,
		IsolateModelUserState: *isolateModelUserStates,
	})
	if err != nil {
		log.Fatalf("failed to create injecting upstream proxy: %v", err)
	}

	sessionsDBPath := filepath.Join(*sessionsDir, "sessions.db")
	sessionStore, err := NewSessionStore(sessionsDBPath)
	if err != nil {
		log.Fatalf("failed to initialize session store: %v", err)
	}
	defer func() {
		if err := sessionStore.Close(); err != nil {
			log.Printf("session store close failed: %v", err)
		}
	}()

	syncServer := NewSyncServer(sessionStore, *defaultUser, *isolateModelUserStates)

	opencodeHandler := func(w http.ResponseWriter, r *http.Request) {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		host := r.Host
		hostname := host
		if *opencodeHostname != "" {
			hostname = *opencodeHostname
		}
		if i := strings.LastIndex(hostname, ":"); i != -1 {
			hostname = hostname[:i]
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
		globalMacros, err := macroMapToStrings(lsCfg.Macros)
		if err != nil {
			log.Printf("opencode: parse global macros: %v", err)
			http.Error(w, fmt.Sprintf("invalid global macros: %v", err), http.StatusInternalServerError)
			return
		}

		ocModels := make(map[string]OpenCodeModel)
		for name, def := range lsCfg.Models {
			mt, _ := def.Metadata["model_type"].(string)
			if !shouldIncludeOpenCodeModel(mt, includeModelTypes, excludeModelTypes) {
				continue
			}

			modelMacros, err := macroMapToStrings(def.Macros)
			if err != nil {
				log.Printf("opencode: parse model macros %s: %v", name, err)
				http.Error(w, fmt.Sprintf("invalid model macros for %s: %v", name, err), http.StatusInternalServerError)
				return
			}

			macros := make(map[string]string, len(globalMacros)+len(modelMacros)+1)
			for k, v := range globalMacros {
				macros[k] = v
			}
			for k, v := range modelMacros {
				macros[k] = v
			}
			macros["MODEL_ID"] = name

			cmd, err := expandMacros(def.Cmd, macros)
			if err != nil {
				log.Printf("opencode: expand macros %s: %v", name, err)
				http.Error(w, fmt.Sprintf("cannot expand macros for %s: %v", name, err), http.StatusInternalServerError)
				return
			}
			cmd = stripCommentOnlyLines(cmd)

			// Skip embedding-only servers (no chat completions endpoint)
			if strings.Contains(cmd, "--embedding") {
				continue
			}

			// Context from configured command. Do not use live /props here.
			configuredCtx := parseContext(cmd)
			maxCtx := 0

			// Read GGUF metadata for capabilities and architecture max context.
			var gguf *GGUFMeta
			if modelPath := parseModelPath(cmd); modelPath != "" {
				if meta, err := readGGUFMeta(modelPath); err != nil {
					log.Printf("opencode: gguf %s: %v", name, err)
				} else {
					gguf = meta
					if gguf.ContextLength > 0 {
						maxCtx = int(gguf.ContextLength)
					}
				}
			}

			m := OpenCodeModel{Name: name}
			if configuredCtx > 0 || maxCtx > 0 {
				contextLimit := configuredCtx
				if contextLimit == 0 {
					contextLimit = maxCtx
				}
				m.Limit = &OpenCodeLimit{Context: contextLimit, Output: contextLimit}
				if maxCtx > 0 {
					m.Limit.Input = maxCtx
				}
			}

			templateOverridden := cmdHasFlag(cmd, reFlagChatTemplate) || cmdHasFlag(cmd, reFlagChatTemplateFile)
			// Capabilities from GGUF chat template when command does not replace the template.
			if gguf != nil && !templateOverridden {
				m.ToolCall = GGUFHasToolCall(gguf.ChatTemplate)
				m.Reasoning = GGUFHasReasoning(gguf.ChatTemplate)
			}

			// Command-line flags can disable reasoning/tool parsing even if GGUF supports it.
			if cmdHasFlag(cmd, reFlagSkipChatParsing) || cmdHasFlag(cmd, reFlagNoJinja) {
				m.ToolCall = false
				m.Reasoning = false
			}
			if cmdHasFlag(cmd, reFlagReasoningOff) || cmdHasFlag(cmd, reFlagReasoningFormatNone) {
				m.Reasoning = false
			}
			if cmdHasFlag(cmd, reFlagReasoningOn) {
				m.Reasoning = true
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
			for _, alias := range def.Aliases {
				aliasModel := m
				aliasModel.Name = alias
				ocModels[alias] = aliasModel
			}
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
	}

	http.HandleFunc("/opencode", opencodeHandler)
	http.HandleFunc("/v1/opencode", opencodeHandler)
	http.HandleFunc("/api/sessions/", syncServer.HandleSessions)

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
		if strings.HasPrefix(r.URL.Path, "/upstream/") {
			injectingUpstreamProxy.ServeHTTP(w, r)
			return
		}

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
	log.Printf("  sessions dir: %s", *sessionsDir)
	log.Printf("  default user: %s", *defaultUser)
	log.Printf("  isolate model user states: %t", *isolateModelUserStates)
	log.Printf("  /sdcpp/* -> dynamically resolved sd model upstream")
	log.Printf("  /upstream/* HTML -> llama.cpp webui sync bootstrap injection")
	if err := http.ListenAndServe(*listen, nil); err != nil {
		log.Fatalf("listen: %v", err)
	}
}
