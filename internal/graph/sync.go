package graph

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/neo4j/neo4j-go-driver/v5/neo4j"

	"github.com/maraichr/lattice/internal/store/postgres"
)

const batchSize = 500

// SyncSymbols upserts symbol nodes into Neo4j from PostgreSQL data.
func (c *Client) SyncSymbols(ctx context.Context, projectID uuid.UUID, symbols []postgres.Symbol) error {
	session := c.Session(ctx)
	defer session.Close(ctx)

	for i := 0; i < len(symbols); i += batchSize {
		end := min(i+batchSize, len(symbols))
		batch := symbols[i:end]

		params := make([]map[string]any, len(batch))
		for j, sym := range batch {
			params[j] = map[string]any{
				"id":            sym.ID.String(),
				"name":          sym.Name,
				"qualifiedName": sym.QualifiedName,
				"kind":          sym.Kind,
				"language":      sym.Language,
				"projectId":     projectID.String(),
				"fileId":        sym.FileID.String(),
				"startLine":     sym.StartLine,
				"endLine":       sym.EndLine,
			}
		}

		_, err := neo4j.ExecuteWrite(ctx, session, func(tx neo4j.ManagedTransaction) (any, error) {
			_, err := tx.Run(ctx, UpsertSymbolNode, map[string]any{"symbols": params})
			if err != nil {
				return struct{}{}, err
			}
			// Also link symbols to files
			_, err = tx.Run(ctx, LinkSymbolToFile, map[string]any{"symbols": params})
			return struct{}{}, err
		})
		if err != nil {
			return fmt.Errorf("sync symbols batch %d: %w", i/batchSize, err)
		}
	}
	return nil
}

// SyncEdges upserts edges into Neo4j from PostgreSQL data.
func (c *Client) SyncEdges(ctx context.Context, projectID uuid.UUID, edges []postgres.SymbolEdge) error {
	session := c.Session(ctx)
	defer session.Close(ctx)

	for i := 0; i < len(edges); i += batchSize {
		end := min(i+batchSize, len(edges))
		batch := edges[i:end]

		params := make([]map[string]any, 0, len(batch))
		for _, edge := range batch {
			// Column-level edges are represented as COLUMN_FLOW by SyncColumnEdges;
			// excluding them here keeps DEPENDS_ON traversals (lineage/impact) clean.
			if isColumnEdgeType(edge.EdgeType) {
				continue
			}
			params = append(params, map[string]any{
				"sourceId":  edge.SourceID.String(),
				"targetId":  edge.TargetID.String(),
				"edgeType":  edge.EdgeType,
				"projectId": projectID.String(),
			})
		}
		if len(params) == 0 {
			continue
		}

		_, err := neo4j.ExecuteWrite(ctx, session, func(tx neo4j.ManagedTransaction) (any, error) {
			_, err := tx.Run(ctx, UpsertEdge, map[string]any{"edges": params})
			return struct{}{}, err
		})
		if err != nil {
			return fmt.Errorf("sync edges batch %d: %w", i/batchSize, err)
		}
	}
	return nil
}

// SyncFiles upserts file nodes into Neo4j from PostgreSQL data.
func (c *Client) SyncFiles(ctx context.Context, projectID uuid.UUID, files []postgres.File) error {
	session := c.Session(ctx)
	defer session.Close(ctx)

	for i := 0; i < len(files); i += batchSize {
		end := min(i+batchSize, len(files))
		batch := files[i:end]

		params := make([]map[string]any, len(batch))
		for j, f := range batch {
			params[j] = map[string]any{
				"id":        f.ID.String(),
				"path":      f.Path,
				"language":  f.Language,
				"projectId": projectID.String(),
				"sourceId":  f.SourceID.String(),
			}
		}

		_, err := neo4j.ExecuteWrite(ctx, session, func(tx neo4j.ManagedTransaction) (any, error) {
			_, err := tx.Run(ctx, UpsertFileNode, map[string]any{"files": params})
			return struct{}{}, err
		})
		if err != nil {
			return fmt.Errorf("sync files batch %d: %w", i/batchSize, err)
		}
	}
	return nil
}

