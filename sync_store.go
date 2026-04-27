package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type SessionSnapshot struct {
	User         string            `json:"user"`
	Scope        string            `json:"scope"`
	LocalStorage map[string]string `json:"localStorage"`
	IndexedDB    json.RawMessage   `json:"indexedDB"`
	UpdatedAt    int64             `json:"updatedAt"`
}

type SessionSyncRequest struct {
	LocalStorage map[string]string `json:"localStorage"`
	IndexedDB    json.RawMessage   `json:"indexedDB"`
	ClientID     string            `json:"clientId,omitempty"`
}

type sessionStateRow struct {
	LocalStorageJSON string
	IndexedDBJSON    string
	UpdatedAt        int64
}

type SessionStore struct {
	db *sql.DB
}

type AttachmentUpload struct {
	Hash      string
	MimeType  string
	Data      []byte
	SourceKey string
}

type idbRecord struct {
	Key   any `json:"key"`
	Value any `json:"value"`
}

type idbStore struct {
	KeyPath       any         `json:"keyPath,omitempty"`
	AutoIncrement bool        `json:"autoIncrement,omitempty"`
	Records       []idbRecord `json:"records"`
}

type idbDatabase struct {
	Version int64               `json:"version"`
	Stores  map[string]idbStore `json:"stores"`
}

type idbSnapshot map[string]idbDatabase

func NewSessionStore(dbPath string) (*SessionStore, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, fmt.Errorf("create sessions dir: %w", err)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	if _, err := db.Exec(`PRAGMA journal_mode = WAL; PRAGMA foreign_keys = ON;`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("configure sqlite pragmas: %w", err)
	}

	schema := `
CREATE TABLE IF NOT EXISTS session_states (
  user TEXT NOT NULL,
  scope TEXT NOT NULL,
  local_storage_json TEXT NOT NULL,
  indexeddb_json TEXT NOT NULL,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY(user, scope)
);

CREATE TABLE IF NOT EXISTS attachments (
  user TEXT NOT NULL,
  scope TEXT NOT NULL,
  hash TEXT NOT NULL,
  mime_type TEXT NOT NULL,
  blob_data BLOB NOT NULL,
  source_key TEXT,
  created_at INTEGER NOT NULL,
  PRIMARY KEY(user, scope, hash)
);
`
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("initialize sqlite schema: %w", err)
	}

	return &SessionStore{db: db}, nil
}

func (s *SessionStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *SessionStore) GetSnapshot(user, scope string) (SessionSnapshot, error) {
	row, err := s.getStateRow(user, scope)
	if err != nil {
		return SessionSnapshot{}, err
	}

	if row == nil {
		return SessionSnapshot{
			User:         user,
			Scope:        scope,
			LocalStorage: map[string]string{},
			IndexedDB:    json.RawMessage(`{}`),
			UpdatedAt:    0,
		}, nil
	}

	localStorage := map[string]string{}
	if err := json.Unmarshal([]byte(row.LocalStorageJSON), &localStorage); err != nil {
		localStorage = map[string]string{}
	}

	indexedDB := row.IndexedDBJSON
	if strings.TrimSpace(indexedDB) == "" {
		indexedDB = "{}"
	}

	return SessionSnapshot{
		User:         user,
		Scope:        scope,
		LocalStorage: localStorage,
		IndexedDB:    json.RawMessage(indexedDB),
		UpdatedAt:    row.UpdatedAt,
	}, nil
}

