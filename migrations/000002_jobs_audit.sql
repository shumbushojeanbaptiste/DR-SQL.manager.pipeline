
CREATE TABLE IF NOT EXISTS backup_jobs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    resource_id UUID NOT NULL
        REFERENCES protected_resources(id) ON DELETE RESTRICT,
    operation TEXT NOT NULL
        CHECK (operation IN ('backup', 'restore_test')),
    backup_type TEXT NOT NULL
        CHECK (backup_type IN ('logical', 'full', 'differential')),
    status TEXT NOT NULL DEFAULT 'queued'
        CHECK (status IN (
            'queued', 'running', 'succeeded', 'failed', 'cancelled'
        )),
    requested_by TEXT NOT NULL DEFAULT 'local-development',
    started_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    error_message TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT backup_job_time_order CHECK (
        finished_at IS NULL OR started_at IS NULL OR finished_at >= started_at
    )
);

CREATE TABLE IF NOT EXISTS backup_artifacts (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    job_id UUID NOT NULL REFERENCES backup_jobs(id) ON DELETE RESTRICT,
    storage_backend TEXT NOT NULL DEFAULT 'local',
    artifact_path TEXT NOT NULL,
    format TEXT NOT NULL,
    size_bytes BIGINT NOT NULL CHECK (size_bytes >= 0),
    sha256 TEXT,
    verified_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS audit_events (
    id BIGSERIAL PRIMARY KEY,
    event_type TEXT NOT NULL,
    actor TEXT NOT NULL,
    resource_id UUID REFERENCES protected_resources(id) ON DELETE RESTRICT,
    job_id UUID REFERENCES backup_jobs(id) ON DELETE RESTRICT,
    outcome TEXT NOT NULL
        CHECK (outcome IN ('accepted', 'succeeded', 'failed', 'rejected')),
    message TEXT NOT NULL,
    details JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_backup_jobs_resource_created
    ON backup_jobs (resource_id, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_backup_jobs_status_created
    ON backup_jobs (status, created_at);

CREATE INDEX IF NOT EXISTS idx_backup_artifacts_job
    ON backup_artifacts (job_id);

CREATE INDEX IF NOT EXISTS idx_audit_events_created
    ON audit_events (created_at DESC);

CREATE INDEX IF NOT EXISTS idx_audit_events_resource_created
    ON audit_events (resource_id, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_audit_events_type_created
    ON audit_events (event_type, created_at DESC);
