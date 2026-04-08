CREATE TABLE raw_references (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id      UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    index_run_id    UUID NOT NULL REFERENCES index_runs(id) ON DELETE CASCADE,
    file_id         UUID NOT NULL REFERENCES files(id) ON DELETE CASCADE,
    from_symbol     TEXT NOT NULL,
    to_name         TEXT NOT NULL DEFAULT '',
    to_qualified    TEXT NOT NULL DEFAULT '',
    reference_type  TEXT NOT NULL,
    confidence      DOUBLE PRECISION NOT NULL DEFAULT 0,
    line            INT,
    language        TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_raw_references_index_run ON raw_references(index_run_id);
CREATE INDEX idx_raw_references_file ON raw_references(file_id);
