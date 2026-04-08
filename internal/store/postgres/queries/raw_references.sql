-- name: InsertRawReference :exec
INSERT INTO raw_references (project_id, index_run_id, file_id, from_symbol, to_name, to_qualified, reference_type, confidence, line, language)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10);

-- name: ListRawReferencesByIndexRun :many
SELECT * FROM raw_references WHERE index_run_id = $1;

-- name: DeleteRawReferencesByIndexRun :exec
DELETE FROM raw_references WHERE index_run_id = $1;
