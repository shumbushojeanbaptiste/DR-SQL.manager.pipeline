package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type CreateJobRequest struct {
	Operation  string `json:"operation"`
	BackupType string `json:"backup_type"`
}

type Job struct {
	ID          string     `json:"id"`
	ResourceKey string     `json:"resource_key"`
	Operation   string     `json:"operation"`
	BackupType  string     `json:"backup_type"`
	Status      string     `json:"status"`
	RequestedBy string     `json:"requested_by"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
	Error       *string    `json:"error_message,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

func (a *API) createJob(w http.ResponseWriter, r *http.Request) {
	resourceKey := r.PathValue("resourceKey")

	var req CreateJobRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	req.Operation = strings.TrimSpace(req.Operation)
	req.BackupType = strings.TrimSpace(req.BackupType)

	if req.Operation != "backup" && req.Operation != "restore_test" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "operation must be backup or restore_test",
		})
		return
	}
	if req.BackupType != "logical" && req.BackupType != "full" && req.BackupType != "differential" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "backup_type must be logical, full, or differential",
		})
		return
	}

	ctx := r.Context()
	tx, err := a.db.Begin(ctx)
	if err != nil {
		internalError(w, err)
		return
	}
	defer tx.Rollback(ctx)

	var job Job
	err = tx.QueryRow(ctx, `
		INSERT INTO backup_jobs (
			resource_id, operation, backup_type, status, requested_by
		)
		SELECT id, $2, $3, 'queued', 'local-development'
		FROM protected_resources
		WHERE resource_key = $1 AND enabled = TRUE
		RETURNING id::text, operation, backup_type, status,
		          requested_by, started_at, finished_at,
		          error_message, created_at, updated_at
	`, resourceKey, req.Operation, req.BackupType).Scan(
		&job.ID, &job.Operation, &job.BackupType, &job.Status,
		&job.RequestedBy, &job.StartedAt, &job.FinishedAt,
		&job.Error, &job.CreatedAt, &job.UpdatedAt,
	)

	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "enabled resource not found",
		})
		return
	}
	if err != nil {
		internalError(w, err)
		return
	}
	job.ResourceKey = resourceKey

	details, _ := json.Marshal(map[string]string{
		"operation":   job.Operation,
		"backup_type": job.BackupType,
		"status":      job.Status,
	})

	_, err = tx.Exec(ctx, `
		INSERT INTO audit_events (
			event_type, actor, resource_id, job_id,
			outcome, message, details
		)
		SELECT
			'job.queued', 'local-development', r.id, $2,
			'accepted', 'DR job request recorded', $3::jsonb
		FROM protected_resources r
		WHERE r.resource_key = $1
	`, resourceKey, job.ID, string(details))
	if err != nil {
		internalError(w, err)
		return
	}

	if err := tx.Commit(ctx); err != nil {
		internalError(w, err)
		return
	}

	writeJSON(w, http.StatusAccepted, job)
}

