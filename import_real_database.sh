#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
COMPOSE=(-f "$ROOT_DIR/docker-compose.realdatabase.yml" --env-file "$ROOT_DIR/.env")

cd "$ROOT_DIR"
docker compose "${COMPOSE[@]}" up -d real-database

echo "Waiting for the real database container..."
for _ in $(seq 1 60); do
  if docker compose "${COMPOSE[@]}" exec -T real-database pg_isready -U postgres -d postgres >/dev/null 2>&1; then
    break
  fi
  sleep 1
done
docker compose "${COMPOSE[@]}" exec -T real-database pg_isready -U postgres -d postgres >/dev/null

# Load local values used by the application database URLs.
set -a
source "$ROOT_DIR/.env"
set +a
BACKUP_DATABASE_USER="${BACKUP_DATABASE_USER:-real_platform}"
REAL_DATABASE_PASSWORD="${REAL_DATABASE_PASSWORD:?Set REAL_DATABASE_PASSWORD in .env}"

docker compose "${COMPOSE[@]}" exec -T real-database psql -v ON_ERROR_STOP=1 -v backup_user="$BACKUP_DATABASE_USER" -v backup_password="$REAL_DATABASE_PASSWORD" -U postgres -d postgres <<'SQL'
SELECT format('CREATE ROLE %I LOGIN PASSWORD %L', :'backup_user', :'backup_password')
WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = :'backup_user') \gexec
SELECT format('ALTER ROLE %I LOGIN PASSWORD %L', :'backup_user', :'backup_password') \gexec
SQL

configure_backup_access() {
  local database="$1"
  docker compose "${COMPOSE[@]}" exec -T -e PGPASSWORD="$REAL_DATABASE_PASSWORD" real-database psql \
    -v ON_ERROR_STOP=1 -v backup_user="$BACKUP_DATABASE_USER" -U postgres -d "$database" <<'SQL'
SELECT format('GRANT CONNECT ON DATABASE %I TO %I', current_database(), :'backup_user') \gexec
SELECT format('GRANT USAGE ON SCHEMA public TO %I', :'backup_user') \gexec
SELECT format('GRANT SELECT ON ALL TABLES IN SCHEMA public TO %I', :'backup_user') \gexec
SELECT format('GRANT SELECT ON ALL SEQUENCES IN SCHEMA public TO %I', :'backup_user') \gexec
SELECT format('ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT ON TABLES TO %I', :'backup_user') \gexec
SELECT format('ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT ON SEQUENCES TO %I', :'backup_user') \gexec
SQL
}

import_dump() {
  local file="$1"
  local database="$2"
  echo "Importing $database from $(basename "$file")"

  docker compose "${COMPOSE[@]}" exec -T real-database psql -v ON_ERROR_STOP=1 -U postgres -d postgres \
    -c "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '$database' AND pid <> pg_backend_pid();" >/dev/null
  docker compose "${COMPOSE[@]}" exec -T real-database dropdb --if-exists --force -U postgres "$database" >/dev/null
  docker compose "${COMPOSE[@]}" exec -T real-database createdb -U postgres "$database"
  docker compose "${COMPOSE[@]}" exec -T -i real-database pg_restore --no-owner --no-privileges --exit-on-error -U postgres -d "$database" < "$file"
  configure_backup_access "$database"
}

import_dump "$ROOT_DIR/initial_db/dump-central_sms_identity-202610281952_ev.sql" central_sms_identity
import_dump "$ROOT_DIR/initial_db/dump-central_sms-202610281951_ev.sql" central_sms
import_dump "$ROOT_DIR/initial_db/dump-central_sms_billing-202610281952_ev.sql" central_sms_billing

echo "Imported databases: central_sms_identity, central_sms, central_sms_billing"
