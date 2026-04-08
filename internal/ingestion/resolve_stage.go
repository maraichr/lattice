package ingestion

import (
	"context"
	"fmt"

	"github.com/maraichr/lattice/internal/resolver"
)

// ResolveStage performs cross-file symbol resolution using DB-driven lookups.
// All symbols are now persisted to the database by parse workers before this
// stage runs, so no in-memory parse results are needed.
type ResolveStage struct {
	engine *resolver.Engine
}

func NewResolveStage(engine *resolver.Engine) *ResolveStage {
	return &ResolveStage{engine: engine}
}

func (s *ResolveStage) Name() string { return "resolve" }

func (s *ResolveStage) Execute(ctx context.Context, rc *IndexRunContext) error {
	created, err := s.engine.ResolveProject(ctx, rc.ProjectID, rc.IndexRunID)
	if err != nil {
		return fmt.Errorf("resolve: %w", err)
	}

	rc.EdgesFound += created
	return nil
}
