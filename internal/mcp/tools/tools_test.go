package tools

import (
	"testing"

	"github.com/maraichr/lattice/internal/mcp"
	"github.com/maraichr/lattice/internal/store/postgres"
)

// --- classifyIntent ---

func TestClassifyIntent_Impact(t *testing.T) {
	tests := []string{
		"What breaks if I rename Customers?",
		"What happens if I delete this table?",
		"What is the impact of changing this function?",
		"Show me the blast radius of modifying OrderService",
		"What would be affected by removing this column?",
	}
	for _, q := range tests {
		if classifyIntent(q) != IntentImpact {
			t.Errorf("expected IntentImpact for %q, got %s", q, classifyIntent(q))
		}
	}
}

func TestClassifyIntent_Lineage(t *testing.T) {
	tests := []string{
		"Where does the data flow from Customers.Email?",
		"Show me the lineage of this column",
		"Where does this data come from?",
		"What transforms this field?",
		"What populates the OrderTotal column?",
	}
	for _, q := range tests {
		if classifyIntent(q) != IntentLineage {
			t.Errorf("expected IntentLineage for %q, got %s", q, classifyIntent(q))
		}
	}
}

func TestClassifyIntent_Overview(t *testing.T) {
	tests := []string{
		"Give me an overview of this project",
		"What is this codebase?",
		"Describe the architecture",
		"Show me a summary",
		"What languages are used?",
		"How big is this project?",
	}
	for _, q := range tests {
		if classifyIntent(q) != IntentOverview {
			t.Errorf("expected IntentOverview for %q, got %s", q, classifyIntent(q))
		}
	}
}

func TestClassifyIntent_Dependencies(t *testing.T) {
	tests := []string{
		"What depends on this table?",
		"Show me the dependencies of OrderService",
		"What calls this function?",
		"What uses this module?",
		"What imports this package?",
	}
	for _, q := range tests {
		if classifyIntent(q) != IntentDeps {
			t.Errorf("expected IntentDeps for %q, got %s", q, classifyIntent(q))
		}
	}
}

func TestClassifyIntent_Subgraph(t *testing.T) {
	tests := []string{
		"Show me everything about order processing",
		"What is the order processing pipeline?",
		"Show me the authentication workflow",
		"Tell me about the payment module",
	}
	for _, q := range tests {
		if classifyIntent(q) != IntentSubgraph {
			t.Errorf("expected IntentSubgraph for %q, got %s", q, classifyIntent(q))
		}
	}
}

func TestClassifyIntent_Search_Default(t *testing.T) {
	tests := []string{
		"CustomerRepository",
		"dbo.Customers",
		"Find the login handler",
	}
	for _, q := range tests {
		if classifyIntent(q) != IntentSearch {
			t.Errorf("expected IntentSearch for %q, got %s", q, classifyIntent(q))
		}
	}
}

// --- extractSearchTerms ---

func TestExtractSearchTerms_RemovesStopWords(t *testing.T) {
	result := extractSearchTerms("What is the CustomerRepository?")
	if result != "customerrepository" {
		t.Errorf("expected 'customerrepository', got %q", result)
	}
}

func TestExtractSearchTerms_PreservesSubstantiveWords(t *testing.T) {
	result := extractSearchTerms("Show me the order processing pipeline")
	// "show" and "me" and "the" are stop words
	if result == "" {
		t.Error("should preserve substantive words")
	}
	if contains(result, "show") || contains(result, "the") {
		t.Errorf("should remove stop words, got %q", result)
	}
}

func TestExtractSearchTerms_EmptyAfterStopWords(t *testing.T) {
	result := extractSearchTerms("what is it?")
	// If all words are stop words, return original
	if result != "what is it?" {
		t.Errorf("all-stop-word input should return original, got %q", result)
	}
}

func TestExtractSearchTerms_StripsPunctuation(t *testing.T) {
	result := extractSearchTerms("What about 'Customers'?")
	if contains(result, "'") || contains(result, "?") {
		t.Errorf("should strip punctuation, got %q", result)
	}
}

func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// --- classifyIntent: cross-language ---

func TestClassifyIntent_CrossLanguage(t *testing.T) {
	tests := []string{
		"what tables does this endpoint touch?",
		"full stack trace of getUserById",
		"who calls this stored procedure?",
		"end to end from frontend to database",
	}
	for _, q := range tests {
		if classifyIntent(q) != IntentCrossLanguage {
			t.Errorf("expected IntentCrossLanguage for %q, got %s", q, classifyIntent(q))
		}
	}
}

