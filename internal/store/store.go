package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	pgvector "github.com/pgvector/pgvector-go"

	"github.com/maraichr/lattice/internal/store/postgres"
)

type Store struct {
	*postgres.Queries
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{
		Queries: postgres.New(pool),
		pool:    pool,
	}
}

func (s *Store) Pool() *pgxpool.Pool {
	return s.pool
}

func (s *Store) WithTx(ctx context.Context, fn func(*postgres.Queries) error) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	if err := fn(s.Queries.WithTx(tx)); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

const upsertEmbeddingSQL = `
INSERT INTO symbol_embeddings (symbol_id, embedding, model)
VALUES ($1, $2, $3)
ON CONFLICT (symbol_id) DO UPDATE SET embedding = $2, model = $3, created_at = now()
`

// embeddingsBatchSize is the maximum number of upserts sent in a single pgx.Batch.
// pgx pipelines all queries in a batch over a single TCP round-trip.
const embeddingsBatchSize = 500

// UpsertSymbolEmbeddingsBatch bulk-upserts symbol embeddings using pgx pipelined
// batches, replacing the per-row Exec pattern with a single network round-trip per
// embeddingsBatchSize rows.
func (s *Store) UpsertSymbolEmbeddingsBatch(ctx context.Context, symbolIDs []uuid.UUID, vectors []pgvector.Vector, model string) error {
	if len(symbolIDs) == 0 {
		return nil
	}
	if len(symbolIDs) != len(vectors) {
		return fmt.Errorf("symbol IDs and vectors length mismatch: %d vs %d", len(symbolIDs), len(vectors))
	}

	for start := 0; start < len(symbolIDs); start += embeddingsBatchSize {
		end := min(start+embeddingsBatchSize, len(symbolIDs))

		batch := &pgx.Batch{}
		for i := start; i < end; i++ {
			batch.Queue(upsertEmbeddingSQL, symbolIDs[i], vectors[i], model)
		}

		results := s.pool.SendBatch(ctx, batch)
		for i := start; i < end; i++ {
			if _, err := results.Exec(); err != nil {
				results.Close()
				return fmt.Errorf("upsert embedding %d: %w", i, err)
			}
		}
		if err := results.Close(); err != nil {
			return fmt.Errorf("close batch results: %w", err)
		}
	}
	return nil
}
