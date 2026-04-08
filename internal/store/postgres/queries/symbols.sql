-- name: CreateSymbol :one
INSERT INTO symbols (project_id, file_id, name, qualified_name, kind, language, start_line, end_line, start_col, end_col, signature, doc_comment)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
ON CONFLICT (project_id, qualified_name, kind) DO UPDATE SET
    file_id = EXCLUDED.file_id,
    name = EXCLUDED.name,
    language = EXCLUDED.language,
    start_line = EXCLUDED.start_line,
    end_line = EXCLUDED.end_line,
    start_col = EXCLUDED.start_col,
    end_col = EXCLUDED.end_col,
    signature = EXCLUDED.signature,
    doc_comment = EXCLUDED.doc_comment,
    updated_at = now()
RETURNING *;

-- name: CountSymbolsByProject :one
SELECT count(*) FROM symbols WHERE project_id = $1;

-- name: DeleteSymbolsByFile :exec
DELETE FROM symbols WHERE file_id = $1;

-- name: GetSymbol :one
SELECT * FROM symbols WHERE id = $1;

-- name: SearchSymbols :many
SELECT * FROM symbols
WHERE project_id = (SELECT id FROM projects WHERE slug = @project_slug)
  AND (name ILIKE '%' || @query || '%' OR qualified_name ILIKE '%' || @query || '%')
  AND (cardinality(@kinds::text[]) = 0 OR kind = ANY(@kinds::text[]))
  AND (cardinality(@languages::text[]) = 0 OR language = ANY(@languages::text[]))
ORDER BY name
LIMIT @lim;

-- name: GetSymbolsByProject :many
SELECT * FROM symbols WHERE project_id = $1 ORDER BY qualified_name LIMIT $2 OFFSET $3;

-- name: ListSymbolsByProject :many
SELECT * FROM symbols WHERE project_id = $1;

-- name: ListSymbolsByFileIDs :many
SELECT * FROM symbols WHERE file_id = ANY($1::uuid[]);

-- name: GetSymbolByQualifiedName :one
SELECT * FROM symbols WHERE project_id = $1 AND qualified_name = $2;

-- name: ListSymbolsByNames :many
SELECT * FROM symbols WHERE project_id = $1 AND name = ANY($2::text[]);

-- name: DeleteSymbolsByFileID :exec
DELETE FROM symbols WHERE file_id = $1;

-- name: ListColumnSymbolsByProject :many
SELECT * FROM symbols WHERE project_id = $1 AND kind = 'column';

-- name: SearchSymbolsGlobal :many
SELECT s.*, p.slug AS project_slug
FROM symbols s
JOIN projects p ON s.project_id = p.id
WHERE (s.name ILIKE '%' || @query || '%' OR s.qualified_name ILIKE '%' || @query || '%')
  AND (cardinality(@kinds::text[]) = 0 OR s.kind = ANY(@kinds::text[]))
  AND (cardinality(@languages::text[]) = 0 OR s.language = ANY(@languages::text[]))
ORDER BY s.name
LIMIT @lim;

-- name: SearchSymbolsRanked :many
SELECT * FROM symbols
WHERE project_id = (SELECT id FROM projects WHERE slug = @project_slug)
  AND (name ILIKE '%' || @query || '%' OR qualified_name ILIKE '%' || @query || '%')
  AND (cardinality(@kinds::text[]) = 0 OR kind = ANY(@kinds::text[]))
  AND (cardinality(@languages::text[]) = 0 OR language = ANY(@languages::text[]))
ORDER BY
  CASE WHEN lower(name) = lower(@query) THEN 0
       WHEN lower(qualified_name) = lower(@query) THEN 1
       WHEN lower(name) LIKE lower(@query) || '%' THEN 2
       ELSE 3 END,
  (COALESCE(metadata->>'in_degree', '0'))::int DESC
LIMIT @lim;

-- name: ListTopSymbolsByKind :many
SELECT * FROM symbols
WHERE project_id = (SELECT id FROM projects WHERE slug = @project_slug)
  AND (cardinality(@kinds::text[]) = 0 OR kind = ANY(@kinds::text[]))
  AND (cardinality(@languages::text[]) = 0 OR language = ANY(@languages::text[]))
ORDER BY (COALESCE(metadata->>'in_degree', '0'))::int DESC
LIMIT @lim;

-- ListSymbolsByQualifiedNames is in symbols_ext.go (hand-written for proper type handling)
-- GetSymbolsByProjectPaged is in symbols_ext.go (hand-written, returns SymbolSummary)
