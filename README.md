# Central SMS DR Manager

Central SMS DR Manager protects three PostgreSQL databases:

- `central_sms_identity`
- `central_sms`
- `central_sms_billing`

The application runs three backup workers, one API, the DR metadata database, and an isolated restore-test database. Backups are custom-format PostgreSQL logical dumps. Full and differential backup requests are supported in the current implementation, but both are currently created as logical `pg_dump` snapshots; they are not PostgreSQL physical backups or WAL-based incremental backups.

## 1. Repository layout

```text
central-sms-dr-manager/
├── cmd/api/                     API service
├── cmd/worker/                  Backup workers
├── scripts/                     Metadata database migration tool
├── migrations/                  Metadata schema migrations
├── backups/                     Generated backup files
├── initial_db/                  PostgreSQL custom-format source dumps
├── docker-compose.yml           Local development stack
├── docker-compose.realdatabase.yml
├── import_real_database.sh      Import source dumps into the real database container
├── deployments/docker/          Production Compose and startup script
├── .github/workflows/ci.yml     Build and Docker Hub publishing
├── .env                         Local/production configuration (keep secret)
├── .env.example                 Configuration template
└── postman_json/all_endpoint.json
```

## 2. Prerequisites

Install:

- Docker Engine with Docker Compose
- Bash
- PostgreSQL 18 client tools for local manual verification, if needed
- A Docker Hub account for production image publishing

Check Docker before continuing:

```sh
docker --version
docker compose version
```

## 3. Prepare the environment file

Use `.env.example` as the template. Keep `.env` out of Git and never expose database passwords in source code, commits, or tickets.

```sh
cp .env.example .env
```

Configure these development values:

```dotenv
HTTP_ADDR=:8080

DR_METADATA_PASSWORD=<metadata-postgresql-password>
DATABASE_URL=postgres://dr_manager:<metadata-password>@127.0.0.1:55432/dr_manager?sslmode=disable

IDENTITY_DATABASE_URL=postgres://real_platform:<real-platform-password>@127.0.0.1:56432/central_sms_identity?sslmode=disable
MESSAGING_DATABASE_URL=postgres://real_platform:<real-platform-password>@127.0.0.1:56432/central_sms?sslmode=disable
BILLING_DATABASE_URL=postgres://real_platform:<real-platform-password>@127.0.0.1:56432/central_sms_billing?sslmode=disable

RESTORE_TEST_DB_PASSWORD=<restore-test-password>
RESTORE_TEST_DATABASE_URL=postgres://dr_restore:<restore-test-password>@127.0.0.1:55433/postgres?sslmode=disable

REAL_DATABASE_PASSWORD=<postgres-admin-password>
REAL_DATABASE_PORT=56432
POSTGRES_CLIENT_VERSION=18

FULL_BACKUP_INTERVAL_SECONDS=300
DIFFERENTIAL_BACKUP_INTERVAL_SECONDS=120

DATA_BACKUPS=./backups
```

URL-encode special characters in PostgreSQL passwords. For example, replace `#` with `%23`, `@` with `%40`, and `:` with `%3A`.

For a production PostgreSQL server reached by Docker containers, use the appropriate production host rather than the container's loopback address. Host networking is used by workers in the provided Compose files, so `127.0.0.1` normally refers to the Docker host.

## 4. Start the metadata database

The repository's metadata database can be started with:

```sh
docker compose -f .docker.compose.postgres up -d
```

Confirm it is running and healthy:

```sh
docker ps --filter name=central-sms-dr-metadata-db
docker exec central-sms-dr-metadata-db pg_isready -U dr_manager -d dr_manager
```

The existing metadata database must not be recreated during ordinary application updates because it stores jobs, artifacts, restore-test records, and audit logs.

## 5. Start the local real application database

The local source database container is defined in `docker-compose.realdatabase.yml`. Import the dumps in `initial_db/` with:

```sh
chmod +x import_real_database.sh
./import_real_database.sh
```

The script:

1. Starts the real database container.
2. Waits for PostgreSQL to become ready.
3. Removes and recreates each target database.
4. Imports `central_sms_identity`, `central_sms`, and `central_sms_billing` from `initial_db/`.
5. Creates or updates the backup login.
6. Grants read access and backup table locks on existing tables and future tables.

Confirm the container and databases:

```sh
docker ps --filter name=central-sms-real-database
for db in central_sms_identity central_sms central_sms_billing; do
  docker exec central-sms-real-database psql -U postgres -d "$db" -c 'SELECT current_database(), count(*) FROM pg_tables WHERE schemaname = '''public''';'
done
```

