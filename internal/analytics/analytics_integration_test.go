//go:build integration

package analytics

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

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

// seedTestGraph creates a small graph: 4 symbols (table, procedure, class, method) + 3 edges.
func seedTestGraph(t *testing.T, s *store.Store) (projectID uuid.UUID, cleanup func()) {
	t.Helper()
	ctx := context.Background()
	slug := fmt.Sprintf("test-analytics-%s", t.Name())

	proj, err := s.CreateProject(ctx, postgres.CreateProjectParams{
		Name:     "Test Analytics Project",
		Slug:     slug,
		TenantID: uuid.MustParse("00000000-0000-0000-0000-000000000099"),
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	source, err := s.CreateSource(ctx, postgres.CreateSourceParams{
		ProjectID:  proj.ID,
		Name:       "test-source",
		SourceType: "upload",
		Config:     []byte("{}"),
	})
	if err != nil {
		t.Fatalf("create source: %v", err)
	}

	file, err := s.UpsertFile(ctx, postgres.UpsertFileParams{
		ProjectID: proj.ID, SourceID: source.ID,
		Path: "test.sql", Language: "tsql", SizeBytes: 1000, Hash: "abc123",
	})
	if err != nil {
		t.Fatalf("create file: %v", err)
	}

	file2, err := s.UpsertFile(ctx, postgres.UpsertFileParams{
		ProjectID: proj.ID, SourceID: source.ID,
		Path: "app.go", Language: "go", SizeBytes: 2000, Hash: "def456",
	})
	if err != nil {
		t.Fatalf("create file2: %v", err)
	}

	table, err := s.CreateSymbol(ctx, postgres.CreateSymbolParams{
		ProjectID: proj.ID, FileID: file.ID,
		Name: "Customers", QualifiedName: "dbo.Customers",
		Kind: "table", Language: "tsql", StartLine: 1, EndLine: 10,
	})
	if err != nil {
		t.Fatalf("create table symbol: %v", err)
	}

	proc, err := s.CreateSymbol(ctx, postgres.CreateSymbolParams{
		ProjectID: proj.ID, FileID: file.ID,
		Name: "GetCustomer", QualifiedName: "dbo.GetCustomer",
		Kind: "procedure", Language: "tsql", StartLine: 20, EndLine: 40,
	})
	if err != nil {
		t.Fatalf("create proc symbol: %v", err)
	}

	class, err := s.CreateSymbol(ctx, postgres.CreateSymbolParams{
		ProjectID: proj.ID, FileID: file2.ID,
		Name: "CustomerRepository", QualifiedName: "app.repository.CustomerRepository",
		Kind: "class", Language: "go", StartLine: 1, EndLine: 80,
	})
	if err != nil {
		t.Fatalf("create class symbol: %v", err)
	}

	method, err := s.CreateSymbol(ctx, postgres.CreateSymbolParams{
		ProjectID: proj.ID, FileID: file2.ID,
		Name: "GetByID", QualifiedName: "app.repository.CustomerRepository.GetByID",
		Kind: "method", Language: "go", StartLine: 10, EndLine: 25,
	})
	if err != nil {
		t.Fatalf("create method symbol: %v", err)
	}

	// Edges: proc -> table (reads_from), class -> proc (calls), method -> table (reads_from)
	s.CreateSymbolEdge(ctx, postgres.CreateSymbolEdgeParams{
		ProjectID: proj.ID, SourceID: proc.ID, TargetID: table.ID, EdgeType: "reads_from",
	})
	s.CreateSymbolEdge(ctx, postgres.CreateSymbolEdgeParams{
		ProjectID: proj.ID, SourceID: class.ID, TargetID: proc.ID, EdgeType: "calls",
	})
	s.CreateSymbolEdge(ctx, postgres.CreateSymbolEdgeParams{
		ProjectID: proj.ID, SourceID: method.ID, TargetID: table.ID, EdgeType: "reads_from",
	})

	cleanup = func() {
		s.Pool().Exec(ctx, "DELETE FROM symbol_edges WHERE project_id = $1", proj.ID)
		s.Pool().Exec(ctx, "DELETE FROM symbols WHERE project_id = $1", proj.ID)
		s.Pool().Exec(ctx, "DELETE FROM files WHERE project_id = $1", proj.ID)
		s.Pool().Exec(ctx, "DELETE FROM sources WHERE project_id = $1", proj.ID)
		s.Pool().Exec(ctx, "DELETE FROM project_analytics WHERE project_id = $1", proj.ID)
		s.Pool().Exec(ctx, "DELETE FROM projects WHERE id = $1", proj.ID)
	}

	return proj.ID, cleanup
}

func TestComputeDegrees_Integration(t *testing.T) {
	s := setupStore(t)
	projID, cleanup := seedTestGraph(t, s)
	defer cleanup()

	engine := NewEngine(s, slog.Default())
	ctx := context.Background()

	if err := engine.ComputeDegrees(ctx, projID); err != nil {
		t.Fatalf("ComputeDegrees: %v", err)
	}

	syms, err := s.ListSymbolsByProject(ctx, projID)
	if err != nil {
		t.Fatalf("list symbols: %v", err)
	}

	for _, sym := range syms {
		var meta map[string]any
		if len(sym.Metadata) > 0 {
			json.Unmarshal(sym.Metadata, &meta)
		}
		if sym.Name == "Customers" {
			inDeg, _ := meta["in_degree"].(float64)
			if inDeg != 2 {
				t.Errorf("Customers should have in_degree=2, got %v", inDeg)
			}
			outDeg, _ := meta["out_degree"].(float64)
			if outDeg != 0 {
				t.Errorf("Customers should have out_degree=0, got %v", outDeg)
			}
		}
		if sym.Name == "GetCustomer" {
			outDeg, _ := meta["out_degree"].(float64)
			if outDeg != 1 {
				t.Errorf("GetCustomer should have out_degree=1, got %v", outDeg)
			}
		}
	}
}

func TestComputePageRank_Integration(t *testing.T) {
	s := setupStore(t)
	projID, cleanup := seedTestGraph(t, s)
	defer cleanup()

	engine := NewEngine(s, slog.Default())
	ctx := context.Background()

	if err := engine.ComputePageRank(ctx, projID); err != nil {
		t.Fatalf("ComputePageRank: %v", err)
	}

	syms, err := s.ListSymbolsByProject(ctx, projID)
	if err != nil {
		t.Fatalf("list symbols: %v", err)
	}

	var customersPR float64
	for _, sym := range syms {
		var meta map[string]any
		if len(sym.Metadata) > 0 {
			json.Unmarshal(sym.Metadata, &meta)
		}
		if pr, ok := meta["pagerank"].(float64); ok {
			if pr <= 0 {
				t.Errorf("symbol %s has non-positive pagerank: %f", sym.Name, pr)
			}
			if sym.Name == "Customers" {
				customersPR = pr
			}
		}
	}

	if customersPR == 0 {
		t.Error("Customers should have a positive PageRank")
	}
}

func TestComputeLayers_Integration(t *testing.T) {
	s := setupStore(t)
	projID, cleanup := seedTestGraph(t, s)
	defer cleanup()

	engine := NewEngine(s, slog.Default())
	ctx := context.Background()

	if err := engine.ComputeLayers(ctx, projID); err != nil {
		t.Fatalf("ComputeLayers: %v", err)
	}

	syms, err := s.ListSymbolsByProject(ctx, projID)
	if err != nil {
		t.Fatalf("list symbols: %v", err)
	}

	expected := map[string]string{
		"Customers":          "data",
		"GetCustomer":        "data",
		"CustomerRepository": "data",
		"GetByID":            "data",
	}

	for _, sym := range syms {
		var meta map[string]any
		if len(sym.Metadata) > 0 {
			json.Unmarshal(sym.Metadata, &meta)
		}
		if expectedLayer, ok := expected[sym.Name]; ok {
			if layer, _ := meta["layer"].(string); layer != expectedLayer {
				t.Errorf("symbol %s: expected layer=%s, got %s", sym.Name, expectedLayer, layer)
			}
		}
	}
}

func TestComputeProjectSummaries_Integration(t *testing.T) {
	s := setupStore(t)
	projID, cleanup := seedTestGraph(t, s)
	defer cleanup()

	engine := NewEngine(s, slog.Default())
	ctx := context.Background()

	if err := engine.ComputeProjectSummaries(ctx, projID); err != nil {
		t.Fatalf("ComputeProjectSummaries: %v", err)
	}

	analytics, err := s.GetProjectAnalytics(ctx, postgres.GetProjectAnalyticsParams{
		ProjectID: projID,
		Scope:     "project",
		ScopeID:   "overview",
	})
	if err != nil {
		t.Fatalf("get project analytics: %v", err)
	}

	if analytics.Summary == nil || *analytics.Summary == "" {
		t.Error("project summary should not be empty")
	}

	var data map[string]any
	json.Unmarshal(analytics.Analytics, &data)
	if totalSymbols, _ := data["total_symbols"].(float64); totalSymbols != 4 {
		t.Errorf("total_symbols should be 4, got %v", totalSymbols)
	}
}

func TestComputeCrossLanguageBridges_Integration(t *testing.T) {
	s := setupStore(t)
	projID, cleanup := seedTestGraph(t, s)
	defer cleanup()

	engine := NewEngine(s, slog.Default())
	ctx := context.Background()

	if err := engine.ComputeCrossLanguageBridges(ctx, projID); err != nil {
		t.Fatalf("ComputeCrossLanguageBridges: %v", err)
	}

	bridges, err := s.ListProjectAnalyticsByScope(ctx, postgres.ListProjectAnalyticsByScopeParams{
		ProjectID: projID,
		Scope:     "bridge",
	})
	if err != nil {
		t.Fatalf("list bridge analytics: %v", err)
	}

	if len(bridges) == 0 {
		t.Error("should find cross-language bridges (go → tsql)")
	}

	found := false
	for _, b := range bridges {
		if b.ScopeID == "go→tsql" {
			found = true
			break
		}
	}
	if !found {
		t.Error("should have a go→tsql bridge")
	}
}

func TestComputeAll_Integration(t *testing.T) {
	s := setupStore(t)
	projID, cleanup := seedTestGraph(t, s)
	defer cleanup()

	engine := NewEngine(s, slog.Default())
	ctx := context.Background()

	if err := engine.ComputeAll(ctx, projID); err != nil {
		t.Fatalf("ComputeAll: %v", err)
	}

	syms, err := s.ListSymbolsByProject(ctx, projID)
	if err != nil {
		t.Fatalf("list symbols: %v", err)
	}

	for _, sym := range syms {
		if len(sym.Metadata) == 0 {
			t.Errorf("symbol %s has no metadata after ComputeAll", sym.Name)
			continue
		}
		var meta map[string]any
		json.Unmarshal(sym.Metadata, &meta)

		for _, field := range []string{"in_degree", "out_degree", "pagerank", "layer"} {
			if _, ok := meta[field]; !ok {
				t.Errorf("symbol %s missing %s", sym.Name, field)
			}
		}
	}
}
