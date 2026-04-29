package main

import (
	"bytes"
	_ "embed"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
)

type InjectionConfig struct {
	DefaultUser           string
	IsolateModelUserState bool
	BackendGuesser        *BackendGuesser
}

var reScriptTagStart = regexp.MustCompile(`(?is)<script\b[^>]*>`)
var reTypeAttr = regexp.MustCompile(`(?is)\btype\s*=\s*("[^"]*"|'[^']*'|[^\s>]+)`)

const bootstrapUserPlaceholder = "__LLAMA_SYNC_USER_ESCAPED__"
const bootstrapScopePlaceholder = "__LLAMA_SYNC_SCOPE_ESCAPED__"

//go:embed webui_bootstrap.js
var webUIBootstrapTemplate string

func injectWebUISync(resp *http.Response, cfg InjectionConfig) error {
	if resp == nil || resp.Request == nil {
		return nil
	}
	if !strings.HasPrefix(resp.Request.URL.Path, "/upstream/") {
		return nil
	}
	if !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/html") {
		resp.Header.Set("X-Llama-Sync-Injection", "skipped-not-html")
		return nil
	}

	modelID := modelIDFromUpstreamPath(resp.Request.URL.Path)
	if modelID != "" && cfg.BackendGuesser != nil {
		isLlamaCpp, resolved := cfg.BackendGuesser.IsLlamaCppModel(modelID)
		if resolved && !isLlamaCpp {
			resp.Header.Set("X-Llama-Sync-Injection", "skipped-not-llamacpp-backend")
			return nil
		}
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()

	htmlBody := string(body)
	if !isLikelyLlamaCppWebUI(htmlBody) {
		resp.Header.Set("X-Llama-Sync-Injection", "skipped-not-llamacpp-webui")
		resp.Body = io.NopCloser(bytes.NewReader(body))
		resp.ContentLength = int64(len(body))
		resp.Header.Set("Content-Length", fmt.Sprintf("%d", len(body)))
		return nil
	}

	scope := "global"
	if cfg.IsolateModelUserState && modelID != "" {
		scope = "model:" + modelID
	}

	rewritten := rewriteScriptTagsForSyncGate(htmlBody)
	injected, changed := injectBootstrapScript(rewritten, cfg.DefaultUser, scope)
	if !changed {
		resp.Header.Set("X-Llama-Sync-Injection", "skipped-no-head-tag")
		resp.Body = io.NopCloser(bytes.NewReader(body))
		resp.ContentLength = int64(len(body))
		resp.Header.Set("Content-Length", fmt.Sprintf("%d", len(body)))
		return nil
	}

	log.Printf("webui sync injection served path=%s model=%s scope=%s", resp.Request.URL.Path, modelID, scope)

	out := []byte(injected)
	resp.Body = io.NopCloser(bytes.NewReader(out))
	resp.ContentLength = int64(len(out))
	resp.Header.Set("Content-Length", fmt.Sprintf("%d", len(out)))
	resp.Header.Set("X-Llama-Sync-Injection", "applied")
	resp.Header.Del("Content-Encoding")
	return nil
}

func isLikelyLlamaCppWebUI(htmlBody string) bool {
	lower := strings.ToLower(htmlBody)
	if strings.Contains(lower, "sd.cpp") || strings.Contains(lower, "stable diffusion") {
		return false
	}
	if strings.Contains(lower, "llama.cpp") {
		return true
	}
	if strings.Contains(lower, "__sveltekit") && strings.Contains(lower, "completion.js") {
		return true
	}
	if strings.Contains(lower, "id=\"app\"") && strings.Contains(lower, "chat") {
		return true
	}

	// Some upstream llama.cpp webui builds are minimal and do not include
	// identifying strings above. For /upstream HTML documents, default to
	// inject unless this was identified as a known non-llama page.
	return true
}

func rewriteScriptTagsForSyncGate(htmlBody string) string {
	return reScriptTagStart.ReplaceAllStringFunc(htmlBody, func(tag string) string {
		if strings.Contains(tag, "data-llama-sync-bootstrap") || strings.Contains(tag, "data-llama-sync-deferred") {
			return tag
		}

		originalType := ""
		if m := reTypeAttr.FindStringSubmatch(tag); len(m) > 1 {
			originalType = strings.Trim(m[1], `"'`)
			tag = reTypeAttr.ReplaceAllString(tag, `type="application/llama-sync-deferred"`)
		} else {
			tag = strings.Replace(tag, ">", ` type="application/llama-sync-deferred">`, 1)
		}

		insertion := fmt.Sprintf(` data-llama-sync-deferred="1" data-llama-sync-type="%s"`, html.EscapeString(originalType))
		if strings.HasSuffix(tag, ">") {
			return strings.TrimSuffix(tag, ">") + insertion + ">"
		}
		return tag
	})
}

func injectBootstrapScript(htmlBody, user, scope string) (string, bool) {
	bootstrap := buildBootstrapScript(user, scope)
	needle := "</head>"
	idx := strings.Index(strings.ToLower(htmlBody), needle)
	if idx == -1 {
		return htmlBody, false
	}
	return htmlBody[:idx] + bootstrap + htmlBody[idx:], true
}

func buildBootstrapScript(user, scope string) string {
	userEscaped := jsEscaped(user)
	scopeEscaped := jsEscaped(scope)
	body := strings.ReplaceAll(webUIBootstrapTemplate, bootstrapUserPlaceholder, userEscaped)
	body = strings.ReplaceAll(body, bootstrapScopePlaceholder, scopeEscaped)

	return "\n<script data-llama-sync-bootstrap=\"1\">\n" + body + "\n</script>\n"
}

func jsQuoted(s string) string {
	return `"` + jsEscaped(s) + `"`
}

func jsEscaped(s string) string {
	replacer := strings.NewReplacer(
		`\\`, `\\\\`,
		`"`, `\\"`,
		"\n", `\\n`,
		"\r", `\\r`,
		"\t", `\\t`,
	)
	return replacer.Replace(s)
}