Test backup credentials:

```sh
for db in central_sms_identity central_sms central_sms_billing; do
  PGPASSWORD='<real-platform-password>' psql     "postgres://real_platform@127.0.0.1:56432/$db?sslmode=disable"     -c 'SELECT current_database();'
done
```

If source dumps change, rerun the import script. It recreates the three databases from the dumps.

## 6. Build and start the DR manager locally

Start the migration, API, restore-test database, and workers:

```sh
docker compose up --build -d
```

Docker Compose runs migrations before starting API and workers. Confirm the containers:

```sh
docker compose ps
```

Check application health:

```sh
curl -i http://127.0.0.1:8080/health/live
curl -i http://127.0.0.1:8080/health/ready
```

Expected live response:

```json
{"status":"alive"}
```

Expected ready response:

```json
{"database":"connected","status":"ready"}
```

## 7. Automatic backups

Each worker creates and automatically tests backups according to these defaults:

- Full snapshot: every 300 seconds.
- Differential snapshot: every 120 seconds.

A successful backup is followed by an automatically queued restore test. The restore-test job uses the same backup type where possible:

- `full` backup → `full` restore test
- `differential` backup → `differential` restore test

Because both scheduled types are implemented with custom-format `pg_dump`, each restore test uses the same verified logical snapshot format.

## 8. Request backups manually

Identity full backup:

```sh
curl -X POST http://127.0.0.1:8080/api/v1/resources/identity-production/jobs \
  -H 'Content-Type: application/json' \
  -d '{"operation":"backup","backup_type":"full"}'
```

Identity differential backup:

```sh
curl -X POST http://127.0.0.1:8080/api/v1/resources/identity-production/jobs \
  -H 'Content-Type: application/json' \
  -d '{"operation":"backup","backup_type":"differential"}'
```

Messaging logical backup:

```sh
curl -X POST http://127.0.0.1:8080/api/v1/resources/messaging-production/jobs \
  -H 'Content-Type: application/json' \
  -d '{"operation":"backup","backup_type":"logical"}'
```

## 9. Request or inspect restore tests

Restore the newest verified backup of the requested backup type:

```sh
curl -X POST http://127.0.0.1:8080/api/v1/resources/identity-production/jobs \
  -H 'Content-Type: application/json' \
  -d '{"operation":"restore_test","backup_type":"full"}'
```

The restore test:

1. Verifies the SHA-256 checksum.
2. Creates a unique temporary database in the isolated restore-test container.
3. Restores the custom-format archive with `pg_restore --exit-on-error`.
4. Counts restored tables and tables containing rows.
5. Records success or failure.
6. Drops the temporary database.

## 10. Inspect jobs, files, and logs

```sh
curl http://127.0.0.1:8080/api/v1/jobs
curl http://127.0.0.1:8080/api/v1/artifacts
curl http://127.0.0.1:8080/api/v1/restore-tests
curl http://127.0.0.1:8080/api/v1/audit-events
```

Backup files are stored under:

```text
./backups/<resource-key>/<job-id>.dump
```

The artifact API exposes the storage path, size, SHA-256 checksum, and verification time. Backups contain sensitive data. Protect the directory with appropriate filesystem permissions and use encrypted off-host replication.

## 11. Import the Postman collection

1. Open Postman.
2. Select **Import**.
3. Choose `postman_json/all_endpoint.json`.
4. Confirm the collection variable:

```text
baseUrl = http://127.0.0.1:8080
```

5. Run **Health → Liveness**, then **Health → Readiness**.
6. Run **Jobs → Create full backup job**.
7. Inspect **Jobs → List jobs** and **Results → List restore tests**.

## 12. Update application code locally

```sh
go test ./...
docker compose build
docker compose up -d --force-recreate api worker-identity worker-messaging worker-billing
```

Schema changes use migration files in `migrations/`. Add new migrations with increasing numeric prefixes instead of editing migrations that have already run.

## 13. Publish to Docker Hub

The GitHub workflow `.github/workflows/ci.yml` builds images when commits are pushed to `main` or when the workflow is run manually.

In GitHub repository settings, create Actions secret:

```text
Name:  DOCKERHUB_TOKEN
Value: <Docker Hub access token>
```

Docker Hub username is:

```text
0788644687
```

The workflow publishes:

```text
0788644687/central-sms-dr-manager:<git-commit-sha>
0788644687/central-sms-dr-manager:latest
```

Login locally, when needed:

```sh
docker login -u 0788644687
docker pull 0788644687/central-sms-dr-manager:latest
```

Do not write the Docker Hub password or token into `.env` or GitHub Actions source.

