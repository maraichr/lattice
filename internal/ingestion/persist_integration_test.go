//go:build integration

package ingestion

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/maraichr/lattice/internal/parser"
	"github.com/maraichr/lattice/internal/store"
	"github.com/maraichr/lattice/internal/store/postgres"
)

func setupStore(t *testing.T) *store.Store {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Fatal("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Skipf("postgres not available: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		t.Skipf("postgres ping failed: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	return store.New(pool)
}

// TestPersistResults_StableIDsPreserveCrossFileEdges reproduces the bug where
// re-indexing a single file used to delete-then-insert its symbols, minting new
// UUIDs and cascade-deleting cross-file edges that pointed at them. With the
// reconcile-based upsert, the symbol keeps its ID and an edge B->A from an
// unchanged file survives a re-index of file A.
func TestPersistResults_StableIDsPreserveCrossFileEdges(t *testing.T) {
	s := setupStore(t)
	ctx := context.Background()

	proj, err := s.CreateProject(ctx, postgres.CreateProjectParams{
		Name:     "Persist Reconcile Test",
		Slug:     fmt.Sprintf("test-persist-%s", t.Name()),
		TenantID: uuid.MustParse("00000000-0000-0000-0000-000000000099"),
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	source, err := s.CreateSource(ctx, postgres.CreateSourceParams{
		ProjectID: proj.ID, Name: "src", SourceType: "upload", Config: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	run, err := s.CreateIndexRun(ctx, postgres.CreateIndexRunParams{ProjectID: proj.ID, SourceID: uuidToPg(source.ID)})
	if err != nil {
		t.Fatalf("create index run: %v", err)
	}

	t.Cleanup(func() {
		s.Pool().Exec(ctx, "DELETE FROM symbol_edges WHERE project_id = $1", proj.ID)
		s.Pool().Exec(ctx, "DELETE FROM symbols WHERE project_id = $1", proj.ID)
		s.Pool().Exec(ctx, "DELETE FROM files WHERE project_id = $1", proj.ID)
		s.Pool().Exec(ctx, "DELETE FROM graph_deletions WHERE project_id = $1", proj.ID)
		s.Pool().Exec(ctx, "DELETE FROM index_runs WHERE project_id = $1", proj.ID)
		s.Pool().Exec(ctx, "DELETE FROM sources WHERE project_id = $1", proj.ID)
		s.Pool().Exec(ctx, "DELETE FROM projects WHERE id = $1", proj.ID)
	})

	// File A defines dbo.ProcA; File B defines dbo.ProcB.
	fileA := parser.FileResult{
		ProjectID: proj.ID, SourceID: source.ID, Path: "a.sql", Language: "tsql", Hash: "a1",
		Symbols: []parser.Symbol{{Name: "ProcA", QualifiedName: "dbo.ProcA", Kind: "procedure", Language: "tsql", StartLine: 1, EndLine: 5}},
	}
	fileB := parser.FileResult{
		ProjectID: proj.ID, SourceID: source.ID, Path: "b.sql", Language: "tsql", Hash: "b1",
		Symbols: []parser.Symbol{{Name: "ProcB", QualifiedName: "dbo.ProcB", Kind: "procedure", Language: "tsql", StartLine: 1, EndLine: 5}},
	}

	if _, _, _, err := PersistResults(ctx, s, run.ID, []parser.FileResult{fileA, fileB}); err != nil {
		t.Fatalf("initial persist: %v", err)
	}

	procA, err := s.GetSymbolByQualifiedName(ctx, postgres.GetSymbolByQualifiedNameParams{ProjectID: proj.ID, QualifiedName: "dbo.ProcA"})
	if err != nil {
		t.Fatalf("get ProcA: %v", err)
	}
	procB, err := s.GetSymbolByQualifiedName(ctx, postgres.GetSymbolByQualifiedNameParams{ProjectID: proj.ID, QualifiedName: "dbo.ProcB"})
	if err != nil {
		t.Fatalf("get ProcB: %v", err)
	}
	originalProcAID := procA.ID

	// Cross-file edge B -> A (resolver would normally create this).
	if _, err := s.CreateSymbolEdge(ctx, postgres.CreateSymbolEdgeParams{
		ProjectID: proj.ID, SourceID: procB.ID, TargetID: procA.ID, EdgeType: "calls",
	}); err != nil {
		t.Fatalf("create cross-file edge: %v", err)
	}

	// Re-index ONLY file A (incremental re-parse), same symbol content.
	if _, _, _, err := PersistResults(ctx, s, run.ID, []parser.FileResult{fileA}); err != nil {
		t.Fatalf("re-persist file A: %v", err)
	}

	// ProcA must keep its ID...
	procAAfter, err := s.GetSymbolByQualifiedName(ctx, postgres.GetSymbolByQualifiedNameParams{ProjectID: proj.ID, QualifiedName: "dbo.ProcA"})
	if err != nil {
		t.Fatalf("get ProcA after re-index: %v", err)
	}
	if procAAfter.ID != originalProcAID {
		t.Fatalf("ProcA ID changed on re-index: %s -> %s (cross-file edges would be lost)", originalProcAID, procAAfter.ID)
	}

	// ...and the cross-file edge B -> A must still exist.
	incoming, err := s.GetIncomingEdges(ctx, originalProcAID)
	if err != nil {
		t.Fatalf("get incoming edges: %v", err)
	}
	found := false
	for _, e := range incoming {
		if e.SourceID == procB.ID && e.EdgeType == "calls" {
			found = true
		}
	}
	if !found {
		t.Fatalf("cross-file edge B->A was dropped on incremental re-index of A")
	}
}

// TestPersistResults_ReconcileStagesDeletions verifies that a symbol removed from a
// re-parsed file is deleted from Postgres and staged for Neo4j pruning.
func TestPersistResults_ReconcileStagesDeletions(t *testing.T) {
	s := setupStore(t)
	ctx := context.Background()

	proj, err := s.CreateProject(ctx, postgres.CreateProjectParams{
		Name:     "Persist Deletion Test",
		Slug:     fmt.Sprintf("test-persist-del-%s", t.Name()),
		TenantID: uuid.MustParse("00000000-0000-0000-0000-000000000099"),
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	source, err := s.CreateSource(ctx, postgres.CreateSourceParams{
		ProjectID: proj.ID, Name: "src", SourceType: "upload", Config: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	run, err := s.CreateIndexRun(ctx, postgres.CreateIndexRunParams{ProjectID: proj.ID, SourceID: uuidToPg(source.ID)})
	if err != nil {
		t.Fatalf("create index run: %v", err)
	}

	t.Cleanup(func() {
		s.Pool().Exec(ctx, "DELETE FROM symbol_edges WHERE project_id = $1", proj.ID)
		s.Pool().Exec(ctx, "DELETE FROM symbols WHERE project_id = $1", proj.ID)
		s.Pool().Exec(ctx, "DELETE FROM files WHERE project_id = $1", proj.ID)
		s.Pool().Exec(ctx, "DELETE FROM graph_deletions WHERE project_id = $1", proj.ID)
		s.Pool().Exec(ctx, "DELETE FROM index_runs WHERE project_id = $1", proj.ID)
		s.Pool().Exec(ctx, "DELETE FROM sources WHERE project_id = $1", proj.ID)
		s.Pool().Exec(ctx, "DELETE FROM projects WHERE id = $1", proj.ID)
	})

	withTwo := parser.FileResult{
		ProjectID: proj.ID, SourceID: source.ID, Path: "c.sql", Language: "tsql", Hash: "c1",
		Symbols: []parser.Symbol{
			{Name: "Keep", QualifiedName: "dbo.Keep", Kind: "procedure", Language: "tsql", StartLine: 1, EndLine: 5},
			{Name: "Drop", QualifiedName: "dbo.Drop", Kind: "procedure", Language: "tsql", StartLine: 6, EndLine: 9},
		},
	}
	if _, _, _, err := PersistResults(ctx, s, run.ID, []parser.FileResult{withTwo}); err != nil {
		t.Fatalf("initial persist: %v", err)
	}
	dropped, err := s.GetSymbolByQualifiedName(ctx, postgres.GetSymbolByQualifiedNameParams{ProjectID: proj.ID, QualifiedName: "dbo.Drop"})
	if err != nil {
		t.Fatalf("get Drop: %v", err)
	}

	// Re-parse the file without "Drop".
	withOne := withTwo
	withOne.Symbols = withTwo.Symbols[:1]
	if _, _, _, err := PersistResults(ctx, s, run.ID, []parser.FileResult{withOne}); err != nil {
		t.Fatalf("re-persist: %v", err)
	}

	if _, err := s.GetSymbolByQualifiedName(ctx, postgres.GetSymbolByQualifiedNameParams{ProjectID: proj.ID, QualifiedName: "dbo.Drop"}); err == nil {
		t.Fatalf("expected dbo.Drop to be deleted from Postgres")
	}

	dels, err := s.ListGraphDeletionsByIndexRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("list graph deletions: %v", err)
	}
	staged := false
	for _, d := range dels {
		if d.NodeType == "symbol" && d.NodeID == dropped.ID {
			staged = true
		}
	}
	if !staged {
		t.Fatalf("removed symbol was not staged for Neo4j pruning")
	}
}

// uuidToPg converts a uuid.UUID to the pgtype.UUID expected by source_id params.
func uuidToPg(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}
