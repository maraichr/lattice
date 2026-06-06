-- graph_deletions stages Neo4j nodes that must be removed because their backing
-- Postgres rows were deleted during an index run (symbols dropped from a re-parsed
-- file, or files removed entirely on an incremental run). The GraphStage reads these
-- after all parse chunks complete, prunes the corresponding Neo4j nodes, and clears
-- the staging rows. This keeps Postgres (source of truth) and Neo4j in sync without
-- giving parse workers direct Neo4j access.
CREATE TABLE graph_deletions (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    index_run_id UUID NOT NULL REFERENCES index_runs(id) ON DELETE CASCADE,
    project_id   UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    node_id      UUID NOT NULL,
    node_type    TEXT NOT NULL CHECK (node_type IN ('symbol', 'file')),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_graph_deletions_index_run ON graph_deletions(index_run_id);
