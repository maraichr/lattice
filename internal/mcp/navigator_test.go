package mcp

import (
	"testing"

	"github.com/maraichr/lattice/internal/store/postgres"
)

// --- classifyKind ---

func TestClassifyKind(t *testing.T) {
	tests := []struct {
		kind     string
		expected symbolKindCategory
	}{
		{"table", categoryData},
		{"view", categoryData},
		{"column", categoryData},
		{"function", categoryCode},
		{"method", categoryCode},
		{"procedure", categoryCode},
		{"trigger", categoryCode},
		{"class", categoryContainer},
		{"interface", categoryContainer},
		{"module", categoryContainer},
		{"package", categoryContainer},
		{"variable", categoryOther},
		{"enum", categoryOther},
		{"unknown", categoryOther},
	}

	for _, tt := range tests {
		got := classifyKind(tt.kind)
		if got != tt.expected {
			t.Errorf("classifyKind(%q) = %d, want %d", tt.kind, got, tt.expected)
		}
	}
}

// --- estimateDetailTokens ---

func TestEstimateDetailTokens_BaseOnly(t *testing.T) {
	sym := makeSymbol("Foo", "class", "app.Foo")
	est := estimateDetailTokens(sym)
	if est != 200 {
		t.Errorf("base estimate should be 200, got %d", est)
	}
}

func TestEstimateDetailTokens_WithDocAndSignature(t *testing.T) {
	doc := "This is a long documentation string for the symbol."
	sig := "func (s *Service) Process(ctx context.Context, input *Input) (*Output, error)"
	sym := makeSymbol("Process", "method", "app.Service.Process")
	sym.DocComment = &doc
	sym.Signature = &sig

	est := estimateDetailTokens(sym)
	expected := 200 + len(doc)/4 + len(sig)/4
	if est != expected {
		t.Errorf("estimate should be %d, got %d", expected, est)
	}
}

// --- SuggestNextSteps ---

func TestSuggestNextSteps_EmptySymbols(t *testing.T) {
	nav := NewNavigator(nil)
	hints := nav.SuggestNextSteps("search_symbols", nil, nil)
	if hints != nil {
		t.Error("empty symbols should return nil hints")
	}
}

func TestSuggestNextSteps_SearchSymbols_DataSymbol(t *testing.T) {
	nav := NewNavigator(nil)
	syms := []postgres.Symbol{makeSymbol("Customers", "table", "dbo.Customers")}
	hints := nav.SuggestNextSteps("search_symbols", syms, nil)

	if hints == nil || len(hints.Steps) == 0 {
		t.Fatal("should return hints after search")
	}

	// First hint should be search_symbols (refine)
	if hints.Steps[0].Tool != "search_symbols" {
		t.Errorf("first hint should be search_symbols, got %s", hints.Steps[0].Tool)
	}

	// For data symbols, should suggest get_lineage
	found := false
	for _, s := range hints.Steps {
		if s.Tool == "get_lineage" {
			found = true
			break
		}
	}
	if !found {
		t.Error("data symbol search should suggest get_lineage")
	}
}

func TestSuggestNextSteps_SearchSymbols_CodeSymbol(t *testing.T) {
	nav := NewNavigator(nil)
	syms := []postgres.Symbol{makeSymbol("ProcessOrder", "function", "app.ProcessOrder")}
	hints := nav.SuggestNextSteps("search_symbols", syms, nil)

	if hints == nil {
		t.Fatal("should return hints")
	}

	found := false
	for _, s := range hints.Steps {
		if s.Tool == "extract_subgraph" {
			found = true
			break
		}
	}
	if !found {
		t.Error("code symbol search should suggest extract_subgraph")
	}
}

func TestSuggestNextSteps_SearchSymbols_ManyResults(t *testing.T) {
	nav := NewNavigator(nil)
	syms := make([]postgres.Symbol, 5)
	for i := range 5 {
		syms[i] = makeSymbol("Sym", "class", "app.Sym")
	}
	hints := nav.SuggestNextSteps("search_symbols", syms, nil)

	found := false
	for _, s := range hints.Steps {
		if s.Tool == "extract_subgraph" {
			found = true
			break
		}
	}
	if !found {
		t.Error(">3 results should suggest extract_subgraph")
	}
}

