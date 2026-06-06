package ingestion

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/maraichr/lattice/internal/graph"
	"github.com/maraichr/lattice/internal/store"
	"github.com/maraichr/lattice/internal/store/postgres"
)

// GraphStage syncs symbols and edges from PostgreSQL to Neo4j.
//
// It is delta-aware: rows touched during this index run carry updated_at/created_at
// at or after the run's started_at, so only those are upserted (MERGE is idempotent).
// Nodes whose Postgres rows were deleted during the run are staged in graph_deletions
// and pruned here, keeping Postgres (the source of truth) and Neo4j consistent. When
// the run start time is unavailable, it falls back to a full project sync.
type GraphStage struct {
	store  *store.Store
	graph  *graph.Client
	logger *slog.Logger
}

func NewGraphStage(s *store.Store, g *graph.Client, logger *slog.Logger) *GraphStage {
	return &GraphStage{store: s, graph: g, logger: logger}
}

func (s *GraphStage) Name() string { return "graph_build" }

func (s *GraphStage) Execute(ctx context.Context, rc *IndexRunContext) error {
	var since time.Time
	if run, err := s.store.GetIndexRun(ctx, rc.IndexRunID); err == nil && run.StartedAt.Valid {
		since = run.StartedAt.Time
	}

	files, symbols, edges, err := s.loadDelta(ctx, rc.ProjectID, since)
	if err != nil {
		return err
	}

	s.logger.Info("syncing to neo4j",
		slog.Bool("incremental", !since.IsZero()),
		slog.Int("files", len(files)),
		slog.Int("symbols", len(symbols)),
		slog.Int("edges", len(edges)))

	if err := s.graph.SyncFiles(ctx, rc.ProjectID, files); err != nil {
		return fmt.Errorf("sync files to neo4j: %w", err)
	}
	if err := s.graph.SyncSymbols(ctx, rc.ProjectID, symbols); err != nil {
		return fmt.Errorf("sync symbols to neo4j: %w", err)
	}
	if err := s.graph.SyncEdges(ctx, rc.ProjectID, edges); err != nil {
		return fmt.Errorf("sync edges to neo4j: %w", err)
	}
	if err := s.graph.SyncColumnEdges(ctx, rc.ProjectID, edges); err != nil {
		return fmt.Errorf("sync column edges to neo4j: %w", err)
	}

	return s.applyDeletions(ctx, rc.IndexRunID)
}

// loadDelta returns the files, symbols, and edges that changed during this run. When
// since is zero (run start time unknown), it returns the full project corpus instead.
func (s *GraphStage) loadDelta(ctx context.Context, projectID uuid.UUID, since time.Time) ([]postgres.File, []postgres.Symbol, []postgres.SymbolEdge, error) {
	if since.IsZero() {
		files, err := s.store.ListFilesByProject(ctx, projectID)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("load files: %w", err)
		}
		symbols, err := s.store.ListSymbolsByProject(ctx, projectID)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("load symbols: %w", err)
		}
		edges, err := s.store.ListEdgesByProject(ctx, projectID)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("load edges: %w", err)
		}
		return files, symbols, edges, nil
	}

	files, err := s.store.ListFilesByProjectUpdatedSince(ctx, postgres.ListFilesByProjectUpdatedSinceParams{ProjectID: projectID, Since: since})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load changed files: %w", err)
	}
	symbols, err := s.store.ListSymbolsByProjectUpdatedSince(ctx, postgres.ListSymbolsByProjectUpdatedSinceParams{ProjectID: projectID, Since: since})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load changed symbols: %w", err)
	}
	edges, err := s.store.ListEdgesByProjectCreatedSince(ctx, postgres.ListEdgesByProjectCreatedSinceParams{ProjectID: projectID, Since: since})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load changed edges: %w", err)
	}
	return files, symbols, edges, nil
}

// applyDeletions prunes Neo4j nodes whose Postgres rows were deleted during this run,
// then clears the staging rows.
func (s *GraphStage) applyDeletions(ctx context.Context, indexRunID uuid.UUID) error {
	dels, err := s.store.ListGraphDeletionsByIndexRun(ctx, indexRunID)
	if err != nil {
		return fmt.Errorf("load graph deletions: %w", err)
	}
	if len(dels) == 0 {
		return nil
	}

	var symbolIDs, fileIDs []string
	for _, d := range dels {
		switch d.NodeType {
		case "symbol":
			symbolIDs = append(symbolIDs, d.NodeID.String())
		case "file":
			fileIDs = append(fileIDs, d.NodeID.String())
		}
	}

	if err := s.graph.DeleteSymbolNodes(ctx, symbolIDs); err != nil {
		return fmt.Errorf("prune symbol nodes: %w", err)
	}
	if err := s.graph.DeleteFileNodes(ctx, fileIDs); err != nil {
		return fmt.Errorf("prune file nodes: %w", err)
	}

	s.logger.Info("neo4j: pruned deleted nodes",
		slog.Int("symbols", len(symbolIDs)),
		slog.Int("files", len(fileIDs)))

	if err := s.store.DeleteGraphDeletionsByIndexRun(ctx, indexRunID); err != nil {
		return fmt.Errorf("clear graph deletions: %w", err)
	}
	return nil
}
