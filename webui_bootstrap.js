(() => {
  if (window.__llamaSwapSyncBootstrapped) {
    return;
  }
  window.__llamaSwapSyncBootstrapped = true;

  const user = "__LLAMA_SYNC_USER_ESCAPED__";
  const scope = "__LLAMA_SYNC_SCOPE_ESCAPED__";
  const clientId = 'sync-' + Math.random().toString(36).slice(2);
  const apiBase = '/api/sessions/' + encodeURIComponent(user);
  let suppressPush = false;
  let pushTimer = null;
  let ws = null;
  let wsReady = false;
  let reconnectTimer = null;
  let lastSyncSignature = '';
  let hasAppliedInitialSnapshot = false;

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

  function signatureOf(localStorageData, indexedDBData) {
    try {
      return JSON.stringify(localStorageData || {}) + '|' + JSON.stringify(indexedDBData || {});
    } catch {
      return '';
    }
  }

  async function pullSnapshotHTTP() {
    const url = apiBase + '/snapshot?scope=' + encodeURIComponent(scope);
    const resp = await fetch(url, { credentials: 'same-origin' });
    if (!resp.ok) {
      throw new Error('snapshot request failed: ' + resp.status);
    }
    return resp.json();
  }

  async function pushSnapshotHTTP(payload) {
    const url = apiBase + '/sync?scope=' + encodeURIComponent(scope);
    const resp = await fetch(url, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      credentials: 'same-origin',
      body: JSON.stringify(payload)
    });
    if (!resp.ok) {
      throw new Error('sync request failed: ' + resp.status);
    }
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

    const signature = signatureOf(payload.localStorage, payload.indexedDB);
    if (signature !== '' && signature === lastSyncSignature) {
      return;
    }

    try {
      if (ws && ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({
          type: 'sync',
          clientId,
          payload
        }));
      } else {
        await pushSnapshotHTTP(payload);
      }
      lastSyncSignature = signature;
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
    if (typeof WebSocket !== 'function') {
      return Promise.resolve();
    }

    const wsScheme = location.protocol === 'https:' ? 'wss' : 'ws';
    const wsUrl = wsScheme + '://' + location.host + apiBase + '/ws?scope=' + encodeURIComponent(scope) + '&clientId=' + encodeURIComponent(clientId);

    return new Promise((resolve) => {
      let resolved = false;
      const resolveOnce = () => {
        if (!resolved) {
          resolved = true;
          resolve();
        }
      };

      const connect = () => {
        try {
          ws = new WebSocket(wsUrl);
        } catch (err) {
          logError('ws connect failed', err);
          reconnectTimer = setTimeout(connect, 2000);
          resolveOnce();
          return;
        }

        ws.onopen = () => {
          wsReady = true;
          resolveOnce();
          try {
            ws.send(JSON.stringify({ type: 'get-snapshot', clientId }));
          } catch (err) {
            logError('ws get-snapshot failed', err);
          }
        };

        ws.onmessage = async (event) => {
          let payload = null;
          try {
            payload = JSON.parse(event.data);
          } catch {
            return;
          }
          if (!payload) {
            return;
          }

          if (payload.type === 'snapshot' && payload.snapshot) {
            try {
              suppressPush = true;
              applyLocalStorage(payload.snapshot.localStorage || {});
              await applyIndexedDB(payload.snapshot.indexedDB || {});
              hasAppliedInitialSnapshot = true;
              lastSyncSignature = signatureOf(
                payload.snapshot.localStorage || {},
                payload.snapshot.indexedDB || {}
              );
              resolveOnce();
            } catch (err) {
              logError('ws apply snapshot failed', err);
            } finally {
              suppressPush = false;
            }
            return;
          }

          if (payload.type === 'error') {
            logError('ws sync server error', payload.error || 'unknown error');
            return;
          }
        };

        ws.onclose = () => {
          wsReady = false;
          reconnectTimer = setTimeout(connect, 2000);
          resolveOnce();
        };

        ws.onerror = () => {
          try { ws.close(); } catch {}
        };
      };

      connect();
    });
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
      installWriteHooks();
      await Promise.race([
        startWebSocket(),
        new Promise((resolve) => setTimeout(resolve, 1500))
      ]);

      // iOS/Safari and some local-network setups may fail or delay WS; ensure
      // startup always gets a snapshot via HTTP fallback.
      if (!hasAppliedInitialSnapshot) {
        try {
          const snapshot = await pullSnapshotHTTP();
          suppressPush = true;
          applyLocalStorage(snapshot.localStorage || {});
          await applyIndexedDB(snapshot.indexedDB || {});
          hasAppliedInitialSnapshot = true;
          lastSyncSignature = signatureOf(snapshot.localStorage || {}, snapshot.indexedDB || {});
        } catch (err) {
          logError('initial HTTP snapshot fallback failed', err);
        } finally {
          suppressPush = false;
        }
      }

      setInterval(() => queuePush(0), 30000);
      queuePush(500);
    } catch (err) {
      suppressPush = false;
      logError('initial sync failed', err);
    } finally {
      await runDeferredScripts();
    }
  })();
})();
