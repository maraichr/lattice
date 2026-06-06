-- name: RecordGraphDeletion :exec
INSERT INTO graph_deletions (index_run_id, project_id, node_id, node_type)
VALUES ($1, $2, $3, $4);

-- name: ListGraphDeletionsByIndexRun :many
SELECT * FROM graph_deletions WHERE index_run_id = $1;

-- name: DeleteGraphDeletionsByIndexRun :exec
DELETE FROM graph_deletions WHERE index_run_id = $1;
