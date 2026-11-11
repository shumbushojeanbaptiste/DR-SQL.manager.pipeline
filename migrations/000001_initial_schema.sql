
CREATE TABLE IF NOT EXISTS protected_resources (
    id UUID PRIMARY KEY,
    resource_key TEXT NOT NULL UNIQUE,
    display_name TEXT NOT NULL,
    resource_type TEXT NOT NULL DEFAULT 'postgresql',
    environment TEXT NOT NULL DEFAULT 'production',
    connection_ref TEXT NOT NULL,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT protected_resources_type_check
        CHECK (resource_type IN ('postgresql')),

    CONSTRAINT protected_resources_environment_check
        CHECK (environment IN ('development', 'staging', 'production'))
);

CREATE TABLE IF NOT EXISTS recovery_policies (
    resource_id UUID PRIMARY KEY
        REFERENCES protected_resources(id) ON DELETE RESTRICT,
    rpo_seconds INTEGER NOT NULL,
    rto_seconds INTEGER NOT NULL,
    retention_days INTEGER NOT NULL,
    full_backup_cron TEXT NOT NULL,
    differential_backup_cron TEXT NOT NULL,
    wal_archiving_required BOOLEAN NOT NULL DEFAULT TRUE,
    restore_test_cron TEXT NOT NULL,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT recovery_policy_positive_values
        CHECK (
            rpo_seconds > 0
            AND rto_seconds > 0
            AND retention_days > 0
        )
);

CREATE INDEX IF NOT EXISTS idx_protected_resources_enabled
    ON protected_resources (enabled);

CREATE INDEX IF NOT EXISTS idx_recovery_policies_enabled
    ON recovery_policies (enabled);


ALTER TABLE protected_resources
    ALTER COLUMN id SET DEFAULT gen_random_uuid();