// SyncColumnEdges upserts column-level edges into Neo4j.
func (c *Client) SyncColumnEdges(ctx context.Context, projectID uuid.UUID, edges []postgres.SymbolEdge) error {
	session := c.Session(ctx)
	defer session.Close(ctx)

	// Filter to column-level edge types
	var colEdges []postgres.SymbolEdge
	for _, e := range edges {
		if isColumnEdgeType(e.EdgeType) {
			colEdges = append(colEdges, e)
		}
	}

	for i := 0; i < len(colEdges); i += batchSize {
		end := min(i+batchSize, len(colEdges))
		batch := colEdges[i:end]

		params := make([]map[string]any, len(batch))
		for j, edge := range batch {
			derivation := edge.EdgeType
			expression := ""
			if len(edge.Metadata) > 0 {
				var meta map[string]string
				if err := json.Unmarshal(edge.Metadata, &meta); err == nil {
					if d, ok := meta["derivation_type"]; ok {
						derivation = d
					}
					expression = meta["expression"]
				}
			}
			params[j] = map[string]any{
				"sourceId":       edge.SourceID.String(),
				"targetId":       edge.TargetID.String(),
				"derivationType": derivation,
				"expression":     expression,
				"projectId":      projectID.String(),
			}
		}

		_, err := neo4j.ExecuteWrite(ctx, session, func(tx neo4j.ManagedTransaction) (any, error) {
			_, err := tx.Run(ctx, UpsertColumnEdge, map[string]any{"edges": params})
			return struct{}{}, err
		})
		if err != nil {
			return fmt.Errorf("sync column edges batch %d: %w", i/batchSize, err)
		}
	}
	return nil
}

// isColumnEdgeType reports whether an edge type is a column-level lineage edge,
// which is represented in Neo4j as COLUMN_FLOW rather than DEPENDS_ON.
func isColumnEdgeType(t string) bool {
	return t == "transforms_to" || t == "direct_copy" || t == "uses_column"
}

// DeleteSymbolNodes removes symbol nodes (and their relationships) from Neo4j by ID.
func (c *Client) DeleteSymbolNodes(ctx context.Context, ids []string) error {
	return c.deleteNodesByID(ctx, DeleteSymbolNodesByID, ids)
}

// DeleteFileNodes removes file nodes (and their relationships) from Neo4j by ID.
func (c *Client) DeleteFileNodes(ctx context.Context, ids []string) error {
	return c.deleteNodesByID(ctx, DeleteFileNodesByID, ids)
}

func (c *Client) deleteNodesByID(ctx context.Context, query string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	session := c.Session(ctx)
	defer session.Close(ctx)

	for i := 0; i < len(ids); i += batchSize {
		end := min(i+batchSize, len(ids))
		batch := ids[i:end]

		_, err := neo4j.ExecuteWrite(ctx, session, func(tx neo4j.ManagedTransaction) (any, error) {
			_, err := tx.Run(ctx, query, map[string]any{"ids": batch})
			return struct{}{}, err
		})
		if err != nil {
			return fmt.Errorf("delete nodes batch %d: %w", i/batchSize, err)
		}
	}
	return nil
}

// ClearProject removes all graph data for a project.
func (c *Client) ClearProject(ctx context.Context, projectID uuid.UUID) error {
	session := c.Session(ctx)
	defer session.Close(ctx)

	_, err := neo4j.ExecuteWrite(ctx, session, func(tx neo4j.ManagedTransaction) (any, error) {
		_, err := tx.Run(ctx, DeleteProjectNodes, map[string]any{
			"projectId": projectID.String(),
		})
		return struct{}{}, err
	})
	return err
}
