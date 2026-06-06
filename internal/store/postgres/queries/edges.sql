-- name: CreateSymbolEdge :one
INSERT INTO symbol_edges (project_id, source_id, target_id, edge_type)
VALUES ($1, $2, $3, $4)
ON CONFLICT (project_id, source_id, target_id, edge_type) DO NOTHING
RETURNING *;

-- name: CountEdgesByProject :one
SELECT count(*) FROM symbol_edges WHERE project_id = $1;

-- name: GetIncomingEdges :many
SELECT * FROM symbol_edges WHERE target_id = $1;

-- name: GetOutgoingEdges :many
SELECT * FROM symbol_edges WHERE source_id = $1;

-- name: ListEdgesByProject :many
SELECT * FROM symbol_edges WHERE project_id = $1;

-- name: ListEdgesByProjectCreatedSince :many
SELECT * FROM symbol_edges WHERE project_id = @project_id AND created_at >= @since;

-- name: CreateSymbolEdgeWithMetadata :one
INSERT INTO symbol_edges (project_id, source_id, target_id, edge_type, metadata)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (project_id, source_id, target_id, edge_type) DO UPDATE
SET metadata = EXCLUDED.metadata
RETURNING *;

-- name: ListColumnEdgesByProject :many
SELECT * FROM symbol_edges
WHERE project_id = $1
  AND edge_type IN ('transforms_to', 'direct_copy', 'uses_column');
