package embedding

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	pgvector "github.com/pgvector/pgvector-go"

	"github.com/maraichr/lattice/internal/store"
)

// EmbedSymbols generates and stores embeddings for all symbols in a project
// that don't already have them. Returns the number of symbols embedded.
//
// API calls are parallelised inside EmbedBatch (bounded by provider concurrency
// limits). Database writes use a single pgx pipelined batch per
// embeddingsBatchSize rows rather than one round-trip per symbol.
func EmbedSymbols(ctx context.Context, client Embedder, s *store.Store, projectID uuid.UUID, logger *slog.Logger) (int, error) {
	symbols, err := s.ListSymbolsWithoutEmbeddings(ctx, projectID)
	if err != nil {
		return 0, fmt.Errorf("list symbols without embeddings: %w", err)
	}
	if len(symbols) == 0 {
		return 0, nil
	}

	logger.Info("embedding symbols", slog.Int("count", len(symbols)))

	// Build text representations.
	texts := make([]string, len(symbols))
	for i, sym := range symbols {
		texts[i] = BuildEmbeddingText(sym)
	}

	// Generate embeddings — concurrent API calls happen inside EmbedBatch.
	embeddings, err := client.EmbedBatch(ctx, texts, "search_document")
	if err != nil {
		return 0, fmt.Errorf("embed batch: %w", err)
	}
	if len(embeddings) != len(symbols) {
		return 0, fmt.Errorf("embedding count mismatch: got %d, expected %d", len(embeddings), len(symbols))
	}

	// Build flat slices for the bulk upsert.
	symbolIDs := make([]uuid.UUID, len(symbols))
	vectors := make([]pgvector.Vector, len(symbols))
	for i, sym := range symbols {
		symbolIDs[i] = sym.ID
		vectors[i] = pgvector.NewVector(embeddings[i])
	}

	// Persist — single pgx pipelined batch per chunk instead of N round-trips.
	if err := s.UpsertSymbolEmbeddingsBatch(ctx, symbolIDs, vectors, client.ModelID()); err != nil {
		return 0, fmt.Errorf("upsert embeddings batch: %w", err)
	}

	return len(symbols), nil
}
