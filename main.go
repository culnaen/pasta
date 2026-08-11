package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed web/*
var webFiles embed.FS

const (
	claimTokenTTL   = 5 * time.Minute
	cleanupInterval = time.Minute
)

type config struct {
	maxFiles        int
	maxFileBytes    int64
	maxPasteBytes   int64
	maxTextBytes    int
	uploadTTL       time.Duration
	readTimeout     time.Duration
	writeTimeout    time.Duration
	allowedTTLHours map[int]struct{}
}

func loadConfig() (config, error) {
	maxFiles, err := positiveEnvInt("PASTA_MAX_FILES", 10)
	if err != nil {
		return config{}, err
	}
	maxFileBytes, err := positiveEnvInt64("PASTA_MAX_FILE_BYTES", 50<<20)
	if err != nil {
		return config{}, err
	}
	maxPasteBytes, err := positiveEnvInt64("PASTA_MAX_PASTE_BYTES", 100<<20)
	if err != nil {
		return config{}, err
	}
	maxTextBytes, err := positiveEnvInt("PASTA_MAX_TEXT_BYTES", 1<<20)
	if err != nil {
		return config{}, err
	}
	uploadTTL, err := durationEnv("PASTA_UPLOAD_TTL", time.Hour)
	if err != nil {
		return config{}, err
	}
	readTimeout, err := durationEnv("PASTA_READ_TIMEOUT", 2*time.Minute)
	if err != nil {
		return config{}, err
	}
	writeTimeout, err := durationEnv("PASTA_WRITE_TIMEOUT", 2*time.Minute)
	if err != nil {
		return config{}, err
	}
	ttlValues := strings.Split(env("PASTA_TTL_HOURS", "1,24,168,720"), ",")
	allowedTTLHours := make(map[int]struct{}, len(ttlValues))
	for _, value := range ttlValues {
		ttl, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || ttl <= 0 {
			return config{}, fmt.Errorf("PASTA_TTL_HOURS must contain positive integer hours")
		}
		allowedTTLHours[ttl] = struct{}{}
	}
	if maxPasteBytes < maxFileBytes {
		return config{}, fmt.Errorf("PASTA_MAX_PASTE_BYTES must be at least PASTA_MAX_FILE_BYTES")
	}
	return config{maxFiles: maxFiles, maxFileBytes: maxFileBytes, maxPasteBytes: maxPasteBytes, maxTextBytes: maxTextBytes, uploadTTL: uploadTTL, readTimeout: readTimeout, writeTimeout: writeTimeout, allowedTTLHours: allowedTTLHours}, nil
}

func positiveEnvInt(name string, fallback int) (int, error) {
	value, err := positiveEnvInt64(name, int64(fallback))
	return int(value), err
}

func positiveEnvInt64(name string, fallback int64) (int64, error) {
	value := env(name, "")
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return parsed, nil
}

func durationEnv(name string, fallback time.Duration) (time.Duration, error) {
	value := env(name, "")
	if value == "" {
		return fallback, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", name)
	}
	return duration, nil
}
func (c config) maxEncodedFileBytes() int64 {
	return (c.maxFileBytes*4+2)/3 + 64
}

func (a *app) publicConfig(w http.ResponseWriter, r *http.Request) {
	ttlHours := make([]int, 0, len(a.config.allowedTTLHours))
	for hours := range a.config.allowedTTLHours {
		ttlHours = append(ttlHours, hours)
	}
	for i := range ttlHours {
		for j := i + 1; j < len(ttlHours); j++ {
			if ttlHours[j] < ttlHours[i] {
				ttlHours[i], ttlHours[j] = ttlHours[j], ttlHours[i]
			}
		}
	}
	a.writeJSON(w, http.StatusOK, map[string]any{
		"maxFiles":      a.config.maxFiles,
		"maxFileBytes":  a.config.maxFileBytes,
		"maxPasteBytes": a.config.maxPasteBytes,
		"maxTextBytes":  a.config.maxTextBytes,
		"ttlHours":      ttlHours,
	})
}

type statsResponse struct {
	Links int `json:"links"`
	Files int `json:"files"`
}

func (a *app) publicStats(w http.ResponseWriter, r *http.Request) {
	var stats statsResponse
	now := time.Now().UTC().Format(time.RFC3339Nano)
	err := a.db.QueryRowContext(r.Context(), `SELECT COUNT(DISTINCT pastes.id), COUNT(paste_objects.id) FROM pastes LEFT JOIN paste_objects ON paste_objects.paste_id=pastes.id AND paste_objects.uploaded=1 WHERE pastes.status='active' AND pastes.expires_at > ?`, now).Scan(&stats.Links, &stats.Files)
	if err != nil {
		a.storageError(w, err)
		return
	}
	a.writeJSON(w, http.StatusOK, stats)
}

func (c config) maxEncodedPasteBytes() int64 {
	return (c.maxPasteBytes*4+2)/3 + 64*int64(c.maxFiles)
}

type app struct {
	db       *sql.DB
	objects  string
	logger   *slog.Logger
	config   config
	uploadMu sync.Mutex
}

type createRequest struct {
	ObjectCount   int  `json:"objectCount"`
	TTLHours      int  `json:"ttlHours"`
	BurnAfterRead bool `json:"burnAfterRead"`
}
type createResponse struct {
	PublicID       string   `json:"publicId"`
	ObjectIDs      []string `json:"objectIds"`
	DeleteToken    string   `json:"deleteToken,omitempty"`
	UploadDeadline string   `json:"uploadDeadline"`
}
type completeRequest struct {
	Manifest      string `json:"manifest"`
	ManifestNonce string `json:"manifestNonce"`
}
type pasteResponse struct {
	PublicID      string   `json:"publicId"`
	Manifest      string   `json:"manifest"`
	ManifestNonce string   `json:"manifestNonce"`
	ObjectIDs     []string `json:"objectIds"`
	BurnAfterRead bool     `json:"burnAfterRead"`
	ExpiresAt     string   `json:"expiresAt"`
	ClaimToken    string   `json:"claimToken,omitempty"`
}

func main() {
	config, err := loadConfig()
	if err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(2)
	}
	addr := env("PASTA_ADDR", ":8080")
	dataDir := env("PASTA_DATA_DIR", "./data")
	if err := os.MkdirAll(filepath.Join(dataDir, "objects"), 0700); err != nil {
		panic(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dataDir, "pasta.sqlite"))
	if err != nil {
		panic(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	a := &app{db: db, objects: filepath.Join(dataDir, "objects"), logger: slog.Default(), config: config}
	if err := a.migrate(); err != nil {
		panic(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go a.cleanupLoop(ctx)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		a.writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := a.db.PingContext(r.Context()); err != nil {
			http.Error(w, "unavailable", 503)
			return
		}
		a.writeJSON(w, 200, map[string]string{"status": "ready"})
	})
	mux.HandleFunc("GET /api/v1/config", a.publicConfig)
	mux.HandleFunc("GET /api/v1/stats", a.publicStats)
	mux.HandleFunc("POST /api/v1/pastes", a.createPaste)
	mux.HandleFunc("PUT /api/v1/pastes/{id}/objects/{object}", a.uploadObject)
	mux.HandleFunc("PUT /api/v1/pastes/{id}/objects/{object}/chunks/{chunk}", a.uploadChunk)
	mux.HandleFunc("POST /api/v1/pastes/{id}/complete", a.completePaste)
	mux.HandleFunc("GET /api/v1/pastes/{id}", a.getPaste)
	mux.HandleFunc("POST /api/v1/pastes/{id}/claim", a.claimPaste)
	mux.HandleFunc("GET /api/v1/pastes/{id}/objects/{object}", a.downloadObject)
	mux.HandleFunc("DELETE /api/v1/pastes/{id}", a.deletePaste)
	static, _ := fs.Sub(webFiles, "web")
	mux.Handle("GET /static/", http.FileServer(http.FS(static)))
	mux.HandleFunc("GET /{$}", a.page)
	mux.HandleFunc("GET /p/{id}", a.page)
	mux.HandleFunc("GET /d/{id}", a.page)
	server := &http.Server{Addr: addr, Handler: securityHeaders(mux), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: config.readTimeout, WriteTimeout: config.writeTimeout, IdleTimeout: 60 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			a.logger.Error("graceful shutdown failed", "error", err)
		}
	}()
	a.logger.Info("pasta listening", "address", addr, "data_dir", dataDir)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		panic(err)
	}
}

func (a *app) migrate() error {
	_, err := a.db.Exec(`PRAGMA foreign_keys=ON;
CREATE TABLE IF NOT EXISTS pastes (id TEXT PRIMARY KEY, public_id TEXT NOT NULL UNIQUE, delete_token_hash TEXT NOT NULL DEFAULT '', format_version INTEGER NOT NULL DEFAULT 1, encrypted_manifest TEXT, manifest_nonce TEXT, status TEXT NOT NULL, burn_after_read INTEGER NOT NULL, expires_at TEXT NOT NULL, upload_deadline TEXT NOT NULL, claimed_at TEXT, created_at TEXT NOT NULL, completed_at TEXT, total_bytes INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS paste_objects (id TEXT PRIMARY KEY, paste_id TEXT NOT NULL REFERENCES pastes(id) ON DELETE CASCADE, storage_path TEXT NOT NULL UNIQUE, bytes INTEGER NOT NULL DEFAULT 0, uploaded INTEGER NOT NULL DEFAULT 0, next_chunk INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS claims (token_hash TEXT PRIMARY KEY,paste_id TEXT NOT NULL,expires_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS claim_reads (token_hash TEXT NOT NULL REFERENCES claims(token_hash) ON DELETE CASCADE, object_id TEXT NOT NULL REFERENCES paste_objects(id) ON DELETE CASCADE, PRIMARY KEY(token_hash,object_id));
CREATE INDEX IF NOT EXISTS pastes_status_expiry ON pastes(status, expires_at);`)
	if err != nil {
		return err
	}
	hasNextChunk, err := tableHasColumn(a.db, "paste_objects", "next_chunk")
	if err != nil {
		return err
	}
	if !hasNextChunk {
		_, err = a.db.Exec(`ALTER TABLE paste_objects ADD COLUMN next_chunk INTEGER NOT NULL DEFAULT 0`)
	}
	return err
}

func tableHasColumn(db *sql.DB, table, column string) (bool, error) {
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

func (a *app) createPaste(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if !decodeJSON(r, &req) {
		a.error(w, 400, "invalid request")
		return
	}
	if req.ObjectCount < 0 || req.ObjectCount > a.config.maxFiles {
		a.error(w, 400, "invalid file count")
		return
	}
	if _, ok := a.config.allowedTTLHours[req.TTLHours]; !ok {
		a.error(w, 400, "invalid TTL")
		return
	}
	now := time.Now().UTC()
	id, err := randomID()
	if err != nil {
		a.error(w, 500, "storage failure")
		return
	}
	pub, err := randomID()
	if err != nil {
		a.error(w, 500, "storage failure")
		return
	}
	deleteToken, err := randomToken()
	if err != nil {
		a.error(w, 500, "storage failure")
		return
	}
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		a.error(w, 500, "storage failure")
		return
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(r.Context(), `INSERT INTO pastes(id,public_id,delete_token_hash,status,burn_after_read,expires_at,upload_deadline,created_at) VALUES(?,?,?,?,?,?,?,?)`, id, pub, hash(deleteToken), "uploading", boolInt(req.BurnAfterRead), now.Add(time.Duration(req.TTLHours)*time.Hour).Format(time.RFC3339Nano), now.Add(a.config.uploadTTL).Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	if err != nil {
		a.error(w, 500, "storage failure")
		return
	}
	objectIDs := make([]string, req.ObjectCount)
	for i := range objectIDs {
		objectIDs[i], err = randomID()
		if err != nil {
			a.error(w, 500, "storage failure")
			return
		}
		_, err = tx.ExecContext(r.Context(), `INSERT INTO paste_objects(id,paste_id,storage_path) VALUES(?,?,?)`, objectIDs[i], id, filepath.Join(pub, objectIDs[i]))
		if err != nil {
			a.error(w, 500, "storage failure")
			return
		}
	}
	if err = tx.Commit(); err != nil {
		a.error(w, 500, "storage failure")
		return
	}
	a.writeJSON(w, http.StatusCreated, createResponse{PublicID: pub, ObjectIDs: objectIDs, DeleteToken: deleteToken, UploadDeadline: now.Add(a.config.uploadTTL).Format(time.RFC3339)})
}

func (a *app) uploadObject(w http.ResponseWriter, r *http.Request) {
	pub, oid := r.PathValue("id"), r.PathValue("object")
	var pasteID, path, status string
	err := a.db.QueryRowContext(r.Context(), `SELECT p.id,o.storage_path,p.status FROM pastes p JOIN paste_objects o ON o.paste_id=p.id WHERE p.public_id=? AND o.id=?`, pub, oid).Scan(&pasteID, &path, &status)
	if err != nil || status != "uploading" {
		a.error(w, 404, "publication unavailable")
		return
	}
	if r.ContentLength < 0 || r.ContentLength > a.config.maxEncodedFileBytes() {
		a.error(w, 413, "file too large")
		return
	}
	full := filepath.Join(a.objects, path)
	if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
		a.error(w, 500, "storage failure")
		return
	}
	suffix, err := randomID()
	if err != nil {
		a.error(w, 500, "storage failure")
		return
	}
	tmp := full + ".tmp-" + suffix
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		a.error(w, 500, "storage failure")
		return
	}
	n, copyErr := io.Copy(f, http.MaxBytesReader(w, r.Body, a.config.maxEncodedFileBytes()))
	closeErr := f.Close()
	if copyErr != nil || closeErr != nil {
		os.Remove(tmp)
		a.error(w, 400, "upload failed")
		return
	}
	if err := os.Rename(tmp, full); err != nil {
		os.Remove(tmp)
		a.error(w, 500, "storage failure")
		return
	}
	_, err = a.db.ExecContext(r.Context(), `UPDATE paste_objects SET bytes=?,uploaded=1 WHERE id=? AND paste_id=?`, n, oid, pasteID)
	if err != nil {
		a.error(w, 500, "storage failure")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *app) uploadChunk(w http.ResponseWriter, r *http.Request) {
	chunk, err := strconv.Atoi(r.PathValue("chunk"))
	if err != nil || chunk < 0 {
		a.error(w, 400, "invalid chunk")
		return
	}
	if r.ContentLength < 0 || r.ContentLength > a.config.maxEncodedFileBytes() {
		a.error(w, 413, "file too large")
		return
	}

	a.uploadMu.Lock()
	defer a.uploadMu.Unlock()

	var pasteID, path, status string
	var objectBytes int64
	var nextChunk, uploaded int
	err = a.db.QueryRowContext(r.Context(), `SELECT p.id,o.storage_path,p.status,o.bytes,o.next_chunk,o.uploaded FROM pastes p JOIN paste_objects o ON o.paste_id=p.id WHERE p.public_id=? AND o.id=?`, r.PathValue("id"), r.PathValue("object")).Scan(&pasteID, &path, &status, &objectBytes, &nextChunk, &uploaded)
	if err != nil || status != "uploading" {
		a.error(w, 404, "publication unavailable")
		return
	}
	full := filepath.Join(a.objects, path)
	if chunk < nextChunk {
		if r.Header.Get("X-Chunk-Final") == "1" && chunk == nextChunk-1 && uploaded == 0 {
			if err := a.finalizeChunk(pasteID, r.PathValue("object"), full); err != nil {
				a.storageError(w, err)
				return
			}
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if uploaded != 0 || chunk != nextChunk || (objectBytes > 0 && nextChunk == 0) {
		a.error(w, http.StatusConflict, "chunk sequence conflict")
		return
	}
	var uploadedBytes int64
	if err := a.db.QueryRowContext(r.Context(), `SELECT COALESCE(SUM(bytes),0) FROM paste_objects WHERE paste_id=?`, pasteID).Scan(&uploadedBytes); err != nil || objectBytes+r.ContentLength > a.config.maxEncodedFileBytes() || uploadedBytes+r.ContentLength > a.config.maxEncodedPasteBytes() {
		a.error(w, 413, "publication too large")
		return
	}
	if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
		a.storageError(w, err)
		return
	}
	tmp := full + ".upload"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		a.storageError(w, err)
		return
	}
	n, copyErr := io.Copy(f, http.MaxBytesReader(w, r.Body, a.config.maxEncodedPasteBytes()-uploadedBytes))
	closeErr := f.Close()
	if errors.Is(copyErr, syscall.ENOSPC) || errors.Is(closeErr, syscall.ENOSPC) {
		a.storageError(w, syscall.ENOSPC)
		return
	}
	if copyErr != nil || closeErr != nil || n != r.ContentLength {
		a.error(w, 400, "upload failed")
		return
	}
	if _, err := a.db.ExecContext(r.Context(), `UPDATE paste_objects SET bytes=bytes+?,next_chunk=? WHERE id=? AND paste_id=?`, n, chunk+1, r.PathValue("object"), pasteID); err != nil {
		a.error(w, 500, "storage failure")
		return
	}
	if r.Header.Get("X-Chunk-Final") == "1" {
		if err := a.finalizeChunk(pasteID, r.PathValue("object"), full); err != nil {
			a.storageError(w, err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *app) finalizeChunk(pasteID, objectID, full string) error {
	if err := os.Rename(full+".upload", full); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if _, err := os.Stat(full); err != nil {
			return err
		}
	}
	_, err := a.db.Exec(`UPDATE paste_objects SET uploaded=1 WHERE id=? AND paste_id=?`, objectID, pasteID)
	return err
}

func (a *app) completePaste(w http.ResponseWriter, r *http.Request) {
	pub := r.PathValue("id")
	var req completeRequest
	if !decodeJSON(r, &req) || len(req.Manifest) == 0 || len(req.Manifest) > a.config.maxTextBytes*2 || len(req.ManifestNonce) == 0 {
		a.error(w, 400, "invalid manifest")
		return
	}
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		a.error(w, 500, "storage failure")
		return
	}
	defer tx.Rollback()
	var id, status, deadline string
	err = tx.QueryRowContext(r.Context(), `SELECT id,status,upload_deadline FROM pastes WHERE public_id=?`, pub).Scan(&id, &status, &deadline)
	if err != nil || status != "uploading" || expired(deadline) {
		a.error(w, 404, "publication unavailable")
		return
	}
	var uploaded, expected, total int
	if err = tx.QueryRowContext(r.Context(), `SELECT COUNT(*) FILTER (WHERE uploaded=1), COUNT(*), COALESCE(SUM(bytes),0) FROM paste_objects WHERE paste_id=?`, id).Scan(&uploaded, &expected, &total); err != nil || uploaded != expected || int64(total) > a.config.maxEncodedPasteBytes() {
		a.error(w, 400, "incomplete or too large upload")
		return
	}
	_, err = tx.ExecContext(r.Context(), `UPDATE pastes SET encrypted_manifest=?,manifest_nonce=?,status='active',completed_at=?,total_bytes=? WHERE id=?`, req.Manifest, req.ManifestNonce, time.Now().UTC().Format(time.RFC3339Nano), total, id)
	if err != nil {
		a.error(w, 500, "storage failure")
		return
	}
	if err = tx.Commit(); err != nil {
		a.error(w, 500, "storage failure")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *app) getPaste(w http.ResponseWriter, r *http.Request)   { a.servePaste(w, r, false) }
func (a *app) claimPaste(w http.ResponseWriter, r *http.Request) { a.servePaste(w, r, true) }
func (a *app) servePaste(w http.ResponseWriter, r *http.Request, claim bool) {
	pub := r.PathValue("id")
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		a.error(w, 500, "storage failure")
		return
	}
	defer tx.Rollback()
	var id, manifest, nonce, status, expires string
	var burn int
	err = tx.QueryRowContext(r.Context(), `SELECT id,encrypted_manifest,manifest_nonce,status,burn_after_read,expires_at FROM pastes WHERE public_id=?`, pub).Scan(&id, &manifest, &nonce, &status, &burn, &expires)
	if err != nil || status != "active" || expired(expires) {
		a.error(w, 404, "publication unavailable")
		return
	}
	if burn != 0 && !claim {
		a.writeJSON(w, 200, map[string]any{"publicId": pub, "burnAfterRead": true, "expiresAt": expires})
		return
	}
	claimToken := ""
	if burn != 0 {
		claimToken, err = randomToken()
		if err != nil {
			a.error(w, 500, "storage failure")
			return
		}
		result, err := tx.ExecContext(r.Context(), `UPDATE pastes SET status='claimed',claimed_at=? WHERE id=? AND status='active'`, time.Now().UTC().Format(time.RFC3339Nano), id)
		if err != nil {
			a.error(w, 500, "storage failure")
			return
		}
		n, _ := result.RowsAffected()
		if n != 1 {
			a.error(w, 404, "publication unavailable")
			return
		}
		_, err = tx.ExecContext(r.Context(), `INSERT INTO claims(token_hash,paste_id,expires_at) VALUES(?,?,?)`, hash(claimToken), id, time.Now().Add(claimTokenTTL).UTC().Format(time.RFC3339Nano))
		if err != nil {
			a.error(w, 500, "storage failure")
			return
		}
	}
	rows, err := tx.QueryContext(r.Context(), `SELECT id FROM paste_objects WHERE paste_id=? AND uploaded=1 ORDER BY id`, id)
	if err != nil {
		a.error(w, 500, "storage failure")
		return
	}
	defer rows.Close()
	var objects []string
	for rows.Next() {
		var oid string
		rows.Scan(&oid)
		objects = append(objects, oid)
	}
	if err = tx.Commit(); err != nil {
		a.error(w, 500, "storage failure")
		return
	}
	a.writeJSON(w, 200, pasteResponse{PublicID: pub, Manifest: manifest, ManifestNonce: nonce, ObjectIDs: objects, BurnAfterRead: burn != 0, ExpiresAt: expires, ClaimToken: claimToken})
}

func (a *app) downloadObject(w http.ResponseWriter, r *http.Request) {
	pub, oid := r.PathValue("id"), r.PathValue("object")
	var path, status, expires, pasteID string
	var burn int
	err := a.db.QueryRowContext(r.Context(), `SELECT o.storage_path,p.status,p.expires_at,p.id,p.burn_after_read FROM pastes p JOIN paste_objects o ON o.paste_id=p.id WHERE p.public_id=? AND o.id=? AND o.uploaded=1`, pub, oid).Scan(&path, &status, &expires, &pasteID, &burn)
	if err != nil || expired(expires) || (status != "active" && status != "claimed") {
		a.error(w, 404, "publication unavailable")
		return
	}
	if burn != 0 {
		tokenHash := hash(r.Header.Get("X-Claim-Token"))
		result, err := a.db.ExecContext(r.Context(), `INSERT INTO claim_reads(token_hash,object_id)
SELECT ?,? WHERE EXISTS (SELECT 1 FROM claims WHERE token_hash=? AND paste_id=? AND expires_at>?)
ON CONFLICT DO NOTHING`, tokenHash, oid, tokenHash, pasteID, time.Now().UTC().Format(time.RFC3339Nano))
		if err != nil {
			a.storageError(w, err)
			return
		}
		n, err := result.RowsAffected()
		if err != nil || n != 1 {
			a.error(w, 404, "publication unavailable")
			return
		}
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", "attachment")
	http.ServeFile(w, r, filepath.Join(a.objects, path))
}

func (a *app) deletePaste(w http.ResponseWriter, r *http.Request) {
	pub := r.PathValue("id")
	var id, token, status string
	err := a.db.QueryRowContext(r.Context(), `SELECT id,delete_token_hash,status FROM pastes WHERE public_id=?`, pub).Scan(&id, &token, &status)
	if err != nil || status == "deleted" {
		a.error(w, 404, "publication unavailable")
		return
	}
	if hash(r.Header.Get("X-Delete-Token")) != token {
		a.error(w, 404, "publication unavailable")
		return
	}
	_, err = a.db.ExecContext(r.Context(), `UPDATE pastes SET status='deleting' WHERE id=?`, id)
	if err != nil {
		a.error(w, 500, "storage failure")
		return
	}
	go a.deleteByID(id)
	w.WriteHeader(http.StatusNoContent)
}

func (a *app) page(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/":
		serveAsset(w, "index.html", "text/html; charset=utf-8")
	case strings.HasPrefix(r.URL.Path, "/p/"):
		serveAsset(w, "paste.html", "text/html; charset=utf-8")
	case strings.HasPrefix(r.URL.Path, "/d/"):
		serveAsset(w, "manage.html", "text/html; charset=utf-8")
	default:
		http.NotFound(w, r)
	}
}
func serveAsset(w http.ResponseWriter, name, ct string) {
	b, err := webFiles.ReadFile("web/" + name)
	if err != nil {
		http.Error(w, "not found", 404)
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Write(b)
}
func (a *app) cleanupLoop(ctx context.Context) {
	a.cleanup()
	t := time.NewTicker(cleanupInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.cleanup()
		}
	}
}
func (a *app) cleanup() {
	nowTime := time.Now().UTC()
	now := nowTime.Format(time.RFC3339Nano)
	claimDeadline := nowTime.Add(-claimTokenTTL).Format(time.RFC3339Nano)
	rows, err := a.db.Query(`SELECT id FROM pastes WHERE (status='active' AND expires_at < ?) OR (status='uploading' AND (expires_at < ? OR upload_deadline < ?)) OR status='deleting' OR (status='claimed' AND claimed_at < ?)`, now, now, now, claimDeadline)
	if err != nil {
		return
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	for _, id := range ids {
		a.db.Exec(`UPDATE pastes SET status='deleting' WHERE id=? AND status != 'deleted'`, id)
		a.deleteByID(id)
	}
	a.db.Exec(`DELETE FROM claims WHERE expires_at < ?`, now)
}
func (a *app) deleteByID(id string) {
	rows, err := a.db.Query(`SELECT storage_path FROM paste_objects WHERE paste_id=?`, id)
	if err != nil {
		return
	}
	defer rows.Close()
	ok := true
	for rows.Next() {
		var p string
		rows.Scan(&p)
		for _, path := range []string{filepath.Join(a.objects, p), filepath.Join(a.objects, p) + ".upload"} {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				ok = false
			}
		}
	}
	if ok {
		a.db.Exec(`UPDATE pastes SET status='deleted' WHERE id=?`, id)
	}
}

func (a *app) storageError(w http.ResponseWriter, err error) {
	if errors.Is(err, syscall.ENOSPC) {
		a.error(w, http.StatusInsufficientStorage, "storage full")
		return
	}
	a.error(w, http.StatusInternalServerError, "storage failure")
}
func (a *app) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
func (a *app) error(w http.ResponseWriter, status int, msg string) {
	a.writeJSON(w, status, map[string]string{"error": msg})
}
func decodeJSON(r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(nil, r.Body, 2<<20)
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v) == nil
}
func randomID() (string, error) {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
func hash(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func expired(s string) bool {
	t, e := time.Parse(time.RFC3339Nano, s)
	return e != nil || !time.Now().Before(t)
}
func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		next.ServeHTTP(w, r)
	})
}

var _ = fmt.Sprintf