func (s *SessionStore) MergeAndSave(user, scope string, req SessionSyncRequest) (SessionSnapshot, error) {
	now := time.Now().UnixMilli()

	existing, err := s.GetSnapshot(user, scope)
	if err != nil {
		return SessionSnapshot{}, err
	}

	incomingLocal := req.LocalStorage
	if incomingLocal == nil {
		incomingLocal = map[string]string{}
	}

	sanitizedIncomingIDB, uploads, err := sanitizeAndExtractIndexedDB(req.IndexedDB)
	if err != nil {
		return SessionSnapshot{}, fmt.Errorf("sanitize indexeddb payload: %w", err)
	}

	mergedLocal := mergeLocalStorage(existing.LocalStorage, incomingLocal)
	mergedIDB, err := mergeIndexedDB(existing.IndexedDB, sanitizedIncomingIDB)
	if err != nil {
		return SessionSnapshot{}, err
	}

	localJSON, err := json.Marshal(mergedLocal)
	if err != nil {
		return SessionSnapshot{}, fmt.Errorf("marshal localStorage: %w", err)
	}
	idbJSON := mergedIDB
	if len(idbJSON) == 0 {
		idbJSON = json.RawMessage(`{}`)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return SessionSnapshot{}, fmt.Errorf("begin sqlite transaction: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	if _, err := tx.Exec(`
INSERT INTO session_states(user, scope, local_storage_json, indexeddb_json, updated_at)
VALUES(?, ?, ?, ?, ?)
ON CONFLICT(user, scope)
DO UPDATE SET
  local_storage_json = excluded.local_storage_json,
  indexeddb_json = excluded.indexeddb_json,
  updated_at = excluded.updated_at
`, user, scope, string(localJSON), string(idbJSON), now); err != nil {
		return SessionSnapshot{}, fmt.Errorf("upsert session snapshot: %w", err)
	}

	for _, upload := range uploads {
		if len(upload.Data) == 0 || upload.Hash == "" {
			continue
		}
		if _, err := tx.Exec(`
INSERT OR IGNORE INTO attachments(user, scope, hash, mime_type, blob_data, source_key, created_at)
VALUES(?, ?, ?, ?, ?, ?, ?)
`, user, scope, upload.Hash, upload.MimeType, upload.Data, upload.SourceKey, now); err != nil {
			return SessionSnapshot{}, fmt.Errorf("store attachment %s: %w", upload.Hash, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return SessionSnapshot{}, fmt.Errorf("commit sqlite transaction: %w", err)
	}

	return SessionSnapshot{
		User:         user,
		Scope:        scope,
		LocalStorage: mergedLocal,
		IndexedDB:    idbJSON,
		UpdatedAt:    now,
	}, nil
}

func (s *SessionStore) getStateRow(user, scope string) (*sessionStateRow, error) {
	row := s.db.QueryRow(`
SELECT local_storage_json, indexeddb_json, updated_at
FROM session_states
WHERE user = ? AND scope = ?
`, user, scope)

	var out sessionStateRow
	if err := row.Scan(&out.LocalStorageJSON, &out.IndexedDBJSON, &out.UpdatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("query session snapshot: %w", err)
	}
	return &out, nil
}

func mergeLocalStorage(existing, incoming map[string]string) map[string]string {
	out := make(map[string]string, len(existing)+len(incoming))
	for k, v := range existing {
		out[k] = v
	}
	for k, v := range incoming {
		out[k] = v
	}
	return out
}

func mergeIndexedDB(existingRaw, incomingRaw json.RawMessage) (json.RawMessage, error) {
	existing := idbSnapshot{}
	incoming := idbSnapshot{}

	if len(existingRaw) > 0 && strings.TrimSpace(string(existingRaw)) != "" {
		if err := json.Unmarshal(existingRaw, &existing); err != nil {
			existing = idbSnapshot{}
		}
	}
	if len(incomingRaw) > 0 && strings.TrimSpace(string(incomingRaw)) != "" {
		if err := json.Unmarshal(incomingRaw, &incoming); err != nil {
			return nil, fmt.Errorf("parse incoming indexeddb snapshot: %w", err)
		}
	}

	for dbName, incomingDB := range incoming {
		existingDB, ok := existing[dbName]
		if !ok {
			existing[dbName] = incomingDB
			continue
		}

		if incomingDB.Version > existingDB.Version {
			existingDB.Version = incomingDB.Version
		}
		if existingDB.Stores == nil {
			existingDB.Stores = map[string]idbStore{}
		}

		for storeName, incomingStore := range incomingDB.Stores {
			existingStore, ok := existingDB.Stores[storeName]
			if !ok {
				existingDB.Stores[storeName] = incomingStore
				continue
			}

			if existingStore.KeyPath == nil {
				existingStore.KeyPath = incomingStore.KeyPath
			}
			existingStore.AutoIncrement = existingStore.AutoIncrement || incomingStore.AutoIncrement
			existingStore.Records = mergeIDBRecords(existingStore.Records, incomingStore.Records)
			existingDB.Stores[storeName] = existingStore
		}

		existing[dbName] = existingDB
	}

	buf, err := json.Marshal(existing)
	if err != nil {
		return nil, fmt.Errorf("marshal merged indexeddb snapshot: %w", err)
	}
	return json.RawMessage(buf), nil
}

func mergeIDBRecords(existing, incoming []idbRecord) []idbRecord {
	out := make([]idbRecord, len(existing))
	copy(out, existing)

	byKey := make(map[string]int, len(out))
	seenRecord := make(map[string]struct{}, len(out))
	for i, rec := range out {
		recKey := canonicalRecordKey(rec)
		if recKey != "" {
			byKey[recKey] = i
		}
		seenRecord[canonicalRecord(rec)] = struct{}{}
	}

	for _, rec := range incoming {
		recKey := canonicalRecordKey(rec)
		if recKey != "" {
			if idx, ok := byKey[recKey]; ok {
				out[idx] = idbRecord{
					Key:   rec.Key,
					Value: mergeJSONValue(out[idx].Value, rec.Value),
				}
				seenRecord[canonicalRecord(out[idx])] = struct{}{}
				continue
			}
		}

		recCanonical := canonicalRecord(rec)
		if _, already := seenRecord[recCanonical]; already {
			continue
		}
		out = append(out, rec)
		seenRecord[recCanonical] = struct{}{}
		if recKey != "" {
			byKey[recKey] = len(out) - 1
		}
	}

	return out
}

func mergeJSONValue(existing, incoming any) any {
	existingMap, okExisting := existing.(map[string]any)
	incomingMap, okIncoming := incoming.(map[string]any)
	if okExisting && okIncoming {
		out := make(map[string]any, len(existingMap)+len(incomingMap))
		for k, v := range existingMap {
			out[k] = v
		}
		for k, v := range incomingMap {
			if prior, hasPrior := out[k]; hasPrior {
				out[k] = mergeJSONValue(prior, v)
				continue
			}
			out[k] = v
		}
		return out
	}

	existingSlice, okExistingSlice := existing.([]any)
	incomingSlice, okIncomingSlice := incoming.([]any)
	if okExistingSlice && okIncomingSlice {
		out := make([]any, 0, len(existingSlice)+len(incomingSlice))
		seen := map[string]struct{}{}
		for _, item := range existingSlice {
			out = append(out, item)
			seen[canonicalAny(item)] = struct{}{}
		}
		for _, item := range incomingSlice {
			k := canonicalAny(item)
			if _, ok := seen[k]; ok {
				continue
			}
			out = append(out, item)
			seen[k] = struct{}{}
		}
		return out
	}

	return incoming
}

func canonicalRecordKey(rec idbRecord) string {
	if rec.Key == nil {
		return ""
	}
	return canonicalAny(rec.Key)
}

func canonicalRecord(rec idbRecord) string {
	return canonicalAny(rec)
}

func canonicalAny(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%T:%v", v, v)
	}
	return string(b)
}

func sanitizeAndExtractIndexedDB(raw json.RawMessage) (json.RawMessage, []AttachmentUpload, error) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "" {
		return json.RawMessage(`{}`), nil, nil
	}

	var root any
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, nil, err
	}

	uploads := []AttachmentUpload{}
	seen := map[string]struct{}{}
	sanitized := sanitizeValue(root, "", &uploads, seen)

	buf, err := json.Marshal(sanitized)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal sanitized indexeddb payload: %w", err)
	}
	return json.RawMessage(buf), uploads, nil
}

func sanitizeValue(value any, path string, uploads *[]AttachmentUpload, seen map[string]struct{}) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for k, v := range typed {
			nextPath := k
			if path != "" {
				nextPath = path + "." + k
			}

			if strVal, ok := v.(string); ok {
				if ref, uploaded := extractAttachmentReference(strVal, k, nextPath, uploads, seen); uploaded {
					out[k] = ref
					continue
				}
			}
			out[k] = sanitizeValue(v, nextPath, uploads, seen)
		}
		return out
	case []any:
		out := make([]any, 0, len(typed))
		for idx, item := range typed {
			nextPath := fmt.Sprintf("%s[%d]", path, idx)
			out = append(out, sanitizeValue(item, nextPath, uploads, seen))
		}
		return out
	case string:
		if ref, uploaded := extractAttachmentReference(typed, "", path, uploads, seen); uploaded {
			return ref
		}
		return typed
	default:
		return typed
	}
}

func extractAttachmentReference(raw, keyHint, sourcePath string, uploads *[]AttachmentUpload, seen map[string]struct{}) (map[string]any, bool) {
	mimeType := ""
	base64Payload := ""

	if strings.HasPrefix(raw, "data:") {
		parts := strings.SplitN(raw, ",", 2)
		if len(parts) != 2 {
			return nil, false
		}
		header := parts[0]
		if !strings.Contains(header, ";base64") {
			return nil, false
		}
		mimeType = strings.TrimPrefix(strings.SplitN(header, ";", 2)[0], "data:")
		base64Payload = parts[1]
	} else if keyHint == "base64Data" || keyHint == "base64Url" {
		base64Payload = raw
		mimeType = "application/octet-stream"
	} else {
		return nil, false
	}

	blob, err := base64.StdEncoding.DecodeString(base64Payload)
	if err != nil || len(blob) == 0 {
		return nil, false
	}

	hash := sha256.Sum256(blob)
	hashHex := hex.EncodeToString(hash[:])

	if _, ok := seen[hashHex]; !ok {
		*uploads = append(*uploads, AttachmentUpload{
			Hash:      hashHex,
			MimeType:  mimeType,
			Data:      blob,
			SourceKey: sourcePath,
		})
		seen[hashHex] = struct{}{}
	}

	return map[string]any{
		"__attachment_ref": hashHex,
		"mimeType":         mimeType,
		"size":             len(blob),
	}, true
}
