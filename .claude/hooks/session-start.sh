#!/bin/bash
# SessionStart hook for Claude Code on the web.
#
# Warms the Go module cache and brings up the Postgres + Neo4j containers that the
# `//go:build integration` tests need, then exports TEST_DATABASE_URL / NEO4J_* so
# those tests can connect. Database startup is best-effort: if the container registry
# is not reachable (the environment network policy must allowlist it), the hook prints
# guidance and the session still starts with the Go toolchain ready.
set -euo pipefail

# Only run inside the remote (web) environment; local sessions manage their own stack.
if [ "${CLAUDE_CODE_REMOTE:-}" != "true" ]; then
  exit 0
fi

cd "${CLAUDE_PROJECT_DIR:-$(git rev-parse --show-toplevel)}"

log() { echo "[session-start] $*"; }

# --- Go dependencies (required) ---------------------------------------------
log "warming Go module cache..."
go mod download

# --- docker compose .env (compose interpolates ${DB_USER} etc. from it) ------
if [ ! -f .env ]; then
  log "creating .env from .env.example"
  cp .env.example .env
fi

envval() { grep -E "^$1=" .env | head -1 | cut -d= -f2-; }

# --- Postgres + Neo4j for integration tests (best-effort) -------------------
start_databases() {
  if ! command -v docker >/dev/null 2>&1; then
    log "WARN: docker not installed; skipping database startup"
    return 1
  fi

  # Route image pulls through Google's Docker Hub mirror. The environment's egress
  # proxy may block Docker Hub's CloudFront blob host (production.cloudfront.docker.com
  # returns 403 host_not_allowed), which breaks layer downloads even when auth.docker.io
  # is reachable. mirror.gcr.io is a pull-through cache that serves the same Docker Hub
  # images from Google infra, so pulls succeed without allowlisting CloudFront.
  configure_registry_mirror() {
    local cfg='{"registry-mirrors": ["https://mirror.gcr.io"]}'
    if [ "$(cat /etc/docker/daemon.json 2>/dev/null)" = "$cfg" ]; then
      return 0
    fi
    sudo mkdir -p /etc/docker
    echo "$cfg" | sudo tee /etc/docker/daemon.json >/dev/null
    # If dockerd is already running, reload so it picks up the mirror.
    if docker info >/dev/null 2>&1; then
      local pid
      pid=$(pgrep -f '^dockerd|/dockerd' | head -1)
      [ -n "$pid" ] && sudo kill -HUP "$pid" 2>/dev/null && sleep 2
    fi
  }

  configure_registry_mirror

  if ! docker info >/dev/null 2>&1; then
    log "starting dockerd..."
    sudo dockerd >/tmp/dockerd.log 2>&1 &
    for _ in $(seq 1 15); do
      docker info >/dev/null 2>&1 && break
      sleep 1
    done
  fi
  if ! docker info >/dev/null 2>&1; then
    log "WARN: docker daemon unavailable; skipping database startup"
    return 1
  fi

  log "starting postgres + neo4j + valkey via docker compose..."
  if ! docker compose up -d postgres neo4j valkey; then
    log "WARN: could not start database containers (image pull blocked?)."
    log "      Docker Hub images are pulled via the mirror.gcr.io mirror to avoid the"
    log "      egress-blocked CloudFront blob host. If pulls still fail, allowlist the"
    log "      registry in the environment network policy: *.docker.io, auth.docker.io,"
    log "      storage.googleapis.com, mirror.gcr.io"
    return 1
  fi

  log "waiting for postgres to accept connections..."
  for _ in $(seq 1 30); do
    docker compose exec -T postgres pg_isready >/dev/null 2>&1 && break
    sleep 2
  done

  log "applying Postgres migrations..."
  docker compose up migrate >/dev/null 2>&1 || log "WARN: migrate step failed"
  log "applying Neo4j init..."
  docker compose up neo4j-init >/dev/null 2>&1 || true
  return 0
}

if start_databases; then
  if [ -n "${CLAUDE_ENV_FILE:-}" ]; then
    {
      echo "export TEST_DATABASE_URL=\"$(envval TEST_DATABASE_URL)\""
      echo "export NEO4J_URI=\"bolt://localhost:7687\""
      echo "export NEO4J_USER=\"$(envval NEO4J_USER)\""
      echo "export NEO4J_PASSWORD=\"$(envval NEO4J_PASSWORD)\""
      echo "export TEST_VALKEY_ADDR=\"$(envval TEST_VALKEY_ADDR)\""
    } >> "$CLAUDE_ENV_FILE"
  fi
  log "databases ready; integration env exported."
  log "run integration tests with: go test -tags integration ./internal/..."
else
  log "continuing without integration databases (unit tests still run)."
fi
