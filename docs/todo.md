# Lattice todo

## Indexing improvements (DNN Platform / complex codebases)

- [x] **Phase 3: C# → SQL cross-language** — Set `FromSymbol` on C# `[Table]`, DbSet, and inline SQL refs; add C# → T-SQL bridge rules; infer source from file symbols when `FromSymbol` empty.
- [x] **Phase 1: Migration-aware symbol consolidation** — Classify migration/schema files by path; `SkipColumnLineage` on `FileInput`; T-SQL parser skips `colRefs` for those files.
- [x] **Phase 2: Reduce direct copy edge volume** — Confidence in lineage edge metadata; optional `lineage_exclude_paths` in project settings; `GetProjectByID` for loading settings.
- [x] **Phase 4: ASP and JavaScript cross-language** — ASP SQL refs get `FromSymbol` from enclosing function/sub; add JS/TS → T-SQL bridge rules.
- [x] **Phase 5: DNN-specific** — Path heuristics for DNN Platform, Providers, Dnn.AdminExperience in migration classification.
- [x] **Documentation** — codegrapspec §6.6, §7.2, §6.8; ADR-001; this todo.

## MCP server

- [x] **Streamable HTTP transport** — Add MCP Go SDK; start Streamable HTTP listener in `cmd/mcp`; register `extract_subgraph` and `ask_codebase`; config `MCP_ADDR` (default `:8080`); graceful shutdown.

## Neo4j sync performance

- [x] **Indexes for sync** — Ensure uniqueness constraints on `Symbol(id)` and `File(id)` at startup so MERGE/MATCH by id are indexed; without them, sync can take 10+ minutes instead of ~30s. See `graph.EnsureIndexes()` and `internal/graph/queries.go` (CreateConstraintSymbolID, CreateConstraintFileID).

## Cross-Language API Connections

- [x] **JS/TS parser: HTTP call extraction** — `extractAPICallRefs` detects `fetch`, `axios.get/post/put/patch/delete`, `http.get`, template literals, and binary-expression concatenation. Emits `calls_api` references with normalized paths (`{*}` for dynamic segments).
- [x] **C# parser: ASP.NET Core endpoint symbols** — `extractASPNetEndpoints` detects `[Route]`, `[HttpGet]`, `[HttpPost]`, `[HttpPut]`, `[HttpPatch]`, `[HttpDelete]` etc. on controller classes. Emits `Kind: "endpoint"` symbols with `Signature: "VERB /path/{id}"`. Route parameters with type constraints (`{id:int}`) are normalized to `{id}`.
- [x] **Java parser: Spring MVC endpoint symbols** — `extractSpringEndpoints` detects `@RestController`/`@Controller` + `@GetMapping`, `@PostMapping`, `@PutMapping`, `@PatchMapping`, `@DeleteMapping`, `@RequestMapping`. Combines class-level `@RequestMapping` base paths with method-level paths. Emits `Kind: "endpoint"` symbols with `Signature: "VERB /path/{id}"`.
- [x] **CrossLangResolver: `api_route_match` strategy** — Added `EndpointsBySignature()` to `SymbolLookup` interface. Normalized route comparison via `normalizeRouteForMatch` (lowercases, replaces `{param}`, `{*}`, `:param` → `{p}`). Bridge rules added for `javascript/typescript → csharp/java`. Production populates signatures via `ListEndpointSymbolsByProject`.

## Parallel Embeddings

- [x] **OpenRouter concurrent API requests** — `EmbedBatch` in `openrouter.go` now uses `errgroup.WithContext` with `SetLimit(10)` to fire up to 10 HTTP requests simultaneously. Chunks are pre-allocated by index so no mutex is required for result assembly.
- [x] **Bedrock concurrent API requests** — Same pattern applied to `bedrock.go` with `SetLimit(8)` to stay within AWS SDK connection limits.
- [x] **Bulk DB upserts via pgx batch** — `Store.UpsertSymbolEmbeddingsBatch` in `internal/store/store.go` pipelines up to 500 `INSERT … ON CONFLICT` statements per `pgx.Batch` `SendBatch` call, reducing N DB round-trips to ⌈N/500⌉.
- [x] **EmbedSymbols pipeline** — `EmbedSymbols` in `embedding/batch.go` now collects symbol IDs and vectors into flat slices and calls `UpsertSymbolEmbeddingsBatch` once instead of one `Exec` per symbol.

## DNN Parser Improvements

- [x] **C# parser: Method name extraction** — Fixed `extractMethodDecl` so that methods with return types (like `HttpResponseMessage`) combined with attributes (like `[HttpGet]`) don't mistakenly get parsed with their return type as their name.
- [x] **C# parser: Route template `[action]` expansion** — Updated `buildRoute` to accept `methodName` and expand `[action]` placeholders in route paths.
- [x] **JS/TS parser: `$.ajax` and `sf.getServiceRoot` support** — Added support for `$.ajax`, `$.post`, etc. and DNN's `sf.getServiceRoot('module')` so the frontend calls to API endpoints are successfully parsed and normalised (e.g. `users/list{*}`).

## Possible follow-ups

- Prefer canonical (non-migration) symbols when resolving FQNs in lineage (symbol metadata `is_migration`).
- Add `confidence` filtering in lineage queries (e.g. Neo4j filter edges below 0.7).
- PgSQL parser: support `SkipColumnLineage` for migration-classified files.
- Cross-language API: extend to other HTTP client libraries (ky, superagent, Angular HttpClient).
- Cross-language API: extract WebSocket and gRPC service connections as a follow-on.
- Cross-language API: surface `calls_api` edges in the Neo4j graph for subgraph traversal and lineage.
