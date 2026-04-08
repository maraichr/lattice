//go:build integration

package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/valkey-io/valkey-go"

	"github.com/maraichr/lattice/internal/analytics"
	"github.com/maraichr/lattice/internal/mcp/session"
	"github.com/maraichr/lattice/internal/store"
	"github.com/maraichr/lattice/internal/store/postgres"
)

const evalModel = "minimax/minimax-m2.5"

func setupHarness(t *testing.T) (*Harness, *store.Store, *session.Manager) {
	t.Helper()

	// Load .env from project root
	_ = godotenv.Load("../../.env")

	apiKey := os.Getenv("OPENROUTER_API_KEY")
	if apiKey == "" {
		t.Skip("OPENROUTER_API_KEY not set — skipping agent eval")
	}

	baseURL := os.Getenv("OPENROUTER_BASE_URL_COMPLETIONS")
	if baseURL == "" {
		baseURL = "https://openrouter.ai/api/v1/chat/completions"
	}

	ctx := context.Background()

	// Connect to Postgres
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Fatal("TEST_DATABASE_URL not set")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Skipf("postgres not available: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		t.Skipf("postgres ping failed: %v", err)
	}
	t.Cleanup(func() { pool.Close() })

	s := store.New(pool)

	// Connect to Valkey
	valkeyAddr := os.Getenv("TEST_VALKEY_ADDR")
	if valkeyAddr == "" {
		t.Fatal("TEST_VALKEY_ADDR not set")
	}
	valkeyClient, err := valkey.NewClient(valkey.ClientOption{
		InitAddress: []string{valkeyAddr},
	})
	if err != nil {
		t.Skipf("valkey not available: %v", err)
	}
	resp := valkeyClient.Do(ctx, valkeyClient.B().Ping().Build())
	if resp.Error() != nil {
		t.Skipf("valkey ping failed: %v", resp.Error())
	}
	t.Cleanup(func() { valkeyClient.Close() })

	sm := session.NewManager(valkeyClient)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	h := NewHarness(HarnessConfig{
		APIKey:  apiKey,
		Model:   evalModel,
		BaseURL: baseURL,
		Store:   s,
		Session: sm,
		Logger:  logger,
	})

	return h, s, sm
}

