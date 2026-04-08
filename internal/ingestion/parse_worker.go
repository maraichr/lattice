package ingestion

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/valkey-io/valkey-go"

	"github.com/maraichr/lattice/internal/parser"
	"github.com/maraichr/lattice/internal/store"
)

// ParseWorker executes parse chunks delivered by the lattice:parse_tasks stream.
// Each worker parses a subset of files and persists results directly to the database,
// avoiding the need to hold all parse results in memory on a single process.
type ParseWorker struct {
	registry *parser.Registry
	store    *store.Store
	client   valkey.Client
	logger   *slog.Logger
}

func NewParseWorker(registry *parser.Registry, s *store.Store, client valkey.Client, logger *slog.Logger) *ParseWorker {
	return &ParseWorker{registry: registry, store: s, client: client, logger: logger}
}

// Handle processes a single parse chunk. It parses all files in the chunk,
// persists them immediately, and signals chunk completion. When the final chunk
// for an index run completes, it re-enqueues a "resume" message to lattice:ingest
// so the pipeline orchestrator can advance to the resolve stage.
func (w *ParseWorker) Handle(ctx context.Context, task ParseTaskMessage) error {
	w.logger.Info("parse chunk started",
		slog.String("index_run_id", task.IndexRunID.String()),
		slog.Int("chunk", task.ChunkIndex+1),
		slog.Int("total_chunks", task.TotalChunks),
		slog.Int("files", len(task.Files)))

	var results []parser.FileResult

	for _, relPath := range task.Files {
		absPath := filepath.Join(task.WorkDir, relPath)
		info, err := os.Stat(absPath)
		if err != nil {
			continue
		}

		fr := w.parseFile(task, absPath, relPath, info)
		if fr != nil {
			results = append(results, *fr)
		}
	}

	files, symbols, edges, err := PersistResults(ctx, w.store, task.IndexRunID, results)
	if err != nil {
		return fmt.Errorf("persist chunk %d: %w", task.ChunkIndex, err)
	}

	w.logger.Info("parse chunk persisted",
		slog.String("index_run_id", task.IndexRunID.String()),
		slog.Int("chunk", task.ChunkIndex+1),
		slog.Int("files", files),
		slog.Int("symbols", symbols),
		slog.Int("edges", edges))

	// Signal chunk completion and check if this was the last one.
	if err := w.signalChunkComplete(ctx, task); err != nil {
		return fmt.Errorf("signal chunk complete: %w", err)
	}

	return nil
}

// parseFile reads and parses a single file, returning nil if no parser handles it.
func (w *ParseWorker) parseFile(task ParseTaskMessage, absPath, relPath string, info os.FileInfo) *parser.FileResult {
	p := w.registry.ForFile(absPath)
	if p == nil {
		return nil
	}

	content, err := os.ReadFile(absPath)
	if err != nil {
		return nil
	}

	// Strip UTF-8 BOM if present (common in DNN Platform SQL files).
	if len(content) >= 3 && content[0] == 0xEF && content[1] == 0xBB && content[2] == 0xBF {
		content = content[3:]
	}

	ext := strings.ToLower(filepath.Ext(absPath))
	language := "sql"
	if ext == ".sql" || ext == ".sqldataprovider" {
		language = parser.DetectDialect(content, ext)
	} else if ext != "" {
		// Use the extension minus the dot as the language for non-SQL files.
		language = strings.TrimPrefix(ext, ".")
	}

	skipColumnLineage := isMigrationOrSchemaFile(relPath, task.LineageExcludePaths)

	input := parser.FileInput{
		Path:              relPath,
		Content:           content,
		Language:          language,
		SkipColumnLineage: skipColumnLineage,
	}

	result, err := p.Parse(input)
	if err != nil {
		w.logger.Warn("parse file failed",
			slog.String("path", relPath),
			slog.String("error", err.Error()))
		return nil
	}

	hash := fmt.Sprintf("%x", sha256.Sum256(content))

	return &parser.FileResult{
		ProjectID:        task.ProjectID,
		SourceID:         task.SourceID,
		Path:             relPath,
		Language:         language,
		SizeBytes:        info.Size(),
		Hash:             hash,
		Symbols:          result.Symbols,
		References:       result.References,
		ColumnReferences: result.ColumnReferences,
	}
}

// signalChunkComplete increments the Valkey completion counter for the index run.
// If all chunks are done, it publishes a resume message to the main ingest stream.
func (w *ParseWorker) signalChunkComplete(ctx context.Context, task ParseTaskMessage) error {
	counterKey := parseChunkCounterKey(task.IndexRunID.String())

	resp := w.client.Do(ctx, w.client.B().Incr().Key(counterKey).Build())
	if err := resp.Error(); err != nil {
		return fmt.Errorf("incr chunk counter: %w", err)
	}

	completed, err := resp.AsInt64()
	if err != nil {
		return fmt.Errorf("read chunk counter: %w", err)
	}

	if int(completed) < task.TotalChunks {
		// Not done yet; other chunks still running.
		return nil
	}

	// All chunks finished — clean up the counter and signal the orchestrator.
	w.client.Do(ctx, w.client.B().Del().Key(counterKey).Build())

	w.logger.Info("all parse chunks complete, resuming pipeline",
		slog.String("index_run_id", task.IndexRunID.String()),
		slog.Int("total_chunks", task.TotalChunks))

	resumeMsg := IngestMessage{
		IndexRunID: task.IndexRunID,
		ProjectID:  task.ProjectID,
		SourceID:   task.SourceID,
		SourceType: task.SourceType,
		Trigger:    "parse_complete",
	}
	producer := NewProducer(w.client)
	if _, err := producer.Enqueue(ctx, resumeMsg); err != nil {
		return fmt.Errorf("enqueue resume message: %w", err)
	}

	return nil
}

// parseChunkCounterKey returns the Valkey key used to track completed parse chunks
// for a given index run.
func parseChunkCounterKey(indexRunID string) string {
	return "lattice:parse:completed:" + indexRunID
}
