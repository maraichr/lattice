-- Migration 007: Add column_references table for distributed parse pipeline.
-- Column references extracted by parse workers are stored here so the lineage
-- stage can run after all parse chunks complete, without holding results in memory.

CREATE TABLE IF NOT EXISTS column_references (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id      UUID        NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    index_run_id    UUID        NOT NULL REFERENCES index_runs(id) ON DELETE CASCADE,
    source_column   TEXT        NOT NULL,
    target_column   TEXT        NOT NULL,
    derivation_type TEXT        NOT NULL,
    expression      TEXT,
    context         TEXT,
    line            INT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_col_refs_project_id    ON column_references (project_id);
CREATE INDEX idx_col_refs_index_run_id  ON column_references (index_run_id);
