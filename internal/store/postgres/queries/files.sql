-- name: UpsertFile :one
INSERT INTO files (project_id, source_id, path, language, size_bytes, hash, last_indexed_at)
VALUES ($1, $2, $3, $4, $5, $6, now())
ON CONFLICT (project_id, source_id, path) DO UPDATE
SET language = EXCLUDED.language,
    size_bytes = EXCLUDED.size_bytes,
    hash = EXCLUDED.hash,
    last_indexed_at = now(),
    updated_at = now()
RETURNING *;

-- name: CountFilesByProject :one
SELECT count(*) FROM files WHERE project_id = $1;

-- name: GetFile :one
SELECT * FROM files WHERE id = $1;

-- name: ListFilesByProject :many
SELECT * FROM files WHERE project_id = $1;

-- name: ListFilesByProjectUpdatedSince :many
SELECT * FROM files WHERE project_id = @project_id AND updated_at >= @since;

-- name: DeleteFile :exec
DELETE FROM files WHERE id = $1;

-- name: ListFilesBySourceID :many
SELECT * FROM files WHERE source_id = $1;

-- name: GetFileByPath :one
SELECT * FROM files WHERE project_id = $1 AND source_id = $2 AND path = $3;
