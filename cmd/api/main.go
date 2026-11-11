package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type API struct {
	db *pgxpool.Pool
}

type Resource struct {
	ID            string    `json:"id"`
	ResourceKey   string    `json:"resource_key"`
	DisplayName   string    `json:"display_name"`
	ResourceType  string    `json:"resource_type"`
	Environment   string    `json:"environment"`
	ConnectionRef string    `json:"connection_ref"`
	Enabled       bool      `json:"enabled"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type CreateResourceRequest struct {
	ResourceKey   string `json:"resource_key"`
	DisplayName   string `json:"display_name"`
	Environment   string `json:"environment"`
	ConnectionRef string `json:"connection_ref"`
}

type Policy struct {
	ResourceKey            string    `json:"resource_key"`
	RPOSeconds             int       `json:"rpo_seconds"`
	RTOSeconds             int       `json:"rto_seconds"`
	RetentionDays          int       `json:"retention_days"`
	FullBackupCron         string    `json:"full_backup_cron"`
	DifferentialBackupCron string    `json:"differential_backup_cron"`
	WALArchivingRequired   bool      `json:"wal_archiving_required"`
	RestoreTestCron        string    `json:"restore_test_cron"`
	Enabled                bool      `json:"enabled"`
	CreatedAt              time.Time `json:"created_at"`
	UpdatedAt              time.Time `json:"updated_at"`
}

type PolicyRequest struct {
	RPOSeconds             int    `json:"rpo_seconds"`
	RTOSeconds             int    `json:"rto_seconds"`
	RetentionDays          int    `json:"retention_days"`
	FullBackupCron         string `json:"full_backup_cron"`
	DifferentialBackupCron string `json:"differential_backup_cron"`
	WALArchivingRequired   bool   `json:"wal_archiving_required"`
	RestoreTestCron        string `json:"restore_test_cron"`
	Enabled                bool   `json:"enabled"`
}

var resourceKeyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)

func main() {
	addr := os.Getenv("HTTP_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8080"
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		log.Fatal("DATABASE_URL is required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	db, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		log.Fatalf("initialize metadata database: %v", err)
	}
	defer db.Close()

	if err := db.Ping(ctx); err != nil {
		log.Fatalf("connect to metadata database: %v", err)
	}

	api := &API{db: db}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health/live", api.live)
	mux.HandleFunc("GET /health/ready", api.ready)

	mux.HandleFunc("GET /api/v1/resources", api.listResources)
	mux.HandleFunc("POST /api/v1/resources", api.createResource)
	mux.HandleFunc("GET /api/v1/resources/{resourceKey}/policy", api.getPolicy)
	mux.HandleFunc("PUT /api/v1/resources/{resourceKey}/policy", api.putPolicy)

	mux.HandleFunc(
		"POST /api/v1/resources/{resourceKey}/jobs",
		api.createJob,
	)
	mux.HandleFunc("GET /api/v1/jobs", api.listJobs)
	mux.HandleFunc("GET /api/v1/artifacts", api.listArtifacts)
	mux.HandleFunc("GET /api/v1/restore-tests", api.listRestoreTests)
	mux.HandleFunc("GET /api/v1/audit-events", api.listAuditEvents)

	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Printf("DR management API listening on %s", addr)
	log.Fatal(server.ListenAndServe())
}

func (a *API) live(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "alive"})
}

func (a *API) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	if err := a.db.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status": "not_ready",
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"status":   "ready",
		"database": "connected",
	})
}

func (a *API) listResources(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Query(r.Context(), `
		SELECT id::text, resource_key, display_name, resource_type,
		       environment, connection_ref, enabled, created_at, updated_at
		FROM protected_resources
		ORDER BY resource_key
	`)
	if err != nil {
		internalError(w, err)
		return
	}
	defer rows.Close()

	resources := make([]Resource, 0)
	for rows.Next() {
		var item Resource
		if err := rows.Scan(
			&item.ID, &item.ResourceKey, &item.DisplayName,
			&item.ResourceType, &item.Environment, &item.ConnectionRef,
			&item.Enabled, &item.CreatedAt, &item.UpdatedAt,
		); err != nil {
			internalError(w, err)
			return
		}
		resources = append(resources, item)
	}

	if err := rows.Err(); err != nil {
		internalError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"items": resources})
}

func (a *API) createResource(w http.ResponseWriter, r *http.Request) {
	var req CreateResourceRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	req.ResourceKey = strings.TrimSpace(req.ResourceKey)
	req.DisplayName = strings.TrimSpace(req.DisplayName)
	req.Environment = strings.TrimSpace(req.Environment)
	req.ConnectionRef = strings.TrimSpace(req.ConnectionRef)

	if !resourceKeyPattern.MatchString(req.ResourceKey) ||
		req.DisplayName == "" || req.ConnectionRef == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "resource_key must be 2-63 lowercase letters, numbers or hyphens; display_name and connection_ref are required",
		})
		return
	}

	if req.Environment == "" {
		req.Environment = "production"
	}
	if req.Environment != "development" &&
		req.Environment != "staging" &&
		req.Environment != "production" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "environment must be development, staging or production",
		})
		return
	}

	// Connection references identify secrets; raw credentials do not belong here.
	if !strings.HasPrefix(req.ConnectionRef, "secret://") {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "connection_ref must be a secret:// reference, not a database URL or password",
		})
		return
	}

	var item Resource
	err := a.db.QueryRow(r.Context(), `
		INSERT INTO protected_resources
			(resource_key, display_name, resource_type, environment, connection_ref)
		VALUES ($1, $2, 'postgresql', $3, $4)
		RETURNING id::text, resource_key, display_name, resource_type,
		          environment, connection_ref, enabled, created_at, updated_at
	`, req.ResourceKey, req.DisplayName, req.Environment, req.ConnectionRef).Scan(
		&item.ID, &item.ResourceKey, &item.DisplayName, &item.ResourceType,
		&item.Environment, &item.ConnectionRef, &item.Enabled,
		&item.CreatedAt, &item.UpdatedAt,
	)

	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "resource could not be registered; check whether its key already exists",
		})
		return
	}

	writeJSON(w, http.StatusCreated, item)
}

func (a *API) getPolicy(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("resourceKey")
	var p Policy

	err := a.db.QueryRow(r.Context(), `
		SELECT r.resource_key, p.rpo_seconds, p.rto_seconds,
		       p.retention_days, p.full_backup_cron,
		       p.differential_backup_cron, p.wal_archiving_required,
		       p.restore_test_cron, p.enabled, p.created_at, p.updated_at
		FROM recovery_policies p
		JOIN protected_resources r ON r.id = p.resource_id
		WHERE r.resource_key = $1
	`, key).Scan(
		&p.ResourceKey, &p.RPOSeconds, &p.RTOSeconds, &p.RetentionDays,
		&p.FullBackupCron, &p.DifferentialBackupCron,
		&p.WALArchivingRequired, &p.RestoreTestCron,
		&p.Enabled, &p.CreatedAt, &p.UpdatedAt,
	)

	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "policy not found"})
		return
	}
	if err != nil {
		internalError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, p)
}

func (a *API) putPolicy(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("resourceKey")

	var req PolicyRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	if req.RPOSeconds <= 0 || req.RTOSeconds <= 0 || req.RetentionDays <= 0 ||
		strings.TrimSpace(req.FullBackupCron) == "" ||
		strings.TrimSpace(req.DifferentialBackupCron) == "" ||
		strings.TrimSpace(req.RestoreTestCron) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "RPO, RTO, retention and all schedule fields must be valid and non-empty",
		})
		return
	}

	var p Policy
	err := a.db.QueryRow(r.Context(), `
		INSERT INTO recovery_policies (
			resource_id, rpo_seconds, rto_seconds, retention_days,
			full_backup_cron, differential_backup_cron,
			wal_archiving_required, restore_test_cron, enabled
		)
		SELECT id, $2, $3, $4, $5, $6, $7, $8, $9
		FROM protected_resources
		WHERE resource_key = $1
		ON CONFLICT (resource_id) DO UPDATE SET
			rpo_seconds = EXCLUDED.rpo_seconds,
			rto_seconds = EXCLUDED.rto_seconds,
			retention_days = EXCLUDED.retention_days,
			full_backup_cron = EXCLUDED.full_backup_cron,
			differential_backup_cron = EXCLUDED.differential_backup_cron,
			wal_archiving_required = EXCLUDED.wal_archiving_required,
			restore_test_cron = EXCLUDED.restore_test_cron,
			enabled = EXCLUDED.enabled,
			updated_at = NOW()
		RETURNING
			(SELECT resource_key FROM protected_resources WHERE id = resource_id),
			rpo_seconds, rto_seconds, retention_days, full_backup_cron,
			differential_backup_cron, wal_archiving_required,
			restore_test_cron, enabled, created_at, updated_at
	`, key, req.RPOSeconds, req.RTOSeconds, req.RetentionDays,
		req.FullBackupCron, req.DifferentialBackupCron,
		req.WALArchivingRequired, req.RestoreTestCron, req.Enabled).Scan(
		&p.ResourceKey, &p.RPOSeconds, &p.RTOSeconds, &p.RetentionDays,
		&p.FullBackupCron, &p.DifferentialBackupCron,
		&p.WALArchivingRequired, &p.RestoreTestCron,
		&p.Enabled, &p.CreatedAt, &p.UpdatedAt,
	)

	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "resource not found"})
		return
	}
	if err != nil {
		internalError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, p)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	defer r.Body.Close()

	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

func internalError(w http.ResponseWriter, err error) {
	log.Printf("API database error: %v", err)
	writeJSON(w, http.StatusInternalServerError, map[string]string{
		"error": "internal server error",
	})
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		log.Printf("write JSON response: %v", err)
	}
}
