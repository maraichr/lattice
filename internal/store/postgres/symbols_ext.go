package postgres

// symbols_ext.go contains hand-written queries supplementing the SQLC-generated symbols.sql.go.
// These provide bulk / paginated lookups needed by the DB-driven resolver.

import (
	"context"

	"github.com/google/uuid"
)

// SymbolSummary is a lightweight symbol projection used by the resolver.
// It contains only the fields needed for cross-file edge resolution.
type SymbolSummary struct {
	ID            uuid.UUID
	QualifiedName string
	Name          string
	Language      string
	FileID        uuid.UUID
}

// ListSymbolsByQualifiedNames returns full Symbol records for a batch of qualified names
// within a project. The caller should keep batch sizes reasonable (≤ 5 000).
func (q *Queries) ListSymbolsByQualifiedNames(ctx context.Context, projectID uuid.UUID, qualifiedNames []string) ([]Symbol, error) {
	if len(qualifiedNames) == 0 {
		return nil, nil
	}

	rows, err := q.db.Query(ctx,
		`SELECT id, project_id, file_id, name, qualified_name, kind, language,
		        start_line, end_line, start_col, end_col, signature, doc_comment,
		        metadata, created_at, updated_at
		 FROM symbols
		 WHERE project_id = $1 AND qualified_name = ANY($2::text[])`,
		projectID, qualifiedNames)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []Symbol
	for rows.Next() {
		var i Symbol
		if err := rows.Scan(
			&i.ID, &i.ProjectID, &i.FileID, &i.Name, &i.QualifiedName,
			&i.Kind, &i.Language, &i.StartLine, &i.EndLine,
			&i.StartCol, &i.EndCol, &i.Signature, &i.DocComment,
			&i.Metadata, &i.CreatedAt, &i.UpdatedAt,
		); err != nil {
			return nil, err
		}
		items = append(items, i)
	}
	return items, rows.Err()
}

// EndpointSummary is a lightweight projection used for cross-language API route matching.
type EndpointSummary struct {
	ID        uuid.UUID
	QualName  string
	Signature string
	Language  string
}

// ListEndpointSymbolsByProject returns all symbols with kind='endpoint' for a project.
// Used by the CrossLangResolver api_route_match strategy to map backend routes.
func (q *Queries) ListEndpointSymbolsByProject(ctx context.Context, projectID uuid.UUID) ([]EndpointSummary, error) {
	rows, err := q.db.Query(ctx,
		`SELECT id, qualified_name, COALESCE(signature, ''), language
		 FROM symbols
		 WHERE project_id = $1 AND kind = 'endpoint'`,
		projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []EndpointSummary
	for rows.Next() {
		var i EndpointSummary
		if err := rows.Scan(&i.ID, &i.QualName, &i.Signature, &i.Language); err != nil {
			return nil, err
		}
		items = append(items, i)
	}
	return items, rows.Err()
}

// GetSymbolsByProjectPaged returns lightweight symbol summaries for a project, paginated
// by (limit, offset). Used by the DB-driven resolver to iterate symbols without loading
// the entire project into memory.
func (q *Queries) GetSymbolsByProjectPaged(ctx context.Context, projectID uuid.UUID, limit, offset int64) ([]SymbolSummary, error) {
	rows, err := q.db.Query(ctx,
		`SELECT id, qualified_name, name, language, file_id
		 FROM symbols
		 WHERE project_id = $1
		 ORDER BY id
		 LIMIT $2 OFFSET $3`,
		projectID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []SymbolSummary
	for rows.Next() {
		var i SymbolSummary
		if err := rows.Scan(&i.ID, &i.QualifiedName, &i.Name, &i.Language, &i.FileID); err != nil {
			return nil, err
		}
		items = append(items, i)
	}
	return items, rows.Err()
}