func (a *API) listJobs(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Query(r.Context(), `
		SELECT j.id::text, r.resource_key, j.operation,
		       j.backup_type, j.status, j.requested_by,
		       j.started_at, j.finished_at, j.error_message,
		       j.created_at, j.updated_at
		FROM backup_jobs j
		JOIN protected_resources r ON r.id = j.resource_id
		ORDER BY j.created_at DESC
		LIMIT 100
	`)
	if err != nil {
		internalError(w, err)
		return
	}
	defer rows.Close()

	items := make([]Job, 0)
	for rows.Next() {
		var job Job
		if err := rows.Scan(
			&job.ID, &job.ResourceKey, &job.Operation,
			&job.BackupType, &job.Status, &job.RequestedBy,
			&job.StartedAt, &job.FinishedAt, &job.Error,
			&job.CreatedAt, &job.UpdatedAt,
		); err != nil {
			internalError(w, err)
			return
		}
		items = append(items, job)
	}
	if err := rows.Err(); err != nil {
		internalError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (a *API) listAuditEvents(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Query(r.Context(), `
		SELECT id, event_type, actor, resource_id::text,
		       job_id::text, outcome, message, details, created_at
		FROM audit_events
		ORDER BY created_at DESC, id DESC
		LIMIT 100
	`)
	if err != nil {
		internalError(w, err)
		return
	}
	defer rows.Close()

	type AuditEvent struct {
		ID         int64           `json:"id"`
		EventType  string          `json:"event_type"`
		Actor      string          `json:"actor"`
		ResourceID *string         `json:"resource_id"`
		JobID      *string         `json:"job_id"`
		Outcome    string          `json:"outcome"`
		Message    string          `json:"message"`
		Details    json.RawMessage `json:"details"`
		CreatedAt  time.Time       `json:"created_at"`
	}

	items := make([]AuditEvent, 0)
	for rows.Next() {
		var event AuditEvent
		if err := rows.Scan(
			&event.ID, &event.EventType, &event.Actor,
			&event.ResourceID, &event.JobID, &event.Outcome,
			&event.Message, &event.Details, &event.CreatedAt,
		); err != nil {
			internalError(w, err)
			return
		}
		items = append(items, event)
	}
	if err := rows.Err(); err != nil {
		internalError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (a *API) listArtifacts(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Query(r.Context(), `
		SELECT a.id::text, r.resource_key, a.job_id::text,
		       j.operation, j.backup_type, a.storage_backend,
		       a.artifact_path, a.format, a.size_bytes, a.sha256,
		       a.verified_at, a.created_at
		FROM backup_artifacts a
		JOIN backup_jobs j ON j.id = a.job_id
		JOIN protected_resources r ON r.id = j.resource_id
		ORDER BY a.created_at DESC
		LIMIT 100
	`)
	if err != nil {
		internalError(w, err)
		return
	}
	defer rows.Close()

	type Artifact struct {
		ID          string     `json:"id"`
		ResourceKey string     `json:"resource_key"`
		JobID       string     `json:"job_id"`
		Operation   string     `json:"operation"`
		BackupType  string     `json:"backup_type"`
		Storage     string     `json:"storage_backend"`
		Path        string     `json:"path"`
		Format      string     `json:"format"`
		SizeBytes   int64      `json:"size_bytes"`
		SHA256      *string    `json:"sha256"`
		VerifiedAt  *time.Time `json:"verified_at,omitempty"`
		CreatedAt   time.Time  `json:"created_at"`
	}

	items := make([]Artifact, 0)
	for rows.Next() {
		var item Artifact
		if err := rows.Scan(
			&item.ID, &item.ResourceKey, &item.JobID,
			&item.Operation, &item.BackupType, &item.Storage,
			&item.Path, &item.Format, &item.SizeBytes, &item.SHA256,
			&item.VerifiedAt, &item.CreatedAt,
		); err != nil {
			internalError(w, err)
			return
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		internalError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (a *API) listRestoreTests(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Query(r.Context(), `
		SELECT t.id::text, r.resource_key, t.job_id::text,
		       t.source_artifact_id::text, t.test_database,
		       t.restored_table_count, t.restored_nonempty_table_count,
		       t.status, t.error_message,
		       t.started_at, t.finished_at, t.created_at
		FROM restore_test_results t
		JOIN backup_jobs j ON j.id = t.job_id
		JOIN protected_resources r ON r.id = j.resource_id
		ORDER BY t.finished_at DESC
		LIMIT 100
	`)
	if err != nil {
		internalError(w, err)
		return
	}
	defer rows.Close()

	type RestoreTest struct {
		ID                         string    `json:"id"`
		ResourceKey                string    `json:"resource_key"`
		JobID                      string    `json:"job_id"`
		SourceArtifactID           *string   `json:"source_artifact_id,omitempty"`
		TestDatabase               *string   `json:"test_database,omitempty"`
		RestoredTableCount         *int64    `json:"restored_table_count,omitempty"`
		RestoredNonemptyTableCount *int64    `json:"restored_nonempty_table_count,omitempty"`
		Status                     string    `json:"status"`
		Error                      *string   `json:"error_message,omitempty"`
		StartedAt                  time.Time `json:"started_at"`
		FinishedAt                 time.Time `json:"finished_at"`
		CreatedAt                  time.Time `json:"created_at"`
	}

	items := make([]RestoreTest, 0)
	for rows.Next() {
		var item RestoreTest
		if err := rows.Scan(
			&item.ID, &item.ResourceKey, &item.JobID,
			&item.SourceArtifactID, &item.TestDatabase,
			&item.RestoredTableCount, &item.RestoredNonemptyTableCount,
			&item.Status, &item.Error,
			&item.StartedAt, &item.FinishedAt, &item.CreatedAt,
		); err != nil {
			internalError(w, err)
			return
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		internalError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}
