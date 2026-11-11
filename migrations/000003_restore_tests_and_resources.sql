CREATE TABLE IF NOT EXISTS restore_test_results (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    job_id UUID NOT NULL UNIQUE
        REFERENCES backup_jobs(id) ON DELETE RESTRICT,
    source_artifact_id UUID
        REFERENCES backup_artifacts(id) ON DELETE RESTRICT,
    test_database TEXT,
    restored_table_count BIGINT,
    restored_nonempty_table_count BIGINT,
    status TEXT NOT NULL CHECK (status IN ('succeeded', 'failed')),
    error_message TEXT,
    started_at TIMESTAMPTZ NOT NULL,
    finished_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT restore_test_result_values CHECK (
        (status = 'succeeded'
            AND source_artifact_id IS NOT NULL
            AND test_database IS NOT NULL
            AND restored_table_count > 0
            AND restored_nonempty_table_count > 0
            AND error_message IS NULL)
        OR
        (status = 'failed' AND error_message IS NOT NULL)
    )
);

CREATE INDEX IF NOT EXISTS idx_restore_test_results_finished
    ON restore_test_results (finished_at DESC);

INSERT INTO protected_resources (
    resource_key, display_name, resource_type, environment, connection_ref
)
VALUES
    ('identity-production', 'Identity PostgreSQL', 'postgresql', 'production',
     'secret://identity/production'),
    ('messaging-production', 'Messaging PostgreSQL', 'postgresql', 'production',
     'secret://messaging/production'),
    ('billing-production', 'Billing PostgreSQL', 'postgresql', 'production',
     'secret://billing/production')
ON CONFLICT (resource_key) DO NOTHING;
