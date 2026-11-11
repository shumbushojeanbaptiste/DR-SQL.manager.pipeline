#!/usr/bin/env bash
set -euo pipefail

DEPLOY_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$DEPLOY_DIR/../.." && pwd)"
ENV_FILE="${ENV_FILE:-$PROJECT_ROOT/.env}"
COMPOSE=(docker compose -f "$DEPLOY_DIR/docker-compose.production.yml" --env-file "$ENV_FILE")

if [[ ! -f "$ENV_FILE" ]]; then
  echo "Production environment file not found: $ENV_FILE" >&2
  exit 1
fi

cd "$PROJECT_ROOT"
echo "Pulling the configured application image..."
"${COMPOSE[@]}" pull migrate

"${COMPOSE[@]}" pull restore-test-db
"${COMPOSE[@]}" pull api worker-identity worker-messaging worker-billing

echo "Running database migrations before rolling services..."
"${COMPOSE[@]}" up -d --wait restore-test-db
"${COMPOSE[@]}" up migrate
MIGRATE_CONTAINER=$("${COMPOSE[@]}" ps -a -q migrate)
MIGRATE_STATUS=$(docker inspect -f '{{.State.ExitCode}}' "$MIGRATE_CONTAINER")
if [[ "$MIGRATE_STATUS" != "0" ]]; then
  echo "Database migration failed with exit code $MIGRATE_STATUS" >&2
  "${COMPOSE[@]}" logs migrate >&2
  exit "$MIGRATE_STATUS"
fi

echo "Starting API and workers..."
"${COMPOSE[@]}" up -d --no-deps api worker-identity worker-messaging worker-billing
"${COMPOSE[@]}" ps
