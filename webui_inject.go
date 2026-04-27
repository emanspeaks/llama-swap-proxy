package main

import (
	"bytes"
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
}

var reScriptTagStart = regexp.MustCompile(`(?is)<script\b[^>]*>`)
var reTypeAttr = regexp.MustCompile(`(?is)\btype\s*=\s*("[^"]*"|'[^']*'|[^\s>]+)`)

func injectWebUISync(resp *http.Response, cfg InjectionConfig) error {
	if resp == nil || resp.Request == nil {
		return nil
	}
	if !strings.HasPrefix(resp.Request.URL.Path, "/upstream/") {
		return nil
	}
	if !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/html") {
		return nil
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()

	htmlBody := string(body)
	if !isLikelyLlamaCppWebUI(htmlBody) {
		resp.Body = io.NopCloser(bytes.NewReader(body))
		resp.ContentLength = int64(len(body))
		resp.Header.Set("Content-Length", fmt.Sprintf("%d", len(body)))
		return nil
	}

	modelID := modelIDFromUpstreamPath(resp.Request.URL.Path)
	scope := "global"
	if cfg.IsolateModelUserState && modelID != "" {
		scope = "model:" + modelID
	}

	rewritten := rewriteScriptTagsForSyncGate(htmlBody)
	injected, changed := injectBootstrapScript(rewritten, cfg.DefaultUser, scope)
	if !changed {
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
	resp.Header.Del("Content-Encoding")
	return nil
}

func modelIDFromUpstreamPath(path string) string {
	trimmed := strings.TrimPrefix(path, "/upstream/")
	if trimmed == "" || trimmed == path {
		return ""
	}
	parts := strings.SplitN(trimmed, "/", 2)
	return strings.TrimSpace(parts[0])
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
	return false
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
	userJS := jsQuoted(user)
	scopeJS := jsQuoted(scope)
	return `
<script data-llama-sync-bootstrap="1">
(() => {
  if (window.__llamaSwapSyncBootstrapped) {
    return;
  }
  window.__llamaSwapSyncBootstrapped = true;

  const user = ` + userJS + `;
  const scope = ` + scopeJS + `;
  const clientId = 'sync-' + Math.random().toString(36).slice(2);
  const apiBase = '/api/sessions/' + encodeURIComponent(user);
  let suppressPush = false;
  let pushTimer = null;

  function logError(...args) {
    try { console.warn('[llama-swap-sync]', ...args); } catch {}
  }

  function dumpLocalStorage() {
    const out = {};
    try {
      for (let i = 0; i < localStorage.length; i += 1) {
        const key = localStorage.key(i);
        if (key === null) continue;
        out[key] = localStorage.getItem(key);
      }
    } catch (err) {
      logError('localStorage dump failed', err);
    }
    return out;
  }

  function applyLocalStorage(entries) {
    if (!entries || typeof entries !== 'object') return;
    for (const [key, value] of Object.entries(entries)) {
      try {
        if (typeof value === 'string') {
          localStorage.setItem(key, value);
        }
      } catch (err) {
        logError('localStorage apply failed', key, err);
      }
    }
  }

  function idbOpen(name, version, stores) {
    return new Promise((resolve, reject) => {
      const req = indexedDB.open(name, Math.max(1, Number(version) || 1));
      req.onupgradeneeded = () => {
        const db = req.result;
        if (!stores || typeof stores !== 'object') {
          return;
        }
        for (const [storeName, storeDef] of Object.entries(stores)) {
          if (db.objectStoreNames.contains(storeName)) {
            continue;
          }
          const options = {};
          if (storeDef && Object.prototype.hasOwnProperty.call(storeDef, 'keyPath')) {
            options.keyPath = storeDef.keyPath;
          }
          if (storeDef && storeDef.autoIncrement) {
            options.autoIncrement = true;
          }
          try {
            db.createObjectStore(storeName, options);
          } catch (err) {
            logError('createObjectStore failed', storeName, err);
          }
        }
      };
      req.onsuccess = () => resolve(req.result);
      req.onerror = () => reject(req.error || new Error('indexedDB.open failed'));
    });
  }

  function txDone(tx) {
    return new Promise((resolve, reject) => {
      tx.oncomplete = () => resolve();
      tx.onerror = () => reject(tx.error || new Error('transaction failed'));
      tx.onabort = () => reject(tx.error || new Error('transaction aborted'));
    });
  }

  async function dumpIndexedDB() {
    if (!indexedDB || typeof indexedDB.databases !== 'function') {
      return {};
    }
    const result = {};
    let dbs = [];
    try {
      dbs = await indexedDB.databases();
    } catch (err) {
      logError('indexedDB.databases failed', err);
      return {};
    }

    for (const dbInfo of dbs) {
      if (!dbInfo || !dbInfo.name) continue;
      const dbName = dbInfo.name;
      try {
        const db = await idbOpen(dbName, dbInfo.version || 1);
        const stores = {};

        for (const storeName of Array.from(db.objectStoreNames)) {
          const tx = db.transaction(storeName, 'readonly');
          const store = tx.objectStore(storeName);
          const records = [];

          await new Promise((resolve, reject) => {
            const req = store.openCursor();
            req.onsuccess = () => {
              const cursor = req.result;
              if (!cursor) {
                resolve();
                return;
              }
              records.push({ key: cursor.key, value: cursor.value });
              cursor.continue();
            };
            req.onerror = () => reject(req.error || new Error('cursor read failed'));
          });

          stores[storeName] = {
            keyPath: store.keyPath,
            autoIncrement: store.autoIncrement,
            records
          };
          await txDone(tx).catch(() => {});
        }

        result[dbName] = { version: db.version, stores };
        db.close();
      } catch (err) {
        logError('dump db failed', dbName, err);
      }
    }

    return result;
  }

  async function applyIndexedDB(snapshot) {
    if (!snapshot || typeof snapshot !== 'object' || !indexedDB) {
      return;
    }

    for (const [dbName, dbDef] of Object.entries(snapshot)) {
      if (!dbDef || typeof dbDef !== 'object') continue;
      const stores = dbDef.stores || {};
      try {
        const db = await idbOpen(dbName, dbDef.version || 1, stores);

        for (const [storeName, storeDef] of Object.entries(stores)) {
          if (!db.objectStoreNames.contains(storeName)) continue;
          const tx = db.transaction(storeName, 'readwrite');
          const store = tx.objectStore(storeName);
          try {
            store.clear();
          } catch (err) {
            logError('clear store failed', dbName, storeName, err);
          }

          const records = Array.isArray(storeDef.records) ? storeDef.records : [];
          for (const rec of records) {
            if (!rec || !Object.prototype.hasOwnProperty.call(rec, 'value')) continue;
            try {
              if (Object.prototype.hasOwnProperty.call(rec, 'key')) {
                store.put(rec.value, rec.key);
              } else {
                store.put(rec.value);
              }
            } catch (err) {
              try {
                store.put(rec.value);
              } catch {
                logError('store put failed', dbName, storeName, err);
              }
            }
          }

          await txDone(tx).catch((err) => logError('apply tx failed', dbName, storeName, err));
        }

        db.close();
      } catch (err) {
        logError('apply db failed', dbName, err);
      }
    }
  }

  async function pullSnapshot() {
    const url = apiBase + '/snapshot?scope=' + encodeURIComponent(scope);
    const resp = await fetch(url, { credentials: 'same-origin' });
    if (!resp.ok) {
      throw new Error('snapshot request failed: ' + resp.status);
    }
    return resp.json();
  }

  async function pushSnapshot() {
    if (suppressPush) {
      return;
    }

    const payload = {
      clientId,
      localStorage: dumpLocalStorage(),
      indexedDB: await dumpIndexedDB()
    };

    const url = apiBase + '/sync?scope=' + encodeURIComponent(scope);
    try {
      await fetch(url, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        credentials: 'same-origin',
        body: JSON.stringify(payload)
      });
    } catch (err) {
      logError('push snapshot failed', err);
    }
  }

  function queuePush(delayMs) {
    if (suppressPush) {
      return;
    }
    if (pushTimer) {
      clearTimeout(pushTimer);
    }
    pushTimer = setTimeout(() => {
      pushTimer = null;
      void pushSnapshot();
    }, delayMs);
  }

  function installWriteHooks() {
    try {
      const nativeSetItem = localStorage.setItem.bind(localStorage);
      const nativeRemoveItem = localStorage.removeItem.bind(localStorage);
      const nativeClear = localStorage.clear.bind(localStorage);

      localStorage.setItem = function(key, value) {
        nativeSetItem(key, value);
        queuePush(150);
      };
      localStorage.removeItem = function(key) {
        nativeRemoveItem(key);
        queuePush(150);
      };
      localStorage.clear = function() {
        nativeClear();
        queuePush(150);
      };
    } catch (err) {
      logError('failed to patch localStorage', err);
    }

    const proto = window.IDBObjectStore && window.IDBObjectStore.prototype;
    if (!proto) {
      return;
    }

    for (const methodName of ['add', 'put', 'delete', 'clear']) {
      const original = proto[methodName];
      if (typeof original !== 'function') continue;
      proto[methodName] = function(...args) {
        const ret = original.apply(this, args);
        queuePush(300);
        return ret;
      };
    }
  }

  function startWebSocket() {
    const wsScheme = location.protocol === 'https:' ? 'wss' : 'ws';
    const wsUrl = wsScheme + '://' + location.host + apiBase + '/ws?scope=' + encodeURIComponent(scope);

    let ws = null;
    let reconnectTimer = null;

    const connect = () => {
      try {
        ws = new WebSocket(wsUrl);
      } catch (err) {
        logError('ws connect failed', err);
        reconnectTimer = setTimeout(connect, 2000);
        return;
      }

      ws.onmessage = async (event) => {
        let payload = null;
        try {
          payload = JSON.parse(event.data);
        } catch {
          return;
        }
        if (!payload || payload.type !== 'updated') {
          return;
        }

        try {
          const snapshot = await pullSnapshot();
          suppressPush = true;
          applyLocalStorage(snapshot.localStorage || {});
          await applyIndexedDB(snapshot.indexedDB || {});
          suppressPush = false;
        } catch (err) {
          suppressPush = false;
          logError('ws pull/apply failed', err);
        }
      };

      ws.onclose = () => {
        reconnectTimer = setTimeout(connect, 2000);
      };

      ws.onerror = () => {
        try { ws.close(); } catch {}
      };
    };

    connect();
  }

  async function runDeferredScripts() {
    const deferred = Array.from(document.querySelectorAll('script[data-llama-sync-deferred="1"]'));
    for (const original of deferred) {
      const script = document.createElement('script');
      for (const attr of Array.from(original.attributes)) {
        if (attr.name === 'data-llama-sync-deferred' || attr.name === 'data-llama-sync-type') {
          continue;
        }
        if (attr.name === 'type') {
          continue;
        }
        script.setAttribute(attr.name, attr.value);
      }

      const restoredType = original.getAttribute('data-llama-sync-type');
      if (restoredType) {
        script.type = restoredType;
      }

      if (original.src) {
        script.src = original.src;
      } else {
        script.textContent = original.textContent;
      }

      const done = new Promise((resolve) => {
        script.onload = () => resolve();
        script.onerror = () => resolve();
      });

      original.parentNode.replaceChild(script, original);
      if (script.src || script.type === 'module') {
        await done;
      }
    }
  }

  (async () => {
    try {
      const snapshot = await pullSnapshot();
      suppressPush = true;
      applyLocalStorage(snapshot.localStorage || {});
      await applyIndexedDB(snapshot.indexedDB || {});
      suppressPush = false;

      installWriteHooks();
      startWebSocket();
      setInterval(() => queuePush(0), 5000);
      queuePush(200);
    } catch (err) {
      suppressPush = false;
      logError('initial sync failed', err);
    } finally {
      await runDeferredScripts();
    }
  })();
})();
</script>
`
}

func jsQuoted(s string) string {
	replacer := strings.NewReplacer(
		`\\`, `\\\\`,
		`"`, `\\"`,
		"\n", `\\n`,
		"\r", `\\r`,
		"\t", `\\t`,
	)
	return `"` + replacer.Replace(s) + `"`
}
