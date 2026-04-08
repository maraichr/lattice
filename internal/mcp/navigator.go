package mcp

import (
	"fmt"

	"github.com/maraichr/lattice/internal/mcp/session"
	"github.com/maraichr/lattice/internal/store/postgres"
)

// NavigationHints suggests next tool calls based on current results.
type NavigationHints struct {
	Steps []NavigationStep `json:"steps"`
}

// NavigationStep is a suggested next MCP tool call.
type NavigationStep struct {
	Tool            string            `json:"tool"`
	Description     string            `json:"description"`
	Params          map[string]string `json:"params,omitempty"`
	EstimatedTokens int               `json:"estimated_tokens,omitempty"`
}

// Navigator generates context-aware navigation hints for MCP tool responses.
type Navigator struct {
	store *postgres.Queries
}

// NewNavigator creates a navigator with access to the store for edge counting.
func NewNavigator(store *postgres.Queries) *Navigator {
	return &Navigator{store: store}
}

// symbolKindCategory classifies symbol kinds for navigation routing.
type symbolKindCategory int

const (
	categoryData symbolKindCategory = iota
	categoryCode
	categoryContainer
	categoryOther
)

func classifyKind(kind string) symbolKindCategory {
	switch kind {
	case "table", "view", "column":
		return categoryData
	case "function", "method", "procedure", "trigger":
		return categoryCode
	case "class", "interface", "module", "package":
		return categoryContainer
	default:
		return categoryOther
	}
}

// SuggestNextSteps returns navigation hints based on the tool that was just called
// and the symbols it returned.
func (n *Navigator) SuggestNextSteps(toolName string, symbols []postgres.Symbol, sess *session.Session) *NavigationHints {
	if len(symbols) == 0 {
		return nil
	}

	hints := &NavigationHints{}

	switch toolName {
	case "search_symbols":
		hints.Steps = n.hintsAfterSearch(symbols)
	case "extract_subgraph":
		hints.Steps = n.hintsAfterSubgraph(symbols)
	case "get_lineage":
		hints.Steps = n.hintsAfterLineage(symbols)
	case "list_project_overview":
		hints.Steps = n.hintsAfterOverview(symbols)
	case "analyze_impact":
		hints.Steps = n.hintsAfterImpact(symbols)
	case "trace_cross_language":
		hints.Steps = n.hintsAfterCrossLanguageTrace(symbols)
	case "semantic_search":
		hints.Steps = n.hintsAfterSemanticSearch(symbols)
	case "get_project_analytics":
		hints.Steps = n.hintsAfterAnalytics(symbols)
	default:
		hints.Steps = n.defaultHints(symbols)
	}

	// Limit to top 3 hints
	if len(hints.Steps) > 3 {
		hints.Steps = hints.Steps[:3]
	}

	return hints
}

func (n *Navigator) hintsAfterSearch(symbols []postgres.Symbol) []NavigationStep {
	steps := make([]NavigationStep, 0, 3)

	if len(symbols) > 0 {
		top := symbols[0]
		steps = append(steps, NavigationStep{
			Tool:            "search_symbols",
			Description:     fmt.Sprintf("Refine search for %s with kind/language filters", top.Name),
			Params:          map[string]string{"query": top.Name},
			EstimatedTokens: 400,
		})

		cat := classifyKind(top.Kind)
		if cat == categoryData {
			steps = append(steps, NavigationStep{
				Tool:            "get_lineage",
				Description:     fmt.Sprintf("Trace data flow for %s", top.Name),
				Params:          map[string]string{"symbol_name": top.Name, "direction": "both"},
				EstimatedTokens: 800,
			})
		} else if cat == categoryCode || cat == categoryContainer {
			steps = append(steps, NavigationStep{
				Tool:            "extract_subgraph",
				Description:     fmt.Sprintf("Show what %s depends on / is depended by", top.Name),
				Params:          map[string]string{"topic": top.Name},
				EstimatedTokens: 600,
			})
		}
	}

	if len(symbols) > 3 {
		steps = append(steps, NavigationStep{
			Tool:            "extract_subgraph",
			Description:     "Extract topic subgraph around these results",
			EstimatedTokens: 1200,
		})
	}

	return steps
}

func (n *Navigator) hintsAfterSubgraph(symbols []postgres.Symbol) []NavigationStep {
	if len(symbols) == 0 {
		return nil
	}

	sym := symbols[0]
	steps := []NavigationStep{
		{
			Tool:            "extract_subgraph",
			Description:     fmt.Sprintf("Explore deeper around %s", sym.Name),
			Params:          map[string]string{"topic": sym.Name},
			EstimatedTokens: 600,
		},
		{
			Tool:            "analyze_impact",
			Description:     fmt.Sprintf("Analyze impact of changing %s", sym.Name),
			Params:          map[string]string{"symbol_name": sym.Name},
			EstimatedTokens: 400,
		},
	}

	if classifyKind(sym.Kind) == categoryData {
		steps = append(steps, NavigationStep{
			Tool:            "get_lineage",
			Description:     fmt.Sprintf("Trace lineage through %s", sym.Name),
			Params:          map[string]string{"symbol_name": sym.Name},
			EstimatedTokens: 800,
		})
	} else {
		steps = append(steps, NavigationStep{
			Tool:            "analyze_impact",
			Description:     fmt.Sprintf("Analyze blast radius of %s", sym.Name),
			Params:          map[string]string{"symbol_name": sym.Name},
			EstimatedTokens: 1000,
		})
	}

	return steps
}

