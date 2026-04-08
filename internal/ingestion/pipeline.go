package ingestion

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/maraichr/lattice/internal/store"
	"github.com/maraichr/lattice/internal/store/postgres"
)

// Pipeline orchestrates the indexing stages for each ingestion job.
//
// # Two-phase execution model
//
// Phase 1 (initial trigger — any trigger except "parse_complete"):
//   CloneStage → ParseStage (enqueues chunks to lattice:parse_tasks, sets rc.TotalChunks)
//   Pipeline halts here. ParseWorkers run concurrently; the last worker re-enqueues
//   an IngestMessage with Trigger="parse_complete" back to lattice:ingest.
//
// Phase 2 (Trigger == "parse_complete"):
//   Clone and Parse stages are skipped. The pipeline resumes from ResolveStage onward.
//   Stages that should only run in phase 2 must implement SkipInPhase1() bool returning true.
type Pipeline struct {
	store  *store.Store
	stages []Stage
	logger *slog.Logger
}

func NewPipeline(s *store.Store, stages []Stage, logger *slog.Logger) *Pipeline {
	return &Pipeline{store: s, stages: stages, logger: logger}
}

// Run processes a single ingestion message through the appropriate pipeline phase.
func (p *Pipeline) Run(ctx context.Context, msg IngestMessage) error {
	p.logger.Info("pipeline started",
		slog.String("index_run_id", msg.IndexRunID.String()),
		slog.String("source_type", msg.SourceType),
		slog.String("trigger", msg.Trigger))

	isParseComplete := msg.Trigger == "parse_complete"

	// On initial trigger: mark as running. On parse_complete: status is already running.
	if !isParseComplete {
		if err := p.store.UpdateIndexRunStatus(ctx, postgres.UpdateIndexRunStatusParams{
			ID:     msg.IndexRunID,
			Status: "running",
		}); err != nil {
			return fmt.Errorf("update status to running: %w", err)
		}
	}

	rc := &IndexRunContext{
		IndexRunID: msg.IndexRunID,
		ProjectID:  msg.ProjectID,
		SourceID:   msg.SourceID,
		SourceType: msg.SourceType,
		Trigger:    msg.Trigger,
	}

	// Load project settings.
	if proj, err := p.store.GetProjectByID(ctx, msg.ProjectID); err == nil && len(proj.Settings) > 0 {
		var settings struct {
			LineageExcludePaths []string `json:"lineage_exclude_paths"`
		}
		if json.Unmarshal(proj.Settings, &settings) == nil {
			rc.LineageExcludePaths = settings.LineageExcludePaths
		}
	}

	for _, stage := range p.stages {
		// Phase routing: skip pre-parse stages when resuming after parse_complete.
		if isParseComplete && isPreParseStage(stage.Name()) {
			p.logger.Info("stage skipped (parse_complete phase)",
				slog.String("stage", stage.Name()),
				slog.String("index_run_id", msg.IndexRunID.String()))
			continue
		}

		p.logger.Info("stage started",
			slog.String("stage", stage.Name()),
			slog.String("index_run_id", msg.IndexRunID.String()))

		if err := stage.Execute(ctx, rc); err != nil {
			errMsg := err.Error()
			_ = p.store.UpdateIndexRunStatus(ctx, postgres.UpdateIndexRunStatusParams{
				ID:           msg.IndexRunID,
				Status:       "failed",
				ErrorMessage: &errMsg,
			})
			return fmt.Errorf("stage %s failed: %w", stage.Name(), err)
		}

		p.logger.Info("stage completed",
			slog.String("stage", stage.Name()),
			slog.String("index_run_id", msg.IndexRunID.String()))

		// After the parse stage in phase 1, chunks have been enqueued. Halt here;
		// the pipeline will resume when parse_complete is received.
		if !isParseComplete && stage.Name() == "parse" && rc.TotalChunks > 0 {
			p.logger.Info("parse chunks enqueued, awaiting completion",
				slog.String("index_run_id", msg.IndexRunID.String()),
				slog.Int("total_chunks", rc.TotalChunks))
			return nil
		}
	}

	// Save commit SHA for incremental indexing on next run.
	if rc.CurrentSHA != "" {
		_ = p.store.UpdateSourceLastCommitSHA(ctx, postgres.UpdateSourceLastCommitSHAParams{
			ID:            rc.SourceID,
			LastCommitSha: &rc.CurrentSHA,
		})
	}

	// Update stats and mark complete.
	_ = p.store.UpdateIndexRunStats(ctx, postgres.UpdateIndexRunStatsParams{
		ID:             msg.IndexRunID,
		FilesProcessed: int32(rc.FilesProcessed),
		SymbolsFound:   int32(rc.SymbolsFound),
		EdgesFound:     int32(rc.EdgesFound),
	})

	if err := p.store.UpdateIndexRunStatus(ctx, postgres.UpdateIndexRunStatusParams{
		ID:     msg.IndexRunID,
		Status: "completed",
	}); err != nil {
		return fmt.Errorf("update status to completed: %w", err)
	}

	p.logger.Info("pipeline completed",
		slog.String("index_run_id", msg.IndexRunID.String()),
		slog.Int("files", rc.FilesProcessed),
		slog.Int("symbols", rc.SymbolsFound),
		slog.Int("edges", rc.EdgesFound))

	return nil
}

// isPreParseStage returns true for stages that should only run in phase 1
// (the initial clone + parse pass) and must be skipped on parse_complete.
func isPreParseStage(name string) bool {
	return name == "clone" || name == "parse"
}

// NoOpStage is a placeholder stage that just logs.
type NoOpStage struct {
	name string
}

func NewNoOpStage(name string) *NoOpStage {
	return &NoOpStage{name: name}
}

func (s *NoOpStage) Name() string { return s.name }

func (s *NoOpStage) Execute(_ context.Context, _ *IndexRunContext) error {
	return nil
}
