# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Development Commands

```bash
# Start all infrastructure (Postgres, Neo4j, Valkey, MinIO, Keycloak) + API/worker with hot-reload
docker compose up -d

# Build all Go binaries
make build

# Run all tests with race detection
make test

# Run a single test
go test ./internal/parser/tsql/ -run TestParseCreateProcedure -race

# Lint (go vet + biome)
make lint

# Format (go fmt + biome)
make fmt

# Code generation (must run after changing SQL queries or GraphQL schema)
make generate           # both SQLC + gqlgen
make generate-sqlc      # after editing internal/store/postgres/queries/*.sql
make generate-graphql   # after editing internal/api/graphql/*.graphqls

# Database migrations
make migrate-up
make migrate-down                              # rolls back 1 migration
make migrate-create name=add_new_table         # creates new migration pair

# Frontend (separate from Go)
cd frontend && pnpm install && pnpm dev        # dev server on :3000
cd frontend && pnpm test                       # vitest
cd frontend && pnpm lint                       # biome check
```

## Architecture

### Four Services

The system runs as four separate Go binaries (`cmd/`), all sharing `internal/`:

- **api** — REST (chi) + GraphQL (gqlgen) server on :8080. Serves the frontend API.
- **worker** — Consumes Valkey stream messages and runs the indexing pipeline.
- **mcp** — Model Context Protocol server on :8090 (behind Caddy TLS on :8443). Exposes codebase graph to LLMs.
- **scheduler** — Cron-based scheduled indexing triggers.

### Indexing Pipeline (worker)

The core of the system. Defined in `internal/ingestion/pipeline.go`, it processes jobs through sequential stages (`Stage` interface):

**Clone → Parse → Resolve → Persist → GraphSync → Lineage → Embed → Analytics**

Each stage reads/writes to `IndexRunContext` which carries state through the pipeline. Key stages:
- `clone_stage` — Clones git repos or fetches from S3/ZIP, handles incremental diffs via commit SHA
- `parse_stage` — Routes files to language parsers via `parser.Registry`, produces `[]FileResult`
- `resolve_stage` — Cross-file symbol resolution (`internal/resolver/`)
- `graph_stage` — Syncs symbols/edges to Neo4j (`internal/graph/`)
- `embed_stage` — Generates vector embeddings via OpenRouter API (`internal/embedding/`)

### Parser System

`internal/parser/parser.go` defines the `Parser` interface. Each language has its own subpackage:
- `tsql/`, `pgsql/` — Hand-written SQL parsers (T-SQL uses regex-based, PgSQL uses `pg_query_go`)
- `java/` — Uses `go-tree-sitter`
- `asp/`, `delphi/`, `csharp/` — Hand-written parsers

Parsers return `ParseResult` containing `[]Symbol`, `[]RawReference`, and `[]ColumnReference`. The registry maps file extensions to parsers.

### Data Layer

- **PostgreSQL** (pgx + SQLC) — Primary store. Queries in `internal/store/postgres/queries/*.sql`, generated code in `internal/store/postgres/`. The `Store` struct in `internal/store/store.go` wraps SQLC queries and provides `WithTx()`.
- **Neo4j** — Graph database for symbol relationships, traversals, impact analysis. Client in `internal/graph/`.
- **Valkey** — Redis-compatible queue for ingestion jobs (`internal/ingestion/queue.go`).
- **MinIO** — S3-compatible storage for uploaded artifacts.

### API Structure

REST routes defined in `internal/api/router.go` using chi. Handlers in `internal/api/handler/`. GraphQL schema in `internal/api/graphql/*.graphqls` with gqlgen resolvers.

Auth is OIDC-based via Keycloak (toggled by `AUTH_ENABLED` env var). When disabled, `DevModeMiddleware` grants all scopes. Scopes: `lattice:read`, `lattice:write`, `lattice:ingest`.

### Frontend

React 19 + TypeScript + Vite 7 + Tailwind CSS 4. Uses shadcn/ui components (Radix primitives), TanStack Query for data fetching, Zustand for state, React Router v7, Cytoscape.js for graph visualization. Linted with Biome (not ESLint).

### MCP Server

`internal/mcp/server.go` implements the MCP protocol. Tools in `internal/mcp/tools/` expose search, lineage, impact analysis, and subgraph extraction to LLMs. Session management in `internal/mcp/session/`.

## Key Conventions

- All Go code uses `log/slog` for structured logging
- Error types for API responses live in `pkg/apierr/`
- Shared domain models in `pkg/models/`
- GraphQL models autobind to `pkg/models` (see `gqlgen.yml`)
- SQLC generates to `internal/store/postgres/` — do not edit generated files (`*.sql.go`)
- Neo4j Cypher migrations in `migrations/neo4j/init.cypher`
- Environment config loaded via `internal/config/` from `.env` file
