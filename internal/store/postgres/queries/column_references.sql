-- name: InsertColumnReference :exec
INSERT INTO column_references (project_id, index_run_id, source_column, target_column, derivation_type, expression, context, line)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- name: ListColumnReferencesByIndexRun :many
SELECT * FROM column_references WHERE index_run_id = $1;

-- name: DeleteColumnReferencesByIndexRun :exec
DELETE FROM column_references WHERE index_run_id = $1;
