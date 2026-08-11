package main

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestAnonymousPasteAlwaysGetsDeleteToken(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "pasta.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := &app{
		db:     db,
		config: config{maxFiles: 1, allowedTTLHours: map[int]struct{}{1: {}}, uploadTTL: time.Hour},
	}
	if err := a.migrate(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/pastes", strings.NewReader(`{"objectCount":0,"ttlHours":1}`))
	response := httptest.NewRecorder()
	a.createPaste(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusCreated, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"deleteToken"`) {
		t.Fatalf("anonymous paste lacks delete token: %s", response.Body.String())
	}
}

func TestLoadConfigAcceptsOverridesAndRejectsInvalidLimits(t *testing.T) {
	t.Setenv("PASTA_MAX_FILES", "3")
	t.Setenv("PASTA_MAX_FILE_BYTES", "1048576")
	t.Setenv("PASTA_MAX_PASTE_BYTES", "2097152")
	t.Setenv("PASTA_MAX_TEXT_BYTES", "1024")
	t.Setenv("PASTA_UPLOAD_TTL", "30m")
	t.Setenv("PASTA_READ_TIMEOUT", "45s")
	t.Setenv("PASTA_WRITE_TIMEOUT", "90s")
	t.Setenv("PASTA_TTL_HOURS", "2,48")
	config, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if config.maxFiles != 3 || config.maxFileBytes != 1048576 || config.maxPasteBytes != 2097152 || config.maxTextBytes != 1024 || config.uploadTTL != 30*time.Minute || config.readTimeout != 45*time.Second || config.writeTimeout != 90*time.Second {
		t.Fatalf("unexpected config: %+v", config)
	}
	if _, ok := config.allowedTTLHours[2]; !ok {
		t.Fatal("configured TTL was not accepted")
	}

	t.Setenv("PASTA_MAX_FILES", "0")
	if _, err := loadConfig(); err == nil {
		t.Fatal("zero PASTA_MAX_FILES was accepted")
	}
}

func TestMigratePreservesUnrelatedLegacyData(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "pasta.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE users (id TEXT PRIMARY KEY);
CREATE TABLE sessions (id TEXT PRIMARY KEY);
CREATE TABLE pastes (id TEXT PRIMARY KEY, public_id TEXT, owner_user_id TEXT REFERENCES users(id), status TEXT, expires_at TEXT);`); err != nil {
		t.Fatal(err)
	}
	a := &app{db: db}
	if err := a.migrate(); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"users", "sessions"} {
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil || count != 1 {
			t.Fatalf("legacy table %q was removed: count=%d err=%v", table, count, err)
		}
	}
	hasOwnerColumn, err := tableHasColumn(db, "pastes", "owner_user_id")
	if err != nil {
		t.Fatal(err)
	}
	if !hasOwnerColumn {
		t.Fatal("legacy paste ownership column was removed")
	}
}