func TestSuggestNextSteps_Details(t *testing.T) {
	nav := NewNavigator(nil)
	syms := []postgres.Symbol{makeSymbol("CustomerRepo", "class", "app.CustomerRepo")}
	hints := nav.SuggestNextSteps("extract_subgraph", syms, nil)

	if hints == nil || len(hints.Steps) < 2 {
		t.Fatal("details should suggest at least 2 steps")
	}

	tools := make(map[string]bool)
	for _, s := range hints.Steps {
		tools[s.Tool] = true
	}
	if !tools["extract_subgraph"] {
		t.Error("should suggest extract_subgraph")
	}
	if !tools["analyze_impact"] {
		t.Error("should suggest analyze_impact")
	}
}

func TestSuggestNextSteps_Details_DataSymbol(t *testing.T) {
	nav := NewNavigator(nil)
	syms := []postgres.Symbol{makeSymbol("Customers", "table", "dbo.Customers")}
	hints := nav.SuggestNextSteps("extract_subgraph", syms, nil)

	found := false
	for _, s := range hints.Steps {
		if s.Tool == "get_lineage" {
			found = true
			break
		}
	}
	if !found {
		t.Error("data symbol details should suggest get_lineage")
	}
}

func TestSuggestNextSteps_Details_CodeSymbol(t *testing.T) {
	nav := NewNavigator(nil)
	syms := []postgres.Symbol{makeSymbol("Process", "method", "app.Service.Process")}
	hints := nav.SuggestNextSteps("extract_subgraph", syms, nil)

	found := false
	for _, s := range hints.Steps {
		if s.Tool == "analyze_impact" {
			found = true
			break
		}
	}
	if !found {
		t.Error("code symbol details should suggest analyze_impact")
	}
}

func TestSuggestNextSteps_Overview(t *testing.T) {
	nav := NewNavigator(nil)
	syms := []postgres.Symbol{makeSymbol("Any", "class", "app.Any")}
	hints := nav.SuggestNextSteps("list_project_overview", syms, nil)

	if hints == nil || len(hints.Steps) != 2 {
		t.Fatal("overview should suggest exactly 2 steps")
	}
	if hints.Steps[0].Tool != "search_symbols" {
		t.Errorf("first overview hint should be search_symbols, got %s", hints.Steps[0].Tool)
	}
	if hints.Steps[1].Tool != "extract_subgraph" {
		t.Errorf("second overview hint should be extract_subgraph, got %s", hints.Steps[1].Tool)
	}
}

func TestSuggestNextSteps_MaxThreeHints(t *testing.T) {
	nav := NewNavigator(nil)
	// Dependencies with many container/data symbols could generate >3 hints
	syms := make([]postgres.Symbol, 10)
	for i := range 10 {
		syms[i] = makeSymbol("Sym", "class", "app.Sym")
	}
	hints := nav.SuggestNextSteps("extract_subgraph", syms, nil)
	if hints != nil && len(hints.Steps) > 3 {
		t.Errorf("should cap at 3 hints, got %d", len(hints.Steps))
	}
}

func TestSuggestNextSteps_UnknownTool(t *testing.T) {
	nav := NewNavigator(nil)
	syms := []postgres.Symbol{makeSymbol("Foo", "class", "app.Foo")}
	hints := nav.SuggestNextSteps("some_unknown_tool", syms, nil)

	if hints == nil || len(hints.Steps) == 0 {
		t.Fatal("unknown tool should return default hints")
	}
	if hints.Steps[0].Tool != "search_symbols" {
		t.Errorf("default hint should be search_symbols, got %s", hints.Steps[0].Tool)
	}
}

func TestSuggestNextSteps_HintsContainParams(t *testing.T) {
	nav := NewNavigator(nil)
	sym := makeSymbol("Customers", "table", "dbo.Customers")
	hints := nav.SuggestNextSteps("search_symbols", []postgres.Symbol{sym}, nil)

	for _, step := range hints.Steps {
		if step.Params != nil {
			if val, ok := step.Params["query"]; ok {
				if val != sym.Name {
					t.Errorf("param query should match %s, got %s", sym.Name, val)
				}
				return
			}
			if val, ok := step.Params["symbol_name"]; ok {
				if val != sym.Name {
					t.Errorf("param symbol_name should match %s, got %s", sym.Name, val)
				}
				return
			}
		}
	}
	t.Error("at least one hint should include query or symbol_name param")
}

func TestSuggestNextSteps_HintsHaveTokenEstimates(t *testing.T) {
	nav := NewNavigator(nil)
	syms := []postgres.Symbol{makeSymbol("Foo", "class", "app.Foo")}
	hints := nav.SuggestNextSteps("search_symbols", syms, nil)

	for _, step := range hints.Steps {
		if step.EstimatedTokens <= 0 {
			t.Errorf("hint for %s should have positive token estimate", step.Tool)
		}
	}
}
