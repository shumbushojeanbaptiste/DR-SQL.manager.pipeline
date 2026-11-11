package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Worker struct {
	db            *pgxpool.Pool
	resourceKey   string
	connectionRef string
	targetURL     string
	restoreURL    string
	backupRoot    string
}

type Job struct {
	ID            string
	ResourceID    string
	ResourceKey   string
	ConnectionRef string
	Operation     string
	BackupType    string
}

func required(name string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		log.Fatalf("%s is required", name)
	}
	return value
}

func main() {
	ctx := context.Background()

	metadataURL := required("DATABASE_URL")
	resourceKey := required("WORKER_RESOURCE_KEY")
	connectionRef := required("EXPECTED_CONNECTION_REF")
	targetURL := required("TARGET_DATABASE_URL")
	restoreURL := required("RESTORE_TEST_DATABASE_URL")

	backupRoot := os.Getenv("BACKUP_ROOT")
	if backupRoot == "" {
		backupRoot = "./backups"
	}

	if !strings.HasPrefix(connectionRef, "secret://") {
		log.Fatal("EXPECTED_CONNECTION_REF must be a secret:// reference")
	}

	pool, err := pgxpool.New(ctx, metadataURL)
	if err != nil {
		log.Fatalf("initialize metadata connection: %v", err)
	}
	defer pool.Close()

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if err := pool.Ping(pingCtx); err != nil {
		log.Fatalf("connect to metadata database: %v", err)
	}

	if _, err := exec.LookPath("pg_dump"); err != nil {
		log.Fatal("pg_dump is required on PATH")
	}

	if _, err := exec.LookPath("pg_restore"); err != nil {
		log.Fatal("pg_restore is required on PATH")
	}

	w := &Worker{
		db:            pool,
		resourceKey:   resourceKey,
		connectionRef: connectionRef,
		targetURL:     targetURL,
		restoreURL:    restoreURL,
		backupRoot:    backupRoot,
	}

	targetConfig, err := pgx.ParseConfig(targetURL)
	if err != nil {
		log.Fatal("TARGET_DATABASE_URL is invalid")
	}
	restoreConfig, err := pgx.ParseConfig(restoreURL)
	if err != nil {
		log.Fatal("RESTORE_TEST_DATABASE_URL is invalid")
	}
	if targetConfig.Host == restoreConfig.Host && targetConfig.Port == restoreConfig.Port {
		log.Fatal("RESTORE_TEST_DATABASE_URL must point to a separate PostgreSQL instance")
	}

	log.Printf("worker started for resource %q", resourceKey)

	fullInterval := scheduledInterval("FULL_BACKUP_INTERVAL_SECONDS", 5*time.Minute)
	differentialInterval := scheduledInterval("DIFFERENTIAL_BACKUP_INTERVAL_SECONDS", 2*time.Minute)
	var lastFull, lastDifferential time.Time

	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for {
		if err := w.processOne(ctx); err != nil {
			log.Printf("worker error: %v", err)
		}
		if err := w.scheduleBackups(ctx, "full", fullInterval, &lastFull); err != nil {
			log.Printf("full backup scheduling error: %v", err)
		}
		if err := w.scheduleBackups(ctx, "differential", differentialInterval, &lastDifferential); err != nil {
			log.Printf("differential backup scheduling error: %v", err)
		}

		<-ticker.C
	}
}

func scheduledInterval(name string, fallback time.Duration) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name)))
	if err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	return fallback
}

