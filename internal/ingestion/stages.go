package ingestion

import (
	"context"

	"github.com/google/uuid"
)

// Stage represents a step in the indexing pipeline.
type Stage interface {
	Name() string
	Execute(ctx context.Context, rc *IndexRunContext) error
}

// IndexRunContext carries state through the pipeline stages.
type IndexRunContext struct {
	IndexRunID uuid.UUID
	ProjectID  uuid.UUID
	SourceID   uuid.UUID
	SourceType string
	Trigger    string

	// Set by clone stage
	WorkDir string

	// Incremental indexing (set by clone stage for git sources)
	Incremental  bool
	PreviousSHA  string
	CurrentSHA   string
	ChangedFiles []string // relative paths of modified/added files
	DeletedFiles []string // relative paths of deleted files

	// Set by parse stage (chunked)
	FilesProcessed int
	SymbolsFound   int
	EdgesFound     int

	// TotalChunks is set by ParseStage after enqueuing all chunks to lattice:parse_tasks.
	// The orchestrator waits until all chunks are acknowledged before advancing.
	TotalChunks int

	// Optional: path patterns to exclude from column lineage (from project.settings lineage_exclude_paths)
	LineageExcludePaths []string
}