// seedEvalGraph creates a test graph with 4 symbols and 3 edges, then computes analytics.
func seedEvalGraph(t *testing.T, s *store.Store) (projectSlug string, cleanup func()) {
	t.Helper()
	ctx := context.Background()
	slug := fmt.Sprintf("eval-agent-%s", t.Name())

	tenant, err := s.CreateTenant(ctx, postgres.CreateTenantParams{
		Name:     "eval-tenant",
		Slug:     slug + "-tenant",
		Settings: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	proj, err := s.CreateProject(ctx, postgres.CreateProjectParams{
		Name:     "Agent Eval Project",
		Slug:     slug,
		TenantID: tenant.ID,
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	source, err := s.CreateSource(ctx, postgres.CreateSourceParams{
		ProjectID:  proj.ID,
		Name:       "eval-source",
		SourceType: "upload",
		Config:     []byte("{}"),
	})
	if err != nil {
		t.Fatalf("create source: %v", err)
	}

	sqlFile, err := s.UpsertFile(ctx, postgres.UpsertFileParams{
		ProjectID: proj.ID, SourceID: source.ID,
		Path: "schema.sql", Language: "tsql", SizeBytes: 1000, Hash: "eval1",
	})
	if err != nil {
		t.Fatalf("create sql file: %v", err)
	}

	goFile, err := s.UpsertFile(ctx, postgres.UpsertFileParams{
		ProjectID: proj.ID, SourceID: source.ID,
		Path: "repository.go", Language: "go", SizeBytes: 2000, Hash: "eval2",
	})
	if err != nil {
		t.Fatalf("create go file: %v", err)
	}

	table, err := s.CreateSymbol(ctx, postgres.CreateSymbolParams{
		ProjectID: proj.ID, FileID: sqlFile.ID,
		Name: "Customers", QualifiedName: "dbo.Customers",
		Kind: "table", Language: "tsql", StartLine: 1, EndLine: 10,
	})
	if err != nil {
		t.Fatalf("create table: %v", err)
	}

	proc, err := s.CreateSymbol(ctx, postgres.CreateSymbolParams{
		ProjectID: proj.ID, FileID: sqlFile.ID,
		Name: "GetCustomer", QualifiedName: "dbo.GetCustomer",
		Kind: "procedure", Language: "tsql", StartLine: 20, EndLine: 40,
	})
	if err != nil {
		t.Fatalf("create proc: %v", err)
	}

	class, err := s.CreateSymbol(ctx, postgres.CreateSymbolParams{
		ProjectID: proj.ID, FileID: goFile.ID,
		Name: "CustomerRepository", QualifiedName: "app.repository.CustomerRepository",
		Kind: "class", Language: "go", StartLine: 1, EndLine: 80,
	})
	if err != nil {
		t.Fatalf("create class: %v", err)
	}

	method, err := s.CreateSymbol(ctx, postgres.CreateSymbolParams{
		ProjectID: proj.ID, FileID: goFile.ID,
		Name: "GetByID", QualifiedName: "app.repository.CustomerRepository.GetByID",
		Kind: "method", Language: "go", StartLine: 10, EndLine: 25,
	})
	if err != nil {
		t.Fatalf("create method: %v", err)
	}

	// C# file and symbol for cross-language tests
	csFile, err := s.UpsertFile(ctx, postgres.UpsertFileParams{
		ProjectID: proj.ID, SourceID: source.ID,
		Path: "repository.cs", Language: "csharp", SizeBytes: 1500, Hash: "eval3",
	})
	if err != nil {
		t.Fatalf("create cs file: %v", err)
	}

	csClass, err := s.CreateSymbol(ctx, postgres.CreateSymbolParams{
		ProjectID: proj.ID, FileID: csFile.ID,
		Name: "CustomerService", QualifiedName: "App.Services.CustomerService",
		Kind: "class", Language: "csharp", StartLine: 1, EndLine: 60,
	})
	if err != nil {
		t.Fatalf("create cs class: %v", err)
	}

	// Edges: proc→table (reads_from), class→proc (calls), method→table (reads_from)
	s.CreateSymbolEdge(ctx, postgres.CreateSymbolEdgeParams{
		ProjectID: proj.ID, SourceID: proc.ID, TargetID: table.ID, EdgeType: "reads_from",
	})
	s.CreateSymbolEdge(ctx, postgres.CreateSymbolEdgeParams{
		ProjectID: proj.ID, SourceID: class.ID, TargetID: proc.ID, EdgeType: "calls",
	})
	s.CreateSymbolEdge(ctx, postgres.CreateSymbolEdgeParams{
		ProjectID: proj.ID, SourceID: method.ID, TargetID: table.ID, EdgeType: "reads_from",
	})

	// Cross-language edges: csClass → proc (calls), csClass → table (uses_table) with metadata
	s.CreateSymbolEdgeWithMetadata(ctx, postgres.CreateSymbolEdgeWithMetadataParams{
		ProjectID: proj.ID, SourceID: csClass.ID, TargetID: proc.ID, EdgeType: "calls",
		Metadata: []byte(`{"confidence": 0.85, "bridge": "csharp→tsql"}`),
	})
	s.CreateSymbolEdgeWithMetadata(ctx, postgres.CreateSymbolEdgeWithMetadataParams{
		ProjectID: proj.ID, SourceID: csClass.ID, TargetID: table.ID, EdgeType: "uses_table",
		Metadata: []byte(`{"confidence": 0.95, "bridge": "csharp→tsql"}`),
	})

	// Compute analytics (PageRank, degrees, layers, summaries, bridges)
	engine := analytics.NewEngine(s, slog.Default())
	if err := engine.ComputeAll(ctx, proj.ID); err != nil {
		t.Fatalf("compute analytics: %v", err)
	}

	cleanup = func() {
		s.Pool().Exec(ctx, "DELETE FROM symbol_edges WHERE project_id = $1", proj.ID)
		s.Pool().Exec(ctx, "DELETE FROM symbols WHERE project_id = $1", proj.ID)
		s.Pool().Exec(ctx, "DELETE FROM files WHERE project_id = $1", proj.ID)
		s.Pool().Exec(ctx, "DELETE FROM sources WHERE project_id = $1", proj.ID)
		s.Pool().Exec(ctx, "DELETE FROM project_analytics WHERE project_id = $1", proj.ID)
		s.Pool().Exec(ctx, "DELETE FROM projects WHERE id = $1", proj.ID)
		s.Pool().Exec(ctx, "DELETE FROM tenants WHERE id = $1", tenant.ID)
	}

	return slug, cleanup
}

func TestAgentEval_TableDependencies(t *testing.T) {
	h, s, _ := setupHarness(t)
	slug, cleanup := seedEvalGraph(t, s)
	defer cleanup()

	ctx := context.Background()
	question := fmt.Sprintf("Using project '%s': What tables does CustomerRepository read from?", slug)

	result, err := h.Run(ctx, question)
	if err != nil {
		t.Fatalf("agent run failed: %v", err)
	}

	t.Logf("Question: %s", result.Question)
	t.Logf("Answer: %s", result.FinalAnswer)
	t.Logf("Tool calls: %d, Turns: %d, Tokens: %d", result.ToolCalls, result.Turns, result.TotalTokens)
	t.Logf("Tool sequence: %v", result.ToolSequence)

	// Correctness: answer should mention Customers table
	lower := strings.ToLower(result.FinalAnswer)
	if !strings.Contains(lower, "customer") {
		t.Errorf("expected answer to mention 'Customers' table, got: %s", result.FinalAnswer)
	}

	// Efficiency: should not need more than 4 tool calls
	if result.ToolCalls > 4 {
		t.Errorf("expected ≤4 tool calls, got %d", result.ToolCalls)
	}
}

func TestAgentEval_ProjectOverview(t *testing.T) {
	h, s, _ := setupHarness(t)
	slug, cleanup := seedEvalGraph(t, s)
	defer cleanup()

	ctx := context.Background()
	question := fmt.Sprintf("Using project '%s': Give me an overview of this project", slug)

	result, err := h.Run(ctx, question)
	if err != nil {
		t.Fatalf("agent run failed: %v", err)
	}

	t.Logf("Question: %s", result.Question)
	t.Logf("Answer: %s", result.FinalAnswer)
	t.Logf("Tool calls: %d, Turns: %d, Tokens: %d", result.ToolCalls, result.Turns, result.TotalTokens)
	t.Logf("Tool sequence: %v", result.ToolSequence)

	// Correctness: answer should contain some project info
	lower := strings.ToLower(result.FinalAnswer)
	hasInfo := strings.Contains(lower, "symbol") ||
		strings.Contains(lower, "4") ||
		strings.Contains(lower, "go") ||
		strings.Contains(lower, "tsql") ||
		strings.Contains(lower, "sql")
	if !hasInfo {
		t.Errorf("expected overview to mention symbols or languages, got: %s", result.FinalAnswer)
	}

	// Efficiency: overview should be 1-2 tool calls
	if result.ToolCalls > 2 {
		t.Errorf("expected ≤2 tool calls for overview, got %d", result.ToolCalls)
	}
}

func TestAgentEval_CrossLanguageBridges(t *testing.T) {
	h, s, _ := setupHarness(t)
	slug, cleanup := seedEvalGraph(t, s)
	defer cleanup()

	ctx := context.Background()
	question := fmt.Sprintf("Using project '%s': Give me an overview of the architecture — how do Go and SQL connect in this project?", slug)

	result, err := h.Run(ctx, question)
	if err != nil {
		t.Fatalf("agent run failed: %v", err)
	}

	t.Logf("Question: %s", result.Question)
	t.Logf("Answer: %s", result.FinalAnswer)
	t.Logf("Tool calls: %d, Turns: %d, Tokens: %d", result.ToolCalls, result.Turns, result.TotalTokens)
	t.Logf("Tool sequence: %v", result.ToolSequence)

	// Correctness: answer should mention bridge or cross-language or specific symbols
	lower := strings.ToLower(result.FinalAnswer)
	hasBridge := strings.Contains(lower, "bridge") ||
		strings.Contains(lower, "cross-language") ||
		strings.Contains(lower, "cross language") ||
		strings.Contains(lower, "customerrepository") ||
		strings.Contains(lower, "repository") ||
		strings.Contains(lower, "go") ||
		strings.Contains(lower, "tsql") ||
		strings.Contains(lower, "sql")
	if !hasBridge {
		t.Errorf("expected answer to mention cross-language relationship, got: %s", result.FinalAnswer)
	}

	// Efficiency: overview + possibly one follow-up
	if result.ToolCalls > 5 {
		t.Errorf("expected ≤5 tool calls, got %d", result.ToolCalls)
	}
}

func TestAgentEval_CrossLanguageTrace(t *testing.T) {
	h, s, _ := setupHarness(t)
	slug, cleanup := seedEvalGraph(t, s)
	defer cleanup()

	ctx := context.Background()
	question := fmt.Sprintf("Using project '%s': What's the full stack trace for GetCustomer? Show me how code flows from C# to SQL.", slug)

	result, err := h.Run(ctx, question)
	if err != nil {
		t.Fatalf("agent run failed: %v", err)
	}

	t.Logf("Question: %s", result.Question)
	t.Logf("Answer: %s", result.FinalAnswer)
	t.Logf("Tool calls: %d, Turns: %d, Tokens: %d", result.ToolCalls, result.Turns, result.TotalTokens)
	t.Logf("Tool sequence: %v", result.ToolSequence)

	lower := strings.ToLower(result.FinalAnswer)
	hasCSharp := strings.Contains(lower, "c#") || strings.Contains(lower, "csharp") || strings.Contains(lower, "customerservice")
	hasSQL := strings.Contains(lower, "tsql") || strings.Contains(lower, "sql") || strings.Contains(lower, "getcustomer") || strings.Contains(lower, "customers")
	if !hasCSharp || !hasSQL {
		t.Errorf("expected answer to mention both C# and T-SQL symbols, got: %s", result.FinalAnswer)
	}
	if result.ToolCalls > 5 {
		t.Errorf("expected ≤5 tool calls, got %d", result.ToolCalls)
	}
}

func TestAgentEval_ImpactAnalysis(t *testing.T) {
	h, s, _ := setupHarness(t)
	slug, cleanup := seedEvalGraph(t, s)
	defer cleanup()

	ctx := context.Background()
	question := fmt.Sprintf("Using project '%s': What would break if I rename the Customers table?", slug)

	result, err := h.Run(ctx, question)
	if err != nil {
		t.Fatalf("agent run failed: %v", err)
	}

	t.Logf("Question: %s", result.Question)
	t.Logf("Answer: %s", result.FinalAnswer)
	t.Logf("Tool calls: %d, Turns: %d, Tokens: %d", result.ToolCalls, result.Turns, result.TotalTokens)
	t.Logf("Tool sequence: %v", result.ToolSequence)

	lower := strings.ToLower(result.FinalAnswer)
	hasProc := strings.Contains(lower, "getcustomer")
	hasClass := strings.Contains(lower, "customerservice") || strings.Contains(lower, "customerrepository")
	if !hasProc && !hasClass {
		t.Errorf("expected answer to mention GetCustomer or CustomerService/CustomerRepository, got: %s", result.FinalAnswer)
	}
	if result.ToolCalls > 4 {
		t.Errorf("expected ≤4 tool calls, got %d", result.ToolCalls)
	}
}

func TestAgentEval_LineageTrace(t *testing.T) {
	h, s, _ := setupHarness(t)
	slug, cleanup := seedEvalGraph(t, s)
	defer cleanup()

	ctx := context.Background()
	question := fmt.Sprintf("Using project '%s': Where does the data in Customers table come from?", slug)

	result, err := h.Run(ctx, question)
	if err != nil {
		t.Fatalf("agent run failed: %v", err)
	}

	t.Logf("Question: %s", result.Question)
	t.Logf("Answer: %s", result.FinalAnswer)
	t.Logf("Tool calls: %d, Turns: %d, Tokens: %d", result.ToolCalls, result.Turns, result.TotalTokens)
	t.Logf("Tool sequence: %v", result.ToolSequence)

	lower := strings.ToLower(result.FinalAnswer)
	hasCaller := strings.Contains(lower, "getcustomer") ||
		strings.Contains(lower, "customerservice") ||
		strings.Contains(lower, "customerrepository") ||
		strings.Contains(lower, "getbyid") ||
		strings.Contains(lower, "upstream")
	if !hasCaller {
		t.Errorf("expected answer to mention upstream callers, got: %s", result.FinalAnswer)
	}
	if result.ToolCalls > 4 {
		t.Errorf("expected ≤4 tool calls, got %d", result.ToolCalls)
	}
}

// TestAgentEval_LiveProject runs the agent against a real indexed project.
// Set EVAL_PROJECT_SLUG and EVAL_QUESTION environment variables to use.
//
// Example:
//
//	EVAL_PROJECT_SLUG=myapp EVAL_QUESTION="What tables does the OrderService write to?" \
//	  go test -tags=integration ./test/agent/... -v -count=1 -run TestAgentEval_LiveProject
func TestAgentEval_LiveProject(t *testing.T) {
	// Load .env before reading env vars (setupHarness also loads it, but we need slug first)
	_ = godotenv.Load("../../.env")

	slug := os.Getenv("EVAL_PROJECT_SLUG")
	if slug == "" {
		t.Skip("EVAL_PROJECT_SLUG not set — skipping live project eval")
	}

	question := os.Getenv("EVAL_QUESTION")
	if question == "" {
		question = "Give me an overview of this project"
	}

	h, _, _ := setupHarness(t)

	ctx := context.Background()
	fullQuestion := fmt.Sprintf("Using project '%s': %s", slug, question)

	result, err := h.Run(ctx, fullQuestion)
	if err != nil {
		t.Fatalf("agent run failed: %v", err)
	}

	t.Logf("Question: %s", result.Question)
	t.Logf("Answer:\n%s", result.FinalAnswer)
	t.Logf("Tool calls: %d", result.ToolCalls)
	t.Logf("Turns: %d", result.Turns)
	t.Logf("Total tokens: %d", result.TotalTokens)
	t.Logf("Tool sequence: %v", result.ToolSequence)

	if result.FinalAnswer == "(max turns reached)" {
		t.Error("agent did not produce an answer within turn limit")
	}
}
