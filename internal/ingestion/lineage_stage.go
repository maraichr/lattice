package ingestion

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/maraichr/lattice/internal/lineage"
	"github.com/maraichr/lattice/internal/parser"
	"github.com/maraichr/lattice/internal/store"
)

// LineageStage builds column-level lineage edges from column references stored in the DB.
// Parse workers persist column references to the column_references table; this stage
// reads them back, builds lineage edges, then cleans up the staging rows.
type LineageStage struct {
	engine *lineage.Engine
	store  *store.Store
	logger *slog.Logger
}

func NewLineageStage(e *lineage.Engine, s *store.Store, logger *slog.Logger) *LineageStage {
	return &LineageStage{engine: e, store: s, logger: logger}
}

func (s *LineageStage) Name() string { return "lineage" }

func (s *LineageStage) Execute(ctx context.Context, rc *IndexRunContext) error {
	// Load column references from DB (stored by parse workers).
	dbRefs, err := s.store.ListColumnReferencesByIndexRun(ctx, rc.IndexRunID)
	if err != nil {
		return fmt.Errorf("load column references: %w", err)
	}

	if len(dbRefs) == 0 {
		s.logger.Info("no column references to process")
		return nil
	}

	// Convert DB rows back to parser.ColumnReference for the lineage engine.
	colRefs := make([]parser.ColumnReference, 0, len(dbRefs))
	for _, r := range dbRefs {
		cr := parser.ColumnReference{
			SourceColumn:   r.SourceColumn,
			TargetColumn:   r.TargetColumn,
			DerivationType: r.DerivationType,
		}
		if r.Expression != nil {
			cr.Expression = *r.Expression
		}
		if r.Context != nil {
			cr.Context = *r.Context
		}
		if r.Line != nil {
			cr.Line = int(*r.Line)
		}
		colRefs = append(colRefs, cr)
	}

	created, err := s.engine.BuildColumnLineage(ctx, rc.ProjectID, colRefs)
	if err != nil {
		return fmt.Errorf("build column lineage: %w", err)
	}

	rc.EdgesFound += created

	// Clean up staging rows now that lineage edges have been written.
	_ = s.store.DeleteColumnReferencesByIndexRun(ctx, rc.IndexRunID)

	return nil
}