func TestPublicConfigExposesOnlyCreationLimits(t *testing.T) {
	a := &app{config: config{maxFiles: 3, maxFileBytes: 1024, maxPasteBytes: 2048, maxTextBytes: 512, allowedTTLHours: map[int]struct{}{48: {}, 2: {}}}}
	response := httptest.NewRecorder()
	a.publicConfig(response, httptest.NewRequest(http.MethodGet, "/api/v1/config", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	body := response.Body.String()
	for _, value := range []string{`"maxFiles":3`, `"maxFileBytes":1024`, `"maxPasteBytes":2048`, `"maxTextBytes":512`, `"ttlHours":[2,48]`} {
		if !strings.Contains(body, value) {
			t.Fatalf("response missing %s: %s", value, body)
		}
	}
}

func TestPublicStatsCountsOnlyAvailableLinksAndFiles(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "pasta.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := &app{db: db}
	if err := a.migrate(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	insertPaste := func(id, status string, expiresAt time.Time) {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO pastes(id,public_id,status,burn_after_read,expires_at,upload_deadline,created_at) VALUES(?,?,?,0,?,?,?)`, id, id, status, expiresAt.Format(time.RFC3339Nano), now.Add(time.Hour).Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}
	insertPaste("active", "active", now.Add(time.Hour))
	insertPaste("expired", "active", now.Add(-time.Hour))
	insertPaste("uploading", "uploading", now.Add(time.Hour))
	if _, err := db.Exec(`INSERT INTO paste_objects(id,paste_id,storage_path,uploaded) VALUES('active-uploaded','active','active-uploaded',1),('active-pending','active','active-pending',0),('expired-uploaded','expired','expired-uploaded',1),('uploading-uploaded','uploading','uploading-uploaded',1)`); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	a.publicStats(response, httptest.NewRequest(http.MethodGet, "/api/v1/stats", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusOK, response.Body.String())
	}
	if body := response.Body.String(); body != "{\"links\":1,\"files\":1}\n" {
		t.Fatalf("stats = %s, want one available link and file", body)
	}
}

func TestCleanupRetainsActivePastePastUploadDeadline(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "pasta.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := &app{db: db, objects: t.TempDir()}
	if err := a.migrate(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := db.Exec(`INSERT INTO pastes(id,public_id,status,burn_after_read,expires_at,upload_deadline,created_at) VALUES('paste','public','active',0,?,?,?)`, now.Add(time.Hour).Format(time.RFC3339Nano), now.Add(-time.Hour).Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	a.cleanup()
	var status string
	if err := db.QueryRow(`SELECT status FROM pastes WHERE id='paste'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "active" {
		t.Fatalf("active paste status = %q, want active", status)
	}
}

func TestCleanupRetainsClaimedPasteForCapabilityWindow(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "pasta.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := &app{db: db, objects: t.TempDir()}
	if err := a.migrate(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := db.Exec(`INSERT INTO pastes(id,public_id,status,burn_after_read,expires_at,upload_deadline,claimed_at,created_at) VALUES('paste','public','claimed',1,?,?,?,?)`, now.Add(time.Hour).Format(time.RFC3339Nano), now.Add(time.Hour).Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	a.cleanup()
	var status string
	if err := db.QueryRow(`SELECT status FROM pastes WHERE id='paste'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "claimed" {
		t.Fatalf("fresh claimed paste status = %q, want claimed", status)
	}
}

func TestConcurrentBurnClaimsOnlyYieldCiphertextOnce(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "pasta.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	a := &app{db: db}
	if err := a.migrate(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := db.Exec(`INSERT INTO pastes(id,public_id,encrypted_manifest,manifest_nonce,status,burn_after_read,expires_at,upload_deadline,created_at) VALUES('paste','public','ciphertext','nonce','active',1,?,?,?)`, now.Add(time.Hour).Format(time.RFC3339Nano), now.Add(time.Hour).Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	statuses := make(chan int, 2)
	for range 2 {
		go func() {
			request := httptest.NewRequest(http.MethodPost, "/api/v1/pastes/public/claim", nil)
			request.SetPathValue("id", "public")
			response := httptest.NewRecorder()
			a.claimPaste(response, request)
			statuses <- response.Code
		}()
	}
	first, second := <-statuses, <-statuses
	if (first == http.StatusOK) == (second == http.StatusOK) {
		t.Fatalf("claim statuses = %d, %d; want exactly one successful claim", first, second)
	}
}

func TestCleanupAfterRestartRemovesExpiredObjects(t *testing.T) {
	dataDir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dataDir, "pasta.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := &app{db: db, objects: filepath.Join(dataDir, "objects")}
	if err := a.migrate(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(a.objects, "public"), 0700); err != nil {
		t.Fatal(err)
	}
	objectPath := filepath.Join(a.objects, "public", "object")
	if err := os.WriteFile(objectPath, []byte("ciphertext"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(objectPath+".upload", []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := db.Exec(`INSERT INTO pastes(id,public_id,status,burn_after_read,expires_at,upload_deadline,created_at) VALUES('paste','public','active',0,?,?,?)`, now.Add(-time.Hour).Format(time.RFC3339Nano), now.Add(time.Hour).Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO paste_objects(id,paste_id,storage_path,bytes,uploaded) VALUES('object','paste','public/object',10,1)`); err != nil {
		t.Fatal(err)
	}
	// A fresh app instance invokes this cleanup path at startup.
	restarted := &app{db: db, objects: a.objects}
	restarted.cleanup()
	var status string
	if err := db.QueryRow(`SELECT status FROM pastes WHERE id='paste'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "deleted" {
		t.Fatalf("expired paste status = %q, want deleted", status)
	}
	if _, err := os.Stat(objectPath); !os.IsNotExist(err) {
		t.Fatalf("expired object still exists: %v", err)
	}
	if _, err := os.Stat(objectPath + ".upload"); !os.IsNotExist(err) {
		t.Fatalf("expired partial upload still exists: %v", err)
	}
}

func TestChunkUploadEnforcesAggregateLimits(t *testing.T) {
	dataDir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dataDir, "pasta.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := &app{
		db:      db,
		objects: filepath.Join(dataDir, "objects"),
		config:  config{maxFileBytes: 1, maxPasteBytes: 100},
	}
	if err := a.migrate(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.Exec(`INSERT INTO pastes(id,public_id,status,burn_after_read,expires_at,upload_deadline,created_at) VALUES('paste','public','uploading',0,?,?,?)`, now, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO paste_objects(id,paste_id,storage_path) VALUES('object','paste','public/object')`); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPut, "/api/v1/pastes/public/objects/object/chunks/0", strings.NewReader(strings.Repeat("a", 66)))
	request.SetPathValue("id", "public")
	request.SetPathValue("object", "object")
	request.SetPathValue("chunk", "0")
	response := httptest.NewRecorder()
	a.uploadChunk(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("first chunk status = %d, want %d", response.Code, http.StatusNoContent)
	}

	request = httptest.NewRequest(http.MethodPut, "/api/v1/pastes/public/objects/object/chunks/1", strings.NewReader("b"))
	request.SetPathValue("id", "public")
	request.SetPathValue("object", "object")
	request.SetPathValue("chunk", "1")
	response = httptest.NewRecorder()
	a.uploadChunk(response, request)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over-limit chunk status = %d, want %d", response.Code, http.StatusRequestEntityTooLarge)
	}
}

func TestChunkUploadIsOrderedAndIdempotent(t *testing.T) {
	dataDir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dataDir, "pasta.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := &app{
		db:      db,
		objects: filepath.Join(dataDir, "objects"),
		config:  config{maxFileBytes: 1, maxPasteBytes: 2},
	}
	if err := a.migrate(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.Exec(`INSERT INTO pastes(id,public_id,status,burn_after_read,expires_at,upload_deadline,created_at) VALUES('paste','public','uploading',0,?,?,?)`, now, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO paste_objects(id,paste_id,storage_path) VALUES('object','paste','public/object')`); err != nil {
		t.Fatal(err)
	}
	upload := func(chunk, body string, final bool) int {
		t.Helper()
		request := httptest.NewRequest(http.MethodPut, "/api/v1/pastes/public/objects/object/chunks/"+chunk, strings.NewReader(body))
		request.SetPathValue("id", "public")
		request.SetPathValue("object", "object")
		request.SetPathValue("chunk", chunk)
		if final {
			request.Header.Set("X-Chunk-Final", "1")
		}
		response := httptest.NewRecorder()
		a.uploadChunk(response, request)
		return response.Code
	}
	if got := upload("0", "a", false); got != http.StatusNoContent {
		t.Fatalf("initial chunk status = %d, want %d", got, http.StatusNoContent)
	}
	if got := upload("0", "a", false); got != http.StatusNoContent {
		t.Fatalf("duplicate chunk status = %d, want %d", got, http.StatusNoContent)
	}
	if got := upload("2", "c", false); got != http.StatusConflict {
		t.Fatalf("out-of-order chunk status = %d, want %d", got, http.StatusConflict)
	}
	if got := upload("1", "b", true); got != http.StatusNoContent {
		t.Fatalf("final chunk status = %d, want %d", got, http.StatusNoContent)
	}
	if got := upload("1", "b", true); got != http.StatusNoContent {
		t.Fatalf("duplicate final chunk status = %d, want %d", got, http.StatusNoContent)
	}
	content, err := os.ReadFile(filepath.Join(a.objects, "public", "object"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "ab" {
		t.Fatalf("stored chunks = %q, want %q", content, "ab")
	}
	var bytes, nextChunk, uploaded int
	if err := db.QueryRow(`SELECT bytes,next_chunk,uploaded FROM paste_objects WHERE id='object'`).Scan(&bytes, &nextChunk, &uploaded); err != nil {
		t.Fatal(err)
	}
	if bytes != 2 || nextChunk != 2 || uploaded != 1 {
		t.Fatalf("chunk state = bytes:%d next:%d uploaded:%d, want bytes:2 next:2 uploaded:1", bytes, nextChunk, uploaded)
	}
}

func TestBurnClaimAllowsEachObjectOnce(t *testing.T) {
	dataDir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dataDir, "pasta.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := &app{db: db, objects: filepath.Join(dataDir, "objects")}
	if err := a.migrate(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(a.objects, "public"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.objects, "public", "object"), []byte("ciphertext"), 0600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := db.Exec(`INSERT INTO pastes(id,public_id,status,burn_after_read,expires_at,upload_deadline,claimed_at,created_at) VALUES('paste','public','claimed',1,?,?,?,?)`, now.Add(time.Hour).Format(time.RFC3339Nano), now.Add(time.Hour).Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO paste_objects(id,paste_id,storage_path,uploaded) VALUES('object','paste','public/object',1)`); err != nil {
		t.Fatal(err)
	}
	token := "claim-token"
	if _, err := db.Exec(`INSERT INTO claims(token_hash,paste_id,expires_at) VALUES(?,?,?)`, hash(token), "paste", now.Add(time.Hour).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	download := func() int {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "/api/v1/pastes/public/objects/object", nil)
		request.SetPathValue("id", "public")
		request.SetPathValue("object", "object")
		request.Header.Set("X-Claim-Token", token)
		response := httptest.NewRecorder()
		a.downloadObject(response, request)
		return response.Code
	}
	if got := download(); got != http.StatusOK {
		t.Fatalf("first download status = %d, want %d", got, http.StatusOK)
	}
	if got := download(); got != http.StatusNotFound {
		t.Fatalf("second download status = %d, want %d", got, http.StatusNotFound)
	}
}
func TestStorageErrorReportsFullStorage(t *testing.T) {
	a := &app{}
	response := httptest.NewRecorder()
	a.storageError(response, syscall.ENOSPC)
	if response.Code != http.StatusInsufficientStorage {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusInsufficientStorage)
	}
}