func (n *Navigator) hintsAfterLineage(symbols []postgres.Symbol) []NavigationStep {
	steps := make([]NavigationStep, 0, 3)

	for _, sym := range symbols {
		if classifyKind(sym.Kind) == categoryCode {
			steps = append(steps, NavigationStep{
				Tool:            "search_symbols",
				Description:     fmt.Sprintf("Examine transformer %s", sym.Name),
				Params:          map[string]string{"query": sym.Name},
				EstimatedTokens: 400,
			})
			break
		}
	}

	steps = append(steps, NavigationStep{
		Tool:            "analyze_impact",
		Description:     "Assess blast radius of changes to this data flow",
		EstimatedTokens: 1000,
	})

	return steps
}

func (n *Navigator) hintsAfterOverview(_ []postgres.Symbol) []NavigationStep {
	return []NavigationStep{
		{
			Tool:            "search_symbols",
			Description:     "Search for specific symbols by name or kind",
			EstimatedTokens: 400,
		},
		{
			Tool:            "extract_subgraph",
			Description:     "Extract a topic subgraph (e.g., 'order processing')",
			EstimatedTokens: 1200,
		},
	}
}

func (n *Navigator) hintsAfterImpact(symbols []postgres.Symbol) []NavigationStep {
	steps := make([]NavigationStep, 0, 2)

	for _, sym := range symbols {
		if classifyKind(sym.Kind) == categoryData || classifyKind(sym.Kind) == categoryContainer {
			steps = append(steps, NavigationStep{
				Tool:            "search_symbols",
				Description:     fmt.Sprintf("Examine impacted %s (%s)", sym.Name, sym.Kind),
				Params:          map[string]string{"query": sym.Name},
				EstimatedTokens: 400,
			})
			if len(steps) >= 2 {
				break
			}
		}
	}

	return steps
}

func (n *Navigator) hintsAfterCrossLanguageTrace(symbols []postgres.Symbol) []NavigationStep {
	steps := make([]NavigationStep, 0, 3)

	for _, sym := range symbols {
		steps = append(steps, NavigationStep{
			Tool:            "analyze_impact",
			Description:     fmt.Sprintf("Analyze blast radius of bridge symbol %s", sym.Name),
			Params:          map[string]string{"symbol_name": sym.Name},
			EstimatedTokens: 1000,
		})
		if len(steps) >= 1 {
			break
		}
	}

	steps = append(steps, NavigationStep{
		Tool:            "get_project_analytics",
		Description:     "Check cross-language bridge coverage",
		Params:          map[string]string{"scope": "bridges"},
		EstimatedTokens: 400,
	})

	return steps
}

func (n *Navigator) hintsAfterSemanticSearch(symbols []postgres.Symbol) []NavigationStep {
	steps := make([]NavigationStep, 0, 3)

	for _, sym := range symbols {
		if classifyKind(sym.Kind) == categoryData {
			steps = append(steps, NavigationStep{
				Tool:            "get_lineage",
				Description:     fmt.Sprintf("Trace lineage for %s", sym.Name),
				Params:          map[string]string{"symbol_name": sym.Name},
				EstimatedTokens: 800,
			})
			break
		}
	}

	if len(symbols) > 0 {
		steps = append(steps, NavigationStep{
			Tool:            "extract_subgraph",
			Description:     "Explore topic subgraph around these results",
			EstimatedTokens: 1200,
		})
	}

	return steps
}

func (n *Navigator) hintsAfterAnalytics(_ []postgres.Symbol) []NavigationStep {
	return []NavigationStep{
		{
			Tool:            "search_symbols",
			Description:     "Drill into specific symbol kinds or languages",
			EstimatedTokens: 400,
		},
		{
			Tool:            "extract_subgraph",
			Description:     "Explore a specific topic in the codebase",
			EstimatedTokens: 1200,
		},
	}
}

func (n *Navigator) defaultHints(symbols []postgres.Symbol) []NavigationStep {
	if len(symbols) == 0 {
		return nil
	}
	sym := symbols[0]
	return []NavigationStep{
		{
			Tool:            "search_symbols",
			Description:     fmt.Sprintf("Examine %s", sym.Name),
			Params:          map[string]string{"query": sym.Name},
			EstimatedTokens: 400,
		},
	}
}

func estimateDetailTokens(sym postgres.Symbol) int {
	base := 200
	if sym.DocComment != nil {
		base += len(*sym.DocComment) / 4
	}
	if sym.Signature != nil {
		base += len(*sym.Signature) / 4
	}
	return base
}