func (w *Worker) scheduleBackups(ctx context.Context, backupType string, interval time.Duration, lastScheduled *time.Time) error {
	if !lastScheduled.IsZero() && time.Since(*lastScheduled) < interval {
		return nil
	}

	var active int
	if err := w.db.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM backup_jobs j
		JOIN protected_resources r ON r.id = j.resource_id
		WHERE r.resource_key = $1
		  AND j.operation = 'backup'
		  AND j.backup_type = $2
		  AND j.status IN ('queued', 'running')
	`, w.resourceKey, backupType).Scan(&active); err != nil {
		return err
	}
	if active > 0 {
		return nil
	}

	_, err := w.db.Exec(ctx, `
		INSERT INTO backup_jobs (resource_id, operation, backup_type, status, requested_by)
		SELECT id, 'backup', $2, 'queued', 'automatic-scheduler'
		FROM protected_resources
		WHERE resource_key = $1 AND enabled = TRUE
	`, w.resourceKey, backupType)
	if err != nil {
		return err
	}
	*lastScheduled = time.Now()
	log.Printf("scheduled %s backup for resource=%s", backupType, w.resourceKey)
	return nil
}

func (w *Worker) processOne(ctx context.Context) error {
	tx, err := w.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var job Job

	err = tx.QueryRow(ctx, `
		SELECT j.id::text, r.id::text, r.resource_key,
		       r.connection_ref, j.operation, j.backup_type
		FROM backup_jobs j
		JOIN protected_resources r ON r.id = j.resource_id
		WHERE j.status = 'queued'
		  AND r.enabled = TRUE
		  AND r.resource_key = $1
		ORDER BY j.created_at
		FOR UPDATE OF j SKIP LOCKED
		LIMIT 1
	`, w.resourceKey).Scan(
		&job.ID,
		&job.ResourceID,
		&job.ResourceKey,
		&job.ConnectionRef,
		&job.Operation,
		&job.BackupType,
	)

	if err == pgx.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}

	if job.ConnectionRef != w.connectionRef {
		return fmt.Errorf(
			"configured secret reference does not match registered resource %q",
			w.resourceKey,
		)
	}

	tag, err := tx.Exec(ctx, `
		UPDATE backup_jobs
		SET status = 'running',
		    started_at = NOW(),
		    updated_at = NOW(),
		    error_message = NULL
		WHERE id = $1 AND status = 'queued'
	`, job.ID)
	if err != nil {
		return err
	}

	if tag.RowsAffected() != 1 {
		return fmt.Errorf("job %s was not claimed", job.ID)
	}

	if err := tx.Commit(ctx); err != nil {
		return err
	}

	if job.Operation == "restore_test" {
		if err := w.restoreTest(ctx, job); err != nil {
			return w.finishFailure(ctx, job, err.Error())
		}
		return nil
	}
	if job.Operation != "backup" {
		return w.finishFailure(
			ctx,
			job,
			"unsupported operation",
		)
	}

	if job.BackupType != "logical" && job.BackupType != "full" && job.BackupType != "differential" {
		return w.finishFailure(ctx, job, "unsupported backup type")
	}

	if err := w.backup(ctx, job); err != nil {
		return w.finishFailure(ctx, job, err.Error())
	}
	return nil
}

func (w *Worker) backup(ctx context.Context, job Job) error {
	started := time.Now()

	dir := filepath.Join(w.backupRoot, job.ResourceKey)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return w.finishFailure(ctx, job, err.Error())
	}

	finalPath := filepath.Join(dir, job.ID+".dump")
	tempPath := finalPath + ".partial"

	defer os.Remove(tempPath)

	dsn, err := pgx.ParseConfig(w.targetURL)
	if err != nil {
		return w.finishFailure(ctx, job, "invalid target database URL")
	}

	env, err := pgEnvironment(w.targetURL, dsn.Database)
	if err != nil {
		return w.finishFailure(ctx, job, "invalid target database URL")
	}

	cmdCtx, cancel := context.WithTimeout(ctx, 2*time.Hour)
	defer cancel()

	cmd := exec.CommandContext(
		cmdCtx,
		"pg_dump",
		"--format=custom",
		"--no-owner",
		"--no-acl",
		"--file", tempPath,
	)
	cmd.Env = env

	output, err := cmd.CombinedOutput()
	if err != nil {
		return w.finishFailure(
			ctx,
			job,
			fmt.Sprintf("pg_dump failed: %s", safeOutput(output)),
		)
	}

	info, err := os.Stat(tempPath)
	if err != nil {
		return w.finishFailure(ctx, job, err.Error())
	}

	if info.Size() == 0 {
		return w.finishFailure(ctx, job, "pg_dump created an empty artifact")
	}

	verifyCtx, verifyCancel := context.WithTimeout(ctx, 2*time.Minute)
	defer verifyCancel()

	verify := exec.CommandContext(
		verifyCtx,
		"pg_restore",
		"--list",
		tempPath,
	)

	verifyOutput, err := verify.CombinedOutput()
	if err != nil {
		return w.finishFailure(
			ctx,
			job,
			fmt.Sprintf("archive verification failed: %s", safeOutput(verifyOutput)),
		)
	}

	file, err := os.Open(tempPath)
	if err != nil {
		return w.finishFailure(ctx, job, err.Error())
	}

	hash := sha256.New()
	_, copyErr := io.Copy(hash, file)
	closeErr := file.Close()

	if copyErr != nil {
		return w.finishFailure(ctx, job, copyErr.Error())
	}
	if closeErr != nil {
		return w.finishFailure(ctx, job, closeErr.Error())
	}

	checksum := hex.EncodeToString(hash.Sum(nil))

	if err := os.Rename(tempPath, finalPath); err != nil {
		return w.finishFailure(ctx, job, err.Error())
	}

	tx, err := w.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	_, err = tx.Exec(ctx, `
		INSERT INTO backup_artifacts (
			job_id,
			storage_backend,
			artifact_path,
			format,
			size_bytes,
			sha256,
			verified_at
		)
		VALUES ($1, 'local', $2, 'pg_dump_custom', $3, $4, NOW())
	`, job.ID, finalPath, info.Size(), checksum)
	if err != nil {
		return err
	}

	_, err = tx.Exec(ctx, `
		UPDATE backup_jobs
		SET status = 'succeeded',
		    finished_at = NOW(),
		    updated_at = NOW()
		WHERE id = $1 AND status = 'running'
	`, job.ID)
	if err != nil {
		return err
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO audit_events (
			event_type,
			actor,
			resource_id,
			job_id,
			outcome,
			message,
			details
		)
		VALUES (
			'backup.completed',
			'worker:' || $2,
			$3,
			$1,
			'succeeded',
			'Logical backup created and archive verified',
			jsonb_build_object(
				'path', $4::text,
				'size_bytes', $5::bigint,
				'sha256', $6::text,
				'duration_ms', $7::bigint
			)
		)
	`,
		job.ID,
		job.ResourceKey,
		job.ResourceID,
		finalPath,
		info.Size(),
		checksum,
		time.Since(started).Milliseconds(),
	)
	if err != nil {
		return err
	}

	// Automatically verify every logical/full/differential snapshot after it is committed.
	_, err = tx.Exec(ctx, `
		INSERT INTO backup_jobs (resource_id, operation, backup_type, status, requested_by)
		VALUES ($1, 'restore_test', $2, 'queued', 'automatic-restore-test')
	`, job.ResourceID, job.BackupType)
	if err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return err
	}

	log.Printf(
		"backup succeeded: resource=%s job=%s bytes=%d sha256=%s",
		job.ResourceKey,
		job.ID,
		info.Size(),
		checksum,
	)

	return nil
}