func TestClassifyIntent_Ranking(t *testing.T) {
	tests := []string{
		"most used tables",
		"top 10 functions",
		"busiest endpoints",
	}
	for _, q := range tests {
		if classifyIntent(q) != IntentRanking {
			t.Errorf("expected IntentRanking for %q, got %s", q, classifyIntent(q))
		}
	}
}

func TestClassifyIntent_Bridges(t *testing.T) {
	tests := []string{
		"show me cross-language bridges",
		"what bridges exist between languages?",
	}
	for _, q := range tests {
		if classifyIntent(q) != IntentBridges {
			t.Errorf("expected IntentBridges for %q, got %s", q, classifyIntent(q))
		}
	}
}

// --- isLowValue ---

func TestIsLowValue_LowPageRankColumn(t *testing.T) {
	sym := postgres.Symbol{Kind: "column"}
	if !isLowValue(sym) {
		t.Error("column with no pagerank should be low value")
	}
}

func TestIsLowValue_Table(t *testing.T) {
	sym := postgres.Symbol{Kind: "table"}
	if isLowValue(sym) {
		t.Error("table should never be low value")
	}
}

// --- symbolTokenEstimate ---

func TestSymbolTokenEstimate(t *testing.T) {
	if symbolTokenEstimate(mcp.VerbositySummary) != 30 {
		t.Error("summary should estimate 30 tokens")
	}
	if symbolTokenEstimate(mcp.VerbosityStandard) != 60 {
		t.Error("standard should estimate 60 tokens")
	}
	if symbolTokenEstimate(mcp.VerbosityFull) != 120 {
		t.Error("full should estimate 120 tokens")
	}
}

// --- estimateSubgraphTokens ---

func TestEstimateSubgraphTokens(t *testing.T) {
	symbols := make([]postgres.Symbol, 10)
	edges := make([]subgraphEdge, 5)
	tokens := estimateSubgraphTokens(symbols, edges, mcp.VerbosityStandard)
	expected := 10*60 + 5*15 + 100
	if tokens != expected {
		t.Errorf("expected %d tokens, got %d", expected, tokens)
	}
}

// --- identifyCore ---

func TestIdentifyCore(t *testing.T) {
	seeds := []postgres.Symbol{
		{ID: [16]byte{1}},
		{ID: [16]byte{2}},
	}
	subgraph := append(seeds, postgres.Symbol{ID: [16]byte{3}})
	core := identifyCore(seeds, subgraph)

	if !core[[16]byte{1}] || !core[[16]byte{2}] {
		t.Error("seed symbols should be core")
	}
	if core[[16]byte{3}] {
		t.Error("non-seed symbols should not be core")
	}
}

// --- agentToolCatalog ---

func TestAgentToolCatalog_HasExpectedTools(t *testing.T) {
	catalog := agentToolCatalog()

	expected := map[string]bool{
		"search_symbols":        false,
		"get_lineage":           false,
		"analyze_impact":        false,
		"extract_subgraph":      false,
		"trace_cross_language":  false,
		"get_project_analytics": false,
		"semantic_search":       false,
	}

	for _, tool := range catalog {
		if tool.Type != "function" {
			t.Errorf("tool %s has type %q, want 'function'", tool.Function.Name, tool.Type)
		}
		if _, ok := expected[tool.Function.Name]; ok {
			expected[tool.Function.Name] = true
		} else {
			t.Errorf("unexpected tool in catalog: %s", tool.Function.Name)
		}
	}

	for name, found := range expected {
		if !found {
			t.Errorf("expected tool %s not in catalog", name)
		}
	}
}

func TestAgentToolCatalog_ExcludesAskCodebase(t *testing.T) {
	catalog := agentToolCatalog()
	for _, tool := range catalog {
		if tool.Function.Name == "ask_codebase" {
			t.Error("ask_codebase should not be in agent tool catalog (would cause recursion)")
		}
		if tool.Function.Name == "list_projects" {
			t.Error("list_projects should not be in agent tool catalog")
		}
	}
}

// --- dispatchToolCall ---

func TestDispatchToolCall_UnknownTool(t *testing.T) {
	h := &AskCodebaseHandler{}
	result := h.dispatchToolCall(nil, "nonexistent_tool", `{}`, "test-project")
	if !contains(result, "Unknown tool") {
		t.Errorf("expected 'Unknown tool' error, got %q", result)
	}
}

func TestDispatchToolCall_InvalidJSON(t *testing.T) {
	h := &AskCodebaseHandler{}
	result := h.dispatchToolCall(nil, "search_symbols", `{invalid`, "test-project")
	if !contains(result, "Error parsing") {
		t.Errorf("expected parsing error, got %q", result)
	}
}