## 14. Deploy to production

Copy the production configuration to the server, set real values, and then verify required variables:

```sh
cp .env.example .env
chmod +x deployments/docker/start_production.sh
```

Required production settings include:

```dotenv
DR_METADATA_PASSWORD=<production-metadata-password>
DATABASE_URL=<production-metadata-database-url>
IDENTITY_DATABASE_URL=<production-identity-database-url>
MESSAGING_DATABASE_URL=<production-messaging-database-url>
BILLING_DATABASE_URL=<production-billing-database-url>
RESTORE_TEST_DB_PASSWORD=<production-restore-test-password>
RESTORE_TEST_DATABASE_URL=<production-restore-test-database-url>
POSTGRES_CLIENT_VERSION=18
```

Start or update production:

```sh
./deployments/docker/start_production.sh
```

To deploy one immutable commit build:

```sh
IMAGE_TAG=<git-commit-sha> ./deployments/docker/start_production.sh
```

The production script:

1. Loads `.env`.
2. Pulls the configured application image.
3. Starts the restore-test database when required.
4. Runs all metadata migrations.
5. Stops deployment only if migration fails.
6. Recreates and starts the API and workers after migration succeeds.
7. Keeps the metadata database and backup volume persistent.

The production script uses `deployments/docker/docker-compose.production.yml`. Confirm containers:

```sh
docker compose -f deployments/docker/docker-compose.production.yml --env-file .env ps
```

Check production health:

```sh
curl -i http://127.0.0.1:8080/health/live
curl -i http://127.0.0.1:8080/health/ready
curl http://127.0.0.1:8080/api/v1/jobs
curl http://127.0.0.1:8080/api/v1/restore-tests
curl http://127.0.0.1:8080/api/v1/audit-events
```

The production API should not be exposed directly to an untrusted network until authentication and TLS are added. Keep port 8080 bound to localhost or place it behind an authenticated reverse proxy.

## 15. Rolling upgrade procedure

Before deployment:

```sh
docker compose -f deployments/docker/docker-compose.production.yml --env-file .env pull
```

Run migrations:

```sh
IMAGE_TAG=<git-commit-sha> ./deployments/docker/start_production.sh
```

This startup order preserves metadata records and applies new schemas before API and worker containers are recreated. A safe application migration must be backward-compatible with the previous workers while it executes.

Back up the metadata database before major schema upgrades:

```sh
pg_dump "$DATABASE_URL" --format=custom --file=dr-manager-before-update.dump
```

## 16. Troubleshooting

### API does not respond

```sh
docker compose logs --tail=100 api
curl -v http://127.0.0.1:8080/health/live
```

In Compose, the API must bind to `:8080`, while Docker publishes only the host address configured by `HTTP_PORT`.

### Backup permission denied

Run the permission-grant section again through `import_real_database.sh`, or grant read access manually to every application table in PostgreSQL:

```sql
GRANT USAGE ON SCHEMA public TO real_platform;
GRANT SELECT ON ALL TABLES IN SCHEMA public TO real_platform;
GRANT SELECT ON ALL SEQUENCES IN SCHEMA public TO real_platform;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT ON TABLES TO real_platform;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT ON SEQUENCES TO real_platform;
```

The role also needs `USAGE` on schemas containing backed-up objects.

### Restore test cannot find a backup

Create a successful backup first, then request the matching type. Check:

```sh
curl http://127.0.0.1:8080/api/v1/artifacts
curl http://127.0.0.1:8080/api/v1/jobs
curl http://127.0.0.1:8080/api/v1/restore-tests
```

### Restore-test database connection fails

Confirm the restore-test container is healthy:

```sh
docker compose ps restore-test-db
docker compose logs --tail=100 restore-test-db
```

Use a separate PostgreSQL database and credentials for restore tests. Do not point restore tests at a production source database.

### Migration fails

```sh
docker compose -f deployments/docker/docker-compose.production.yml --env-file .env run --rm migrate
```

Do not start API or workers again until migration logs are resolved. Inspect migrations in numeric order under `migrations/`.

## 17. Security and operational limits

- `full`, `differential`, and `logical` currently use custom-format PostgreSQL logical dumps.
- True PostgreSQL physical full/differential backups and WAL/PITR are not implemented.
- Retention and automatic stale-job recovery are not implemented.
- The API has no authentication or RBAC in this version.
- Workers use `pg_dump` and require read access plus normal table-lock privileges.
- Abrupt worker crashes can leave a job in `running` until operator recovery is added.
- Backups should be replicated off-host and encrypted at rest.