func (w *Worker) restoreTest(ctx context.Context, job Job) error {
	started := time.Now()

	var artifactID, artifactPath, checksum string
	err := w.db.QueryRow(ctx, `
		SELECT a.id::text, a.artifact_path, a.sha256
		FROM backup_artifacts a
		JOIN backup_jobs b ON b.id = a.job_id
		WHERE b.resource_id = $1
		  AND b.operation = 'backup'
		  AND b.backup_type = $2
		  AND b.status = 'succeeded'
		  AND a.verified_at IS NOT NULL
		ORDER BY b.finished_at DESC
		LIMIT 1
	`, job.ResourceID, job.BackupType).Scan(&artifactID, &artifactPath, &checksum)
	if errors.Is(err, pgx.ErrNoRows) {
		return w.finishFailure(ctx, job, "no verified backup is available to restore")
	}
	if err != nil {
		return w.finishFailure(ctx, job, fmt.Sprintf("load backup artifact for restore test: %v", err))
	}

	tableCount, nonemptyTableCount, testDatabase, err :=
		w.runRestoreTest(ctx, job, artifactPath, checksum)
	if err != nil {
		return w.finishFailure(ctx, job, err.Error())
	}

	tx, err := w.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	_, err = tx.Exec(ctx, `
		INSERT INTO restore_test_results (
			job_id, source_artifact_id, test_database,
			restored_table_count, restored_nonempty_table_count,
			status, started_at, finished_at
		)
		VALUES ($1, $2, $3, $4, $5, 'succeeded', $6, NOW())
	`, job.ID, artifactID, testDatabase, tableCount, nonemptyTableCount, started)
	if err != nil {
		return err
	}

	_, err = tx.Exec(ctx, `
		UPDATE backup_jobs
		SET status = 'succeeded', finished_at = NOW(), updated_at = NOW()
		WHERE id = $1 AND status = 'running'
	`, job.ID)
	if err != nil {
		return err
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO audit_events (
			event_type, actor, resource_id, job_id, outcome, message, details
		)
		VALUES (
			'restore_test.completed', 'worker:' || $2, $3, $1, 'succeeded',
			'Logical backup restored and verified in an isolated database',
			jsonb_build_object(
				'source_artifact_id', $4::text,
				'restored_table_count', $5::bigint,
				'restored_nonempty_table_count', $6::bigint,
				'duration_ms', $7::bigint
			)
		)
	`, job.ID, job.ResourceKey, job.ResourceID, artifactID, tableCount,
		nonemptyTableCount, time.Since(started).Milliseconds())
	if err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return err
	}

	log.Printf("restore test succeeded: resource=%s job=%s tables=%d",
		job.ResourceKey, job.ID, tableCount)
	return nil
}

func (w *Worker) runRestoreTest(
	ctx context.Context,
	job Job,
	artifactPath string,
	expectedChecksum string,
) (
	tableCount int64,
	nonemptyTableCount int64,
	testDatabase string,
	resultErr error,
) {
	root, err := filepath.Abs(filepath.Join(w.backupRoot, job.ResourceKey))
	if err != nil {
		return 0, 0, "", fmt.Errorf("resolve backup directory: %w", err)
	}
	resolvedPath, err := filepath.Abs(artifactPath)
	if err != nil {
		return 0, 0, "", fmt.Errorf("resolve artifact path: %w", err)
	}
	relativePath, err := filepath.Rel(root, resolvedPath)
	if err != nil || relativePath == ".." ||
		strings.HasPrefix(relativePath, ".."+string(os.PathSeparator)) {
		return 0, 0, "", fmt.Errorf("backup artifact is outside the worker backup directory")
	}

	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return 0, 0, "", fmt.Errorf("resolve backup directory: %w", err)
	}
	resolvedPath, err = filepath.EvalSymlinks(resolvedPath)
	if err != nil {
		return 0, 0, "", fmt.Errorf("resolve backup artifact: %w", err)
	}
	relativePath, err = filepath.Rel(root, resolvedPath)
	if err != nil || relativePath == ".." ||
		strings.HasPrefix(relativePath, ".."+string(os.PathSeparator)) {
		return 0, 0, "", fmt.Errorf("backup artifact is outside the worker backup directory")
	}

	info, err := os.Stat(resolvedPath)
	if err != nil {
		return 0, 0, "", fmt.Errorf("stat backup artifact: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return 0, 0, "", fmt.Errorf("backup artifact is not a nonempty regular file")
	}

	file, err := os.Open(resolvedPath)
	if err != nil {
		return 0, 0, "", fmt.Errorf("open backup artifact: %w", err)
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, file)
	closeErr := file.Close()
	if copyErr != nil {
		return 0, 0, "", fmt.Errorf("checksum backup artifact: %w", copyErr)
	}
	if closeErr != nil {
		return 0, 0, "", fmt.Errorf("close backup artifact: %w", closeErr)
	}
	if hex.EncodeToString(hash.Sum(nil)) != expectedChecksum {
		return 0, 0, "", fmt.Errorf("backup artifact checksum does not match its audit record")
	}

	adminConfig, err := pgx.ParseConfig(w.restoreURL)
	if err != nil {
		return 0, 0, "", fmt.Errorf("invalid restore test database URL")
	}
	adminConn, err := pgx.ConnectConfig(ctx, adminConfig)
	if err != nil {
		return 0, 0, "", fmt.Errorf("connect to isolated restore test PostgreSQL instance: %w", err)
	}
	defer adminConn.Close(context.Background())

	databaseName := "dr_restore_" + strings.ReplaceAll(job.ID, "-", "")
	if _, err := adminConn.Exec(ctx,
		"CREATE DATABASE "+pgx.Identifier{databaseName}.Sanitize()); err != nil {
		return 0, 0, "", fmt.Errorf("create isolated restore test database: %w", err)
	}
	databaseCreated := true
	defer func() {
		if !databaseCreated {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, cleanupErr := adminConn.Exec(cleanupCtx,
			"DROP DATABASE IF EXISTS "+pgx.Identifier{databaseName}.Sanitize()+" WITH (FORCE)")
		if cleanupErr != nil {
			if resultErr != nil {
				resultErr = fmt.Errorf("%v; remove temporary restore database: %w", resultErr, cleanupErr)
			} else {
				resultErr = fmt.Errorf("remove temporary restore database: %w", cleanupErr)
			}
		}
	}()

	restoreCtx, cancel := context.WithTimeout(ctx, 2*time.Hour)
	defer cancel()
	command := exec.CommandContext(
		restoreCtx,
		"pg_restore",
		"--exit-on-error",
		"--no-owner",
		"--no-acl",
		"--dbname", databaseName,
		resolvedPath,
	)
	command.Env, err = pgEnvironment(w.restoreURL, databaseName)
	if err != nil {
		return 0, 0, "", fmt.Errorf("invalid restore test database URL")
	}
	output, err := command.CombinedOutput()
	if err != nil {
		return 0, 0, "", fmt.Errorf("pg_restore failed: %s", safeOutput(output))
	}

	testConfig, err := pgx.ParseConfig(w.restoreURL)
	if err != nil {
		return 0, 0, "", fmt.Errorf("invalid restore test database URL")
	}
	testConfig.Database = databaseName
	testConn, err := pgx.ConnectConfig(restoreCtx, testConfig)
	if err != nil {
		return 0, 0, "", fmt.Errorf("connect to restored test database: %w", err)
	}
	rows, err := testConn.Query(restoreCtx, `
		SELECT n.nspname, c.relname
		FROM pg_catalog.pg_class c
		JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind IN ('r', 'p')
		  AND n.nspname <> 'information_schema'
		  AND n.nspname NOT LIKE 'pg_%'
	`)
	if err != nil {
		_ = testConn.Close(restoreCtx)
		return 0, 0, "", fmt.Errorf("list restored user tables: %w", err)
	}
	type tableName struct {
		schema string
		name   string
	}
	tables := make([]tableName, 0)
	for rows.Next() {
		var table tableName
		if err := rows.Scan(&table.schema, &table.name); err != nil {
			rows.Close()
			_ = testConn.Close(restoreCtx)
			return 0, 0, "", fmt.Errorf("read restored table list: %w", err)
		}
		tables = append(tables, table)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		_ = testConn.Close(restoreCtx)
		return 0, 0, "", fmt.Errorf("read restored table list: %w", err)
	}
	rows.Close()
	tableCount = int64(len(tables))
	for _, table := range tables {
		var hasRows bool
		query := "SELECT EXISTS (SELECT 1 FROM " +
			pgx.Identifier{table.schema, table.name}.Sanitize() + " LIMIT 1)"
		if err := testConn.QueryRow(restoreCtx, query).Scan(&hasRows); err != nil {
			_ = testConn.Close(restoreCtx)
			return 0, 0, "", fmt.Errorf("verify restored table %s.%s: %w",
				table.schema, table.name, err)
		}
		if hasRows {
			nonemptyTableCount++
		}
	}
	closeTestErr := testConn.Close(restoreCtx)
	if closeTestErr != nil {
		return 0, 0, "", fmt.Errorf("close restored test database connection: %w", closeTestErr)
	}
	if tableCount == 0 {
		return 0, 0, "", fmt.Errorf("restored database contains no user tables")
	}
	if nonemptyTableCount == 0 {
		return 0, 0, "", fmt.Errorf("restored database contains no table rows")
	}

	if _, err := adminConn.Exec(ctx,
		"DROP DATABASE "+pgx.Identifier{databaseName}.Sanitize()+" WITH (FORCE)"); err != nil {
		return 0, 0, "", fmt.Errorf("drop temporary restore test database: %w", err)
	}
	databaseCreated = false

	return tableCount, nonemptyTableCount, databaseName, nil
}

func pgEnvironment(databaseURL, database string) ([]string, error) {
	config, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		return nil, err
	}
	env := make([]string, 0, len(os.Environ())+7)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(key, "PG") {
			env = append(env, entry)
		}
	}
	return append(env,
		"PGHOST="+config.Host,
		"PGPORT="+fmt.Sprint(config.Port),
		"PGUSER="+config.User,
		"PGDATABASE="+database,
		"PGPASSWORD="+config.Password,
		"PGSSLMODE="+sslMode(databaseURL),
		"PGCONNECT_TIMEOUT=10",
	), nil
}

func (w *Worker) finishFailure(
	ctx context.Context,
	job Job,
	message string,
) error {
	if len(message) > 2000 {
		message = message[:2000]
	}

	tx, err := w.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	_, err = tx.Exec(ctx, `
		UPDATE backup_jobs
		SET status = 'failed',
		    error_message = $2,
		    finished_at = NOW(),
		    updated_at = NOW()
		WHERE id = $1 AND status = 'running'
	`, job.ID, message)
	if err != nil {
		return err
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO audit_events (
			event_type,
			actor,
			resource_id,
			job_id,
			outcome,
			message,
			details
		)
		VALUES (
			CASE WHEN $5 = 'restore_test' THEN 'restore_test.failed'
			     ELSE 'backup.failed' END,
			'worker:' || $2,
			$3,
			$1,
			'failed',
			CASE WHEN $5 = 'restore_test' THEN 'Restore test failed'
			     ELSE 'Backup job failed' END,
			jsonb_build_object('error', $4::text)
		)
	`, job.ID, job.ResourceKey, job.ResourceID, message, job.Operation)
	if err != nil {
		return err
	}

	if job.Operation == "restore_test" {
		_, err = tx.Exec(ctx, `
			INSERT INTO restore_test_results (
				job_id, status, error_message, started_at, finished_at
			)
			VALUES ($1, 'failed', $2, COALESCE(
				(SELECT started_at FROM backup_jobs WHERE id = $1), NOW()
			), NOW())
		`, job.ID, message)
		if err != nil {
			return err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return err
	}

	log.Printf(
		"job failed: resource=%s job=%s error=%s",
		job.ResourceKey,
		job.ID,
		message,
	)

	return nil
}

func sslMode(databaseURL string) string {
	parsed, err := url.Parse(databaseURL)
	if err == nil && parsed.Query().Get("sslmode") != "" {
		return parsed.Query().Get("sslmode")
	}
	return "prefer"
}

func safeOutput(output []byte) string {
	value := strings.TrimSpace(string(output))
	if len(value) > 1500 {
		value = value[:1500]
	}
	return value
}
