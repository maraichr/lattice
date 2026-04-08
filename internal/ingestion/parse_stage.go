package ingestion

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/maraichr/lattice/internal/store"
	"github.com/maraichr/lattice/internal/store/postgres"
	"github.com/valkey-io/valkey-go"
)

// ParseStage walks the work directory, groups files into chunks, and enqueues
// each chunk to the lattice:parse_tasks stream for distributed processing.
// It does not parse files directly; parse workers handle the actual parsing.
type ParseStage struct {
	store  *store.Store
	client valkey.Client
}

func NewParseStage(store *store.Store, client valkey.Client) *ParseStage {
	return &ParseStage{store: store, client: client}
}

func (s *ParseStage) Name() string { return "parse" }

func (s *ParseStage) Execute(ctx context.Context, rc *IndexRunContext) error {
	if rc.WorkDir == "" {
		return nil
	}

	// Handle incremental: delete symbols for removed files before enqueuing parse tasks.
	if rc.Incremental && len(rc.DeletedFiles) > 0 {
		for _, delPath := range rc.DeletedFiles {
			file, err := s.store.GetFileByPath(ctx, postgres.GetFileByPathParams{
				ProjectID: rc.ProjectID,
				SourceID:  rc.SourceID,
				Path:      delPath,
			})
			if err != nil {
				continue
			}
			_ = s.store.DeleteSymbolsByFileID(ctx, file.ID)
		}
	}

	// Collect the list of files to parse.
	var files []string
	if rc.Incremental && len(rc.ChangedFiles) > 0 {
		for _, relPath := range rc.ChangedFiles {
			absPath := filepath.Join(rc.WorkDir, relPath)
			if _, err := os.Stat(absPath); err == nil {
				files = append(files, relPath)
			}
		}
	} else {
		err := filepath.Walk(rc.WorkDir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return err
			}
			relPath, _ := filepath.Rel(rc.WorkDir, path)
			files = append(files, relPath)
			return nil
		})
		if err != nil {
			return fmt.Errorf("walk work dir: %w", err)
		}
	}

	if len(files) == 0 {
		return nil
	}

	// Split files into chunks and enqueue each chunk.
	chunks := chunkStrings(files, ParseTaskChunkSize)
	totalChunks := len(chunks)

	for i, chunk := range chunks {
		task := ParseTaskMessage{
			IndexRunID:          rc.IndexRunID,
			ProjectID:           rc.ProjectID,
			SourceID:            rc.SourceID,
			SourceType:          rc.SourceType,
			WorkDir:             rc.WorkDir,
			ChunkIndex:          i,
			TotalChunks:         totalChunks,
			Files:               chunk,
			LineageExcludePaths: rc.LineageExcludePaths,
		}
		if _, err := EnqueueParseTask(ctx, s.client, task); err != nil {
			return fmt.Errorf("enqueue parse chunk %d/%d: %w", i+1, totalChunks, err)
		}
	}

	rc.TotalChunks = totalChunks
	return nil
}

// chunkStrings splits a slice into sub-slices of at most size n.
func chunkStrings(items []string, n int) [][]string {
	var chunks [][]string
	for len(items) > 0 {
		end := n
		if end > len(items) {
			end = len(items)
		}
		chunks = append(chunks, items[:end])
		items = items[end:]
	}
	return chunks
}

// isMigrationOrSchemaFile returns true for paths that look like migration or schema DDL
// (e.g. Database/, Migrations/, Scripts/, *.Install.sql, *.Upgrade.sql), DNN-style paths
// (DNN Platform/, Dnn.AdminExperience/, Providers/), or that match project lineage_exclude_paths.
func isMigrationOrSchemaFile(relPath string, lineageExcludePaths []string) bool {
	norm := strings.ReplaceAll(relPath, "\\", "/")
	lower := strings.ToLower(norm)
	if strings.Contains(lower, "database/") || strings.Contains(lower, "migrations/") ||
		strings.Contains(lower, "scripts/") || strings.Contains(lower, "/database/") ||
		strings.Contains(lower, "/migrations/") || strings.Contains(lower, "/scripts/") {
		return true
	}
	if strings.Contains(lower, "dnn platform/") || strings.Contains(lower, "dnn.adminexperience/") ||
		strings.Contains(lower, "providers/") {
		return true
	}
	if strings.HasSuffix(lower, ".install.sql") || strings.HasSuffix(lower, ".upgrade.sql") {
		return true
	}
	for _, pattern := range lineageExcludePaths {
		matched, _ := filepath.Match(strings.ToLower(pattern), lower)
		if matched {
			return true
		}
		if !strings.ContainsAny(pattern, "*?[\\") && strings.Contains(lower, strings.ToLower(pattern)) {
			return true
		}
	}
	return false
}
