//go:build integration

package agent

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/maraichr/lattice/internal/mcp/session"
	"github.com/maraichr/lattice/internal/mcp/tools"
	"github.com/maraichr/lattice/internal/store"
)

// buildToolsAndDispatch returns the OpenAI tool schemas and a dispatch map for the eval harness.
func buildToolsAndDispatch(s *store.Store, sm *session.Manager, logger *slog.Logger) ([]openaiTool, map[string]ToolFunc) {
	subgraphHandler := tools.NewExtractSubgraphHandler(s, sm, nil, logger)
	askHandler := tools.NewAskCodebaseHandler(s, sm, nil, logger)
	searchHandler := tools.NewSearchSymbolsHandler(s, sm, logger)
	lineageHandler := tools.NewGetLineageHandler(s, logger)
	impactHandler := tools.NewAnalyzeImpactHandler(s, logger)
	analyticsHandler := tools.NewGetProjectAnalyticsHandler(s, logger)
	semanticHandler := tools.NewSemanticSearchHandler(s, nil, logger)
	traceHandler := tools.NewTraceCrossLanguageHandler(s, logger)
	listHandler := tools.NewListProjectsHandler(s, logger)

	schemas := []openaiTool{
		{
			Type: "function",
			Function: toolFunction{
				Name:        "extract_subgraph",
				Description: "Extract a subgraph of symbols and relationships around a topic or set of seed symbols. Returns symbol cards with metadata, edges, and navigation hints.",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {
						"project": {
							"type": "string",
							"description": "Project slug identifier"
						},
						"topic": {
							"type": "string",
							"description": "Symbol name or partial name to search for (e.g. 'Users', 'GetCustomer', 'Repository'). NOT natural language — use actual symbol names."
						},
						"kinds": {
							"type": "array",
							"items": {"type": "string"},
							"description": "Filter seed symbols by kind: table, procedure, class, method, function, column, interface, field, property, enum"
						},
						"seed_symbols": {
							"type": "array",
							"items": {"type": "string"},
							"description": "Explicit symbol UUIDs to use as BFS seeds"
						},
						"max_depth": {
							"type": "integer",
							"description": "Maximum BFS depth (default 2)"
						},
						"max_nodes": {
							"type": "integer",
							"description": "Maximum symbols to return (default 50)"
						},
						"verbosity": {
							"type": "string",
							"enum": ["summary", "normal", "full"],
							"description": "Level of detail in symbol cards"
						},
						"session_id": {
							"type": "string",
							"description": "Session ID for deduplication and navigation context"
						}
					},
					"required": ["project"]
				}`),
			},
		},
		{
			Type: "function",
			Function: toolFunction{
				Name:        "ask_codebase",
				Description: "Ask a natural language question about the codebase. Routes to the appropriate analysis: overview, search, ranking (most used/important), impact analysis, lineage tracing, or subgraph exploration. Supports filtering by symbol kind and language.",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {
						"project": {
							"type": "string",
							"description": "Project slug identifier"
						},
						"question": {
							"type": "string",
							"description": "Natural language question about the codebase"
						},
						"kinds": {
							"type": "array",
							"items": {"type": "string"},
							"description": "Filter by symbol kinds: table, procedure, class, method, function, column, interface, field, property, enum"
						},
						"languages": {
							"type": "array",
							"items": {"type": "string"},
							"description": "Filter by languages: csharp, tsql, javascript, typescript, go, java, etc."
						},
						"verbosity": {
							"type": "string",
							"enum": ["summary", "normal", "full"],
							"description": "Level of detail in response"
						},
						"session_id": {
							"type": "string",
							"description": "Session ID for context continuity"
						}
					},
					"required": ["project", "question"]
				}`),
			},
		},
		{
			Type: "function",
			Function: toolFunction{
				Name:        "search_symbols",
				Description: "Search for symbols (tables, procedures, classes, functions, etc.) by name or keyword within a project. Supports filtering by kind and language.",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {
						"project": {
							"type": "string",
							"description": "Project slug identifier"
						},
						"query": {
							"type": "string",
							"description": "Symbol name or keyword to search for"
						},
						"kinds": {
							"type": "array",
							"items": {"type": "string"},
							"description": "Filter by symbol kinds"
						},
						"languages": {
							"type": "array",
							"items": {"type": "string"},
							"description": "Filter by languages"
						},
						"limit": {
							"type": "integer",
							"description": "Maximum results to return (default 20)"
						},
						"verbosity": {
							"type": "string",
							"enum": ["summary", "normal", "full"],
							"description": "Level of detail in symbol cards"
						},
						"session_id": {
							"type": "string",
							"description": "Session ID for deduplication"
						}
					},
					"required": ["project", "query"]
				}`),
			},
		},
		{
			Type: "function",
			Function: toolFunction{
				Name:        "get_lineage",
				Description: "Trace the upstream (data sources, callers) or downstream (consumers, dependents) lineage of a symbol. Useful for understanding data flow and call chains.",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {
						"project": {
							"type": "string",
							"description": "Project slug identifier"
						},
						"symbol_id": {
							"type": "string",
							"description": "UUID of the symbol to trace from"
						},
						"symbol_name": {
							"type": "string",
							"description": "Name of the symbol to trace from (alternative to symbol_id)"
						},
						"direction": {
							"type": "string",
							"enum": ["upstream", "downstream", "both"],
							"description": "Direction to trace (default: both)"
						},
						"max_depth": {
							"type": "integer",
							"description": "Maximum trace depth (default: 3)"
						}
					},
					"required": ["project"]
				}`),
			},
		},
		{
			Type: "function",
			Function: toolFunction{
				Name:        "analyze_impact",
				Description: "Analyze the blast radius of modifying, deleting, or renaming a symbol. Shows direct and transitive impacts with severity classification.",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {
						"project": {
							"type": "string",
							"description": "Project slug identifier"
						},
						"symbol_id": {
							"type": "string",
							"description": "UUID of the symbol to analyze"
						},
						"symbol_name": {
							"type": "string",
							"description": "Name of the symbol to analyze (alternative to symbol_id)"
						},
						"change_type": {
							"type": "string",
							"enum": ["modify", "delete", "rename"],
							"description": "Type of change to analyze (default: modify)"
						},
						"max_depth": {
							"type": "integer",
							"description": "Maximum impact depth (default: 3)"
						}
					},
					"required": ["project"]
				}`),
			},
		},
		{
			Type: "function",
			Function: toolFunction{
				Name:        "get_project_analytics",
				Description: "Get project-level analytics: summary stats, language distribution, symbol kind counts, architectural layer distribution, or cross-language bridges.",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {
						"project": {
							"type": "string",
							"description": "Project slug identifier"
						},
						"scope": {
							"type": "string",
							"enum": ["summary", "languages", "kinds", "layers", "bridges"],
							"description": "Analytics scope (default: summary)"
						}
					},
					"required": ["project"]
				}`),
			},
		},
		{
			Type: "function",
			Function: toolFunction{
				Name:        "semantic_search",
				Description: "Search symbols using natural language via vector embeddings. Finds conceptually similar symbols even without exact name matches.",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {
						"project": {
							"type": "string",
							"description": "Project slug identifier"
						},
						"query": {
							"type": "string",
							"description": "Natural language query describing what you're looking for"
						},
						"kinds": {
							"type": "array",
							"items": {"type": "string"},
							"description": "Filter by symbol kinds"
						},
						"top_k": {
							"type": "integer",
							"description": "Number of results to return (default: 10)"
						}
					},
					"required": ["project", "query"]
				}`),
			},
		},
		{
			Type: "function",
			Function: toolFunction{
				Name:        "trace_cross_language",
				Description: "Trace cross-language execution paths from a symbol. Shows how code flows across language boundaries (e.g., C# → T-SQL) with confidence scores.",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {
						"project": {
							"type": "string",
							"description": "Project slug identifier"
						},
						"symbol_id": {
							"type": "string",
							"description": "UUID of the symbol to trace from"
						},
						"symbol_name": {
							"type": "string",
							"description": "Name of the symbol to trace from (alternative to symbol_id)"
						},
						"direction": {
							"type": "string",
							"enum": ["upstream", "downstream", "full"],
							"description": "Direction to trace (default: full)"
						},
						"max_depth": {
							"type": "integer",
							"description": "Maximum trace depth (default: 5)"
						},
						"session_id": {
							"type": "string",
							"description": "Session ID for context continuity"
						}
					},
					"required": ["project"]
				}`),
			},
		},
		{
			Type: "function",
			Function: toolFunction{
				Name:        "list_projects",
				Description: "List all projects accessible to the authenticated user. Returns project slug, name, and description.",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {
						"limit": {
							"type": "integer",
							"description": "Maximum number of projects to return"
						}
					}
				}`),
			},
		},
	}

	dispatch := map[string]ToolFunc{
		"extract_subgraph": func(ctx context.Context, argsJSON json.RawMessage) (string, error) {
			var params tools.ExtractSubgraphParams
			if err := json.Unmarshal(argsJSON, &params); err != nil {
				return "", err
			}
			return subgraphHandler.Handle(ctx, params)
		},
		"ask_codebase": func(ctx context.Context, argsJSON json.RawMessage) (string, error) {
			var params tools.AskCodebaseParams
			if err := json.Unmarshal(argsJSON, &params); err != nil {
				return "", err
			}
			return askHandler.Handle(ctx, params)
		},
		"search_symbols": func(ctx context.Context, argsJSON json.RawMessage) (string, error) {
			var params tools.SearchSymbolsParams
			if err := json.Unmarshal(argsJSON, &params); err != nil {
				return "", err
			}
			return searchHandler.Handle(ctx, params)
		},
		"get_lineage": func(ctx context.Context, argsJSON json.RawMessage) (string, error) {
			var params tools.GetLineageParams
			if err := json.Unmarshal(argsJSON, &params); err != nil {
				return "", err
			}
			return lineageHandler.Handle(ctx, params)
		},
		"analyze_impact": func(ctx context.Context, argsJSON json.RawMessage) (string, error) {
			var params tools.AnalyzeImpactParams
			if err := json.Unmarshal(argsJSON, &params); err != nil {
				return "", err
			}
			return impactHandler.Handle(ctx, params)
		},
		"get_project_analytics": func(ctx context.Context, argsJSON json.RawMessage) (string, error) {
			var params tools.GetProjectAnalyticsParams
			if err := json.Unmarshal(argsJSON, &params); err != nil {
				return "", err
			}
			return analyticsHandler.Handle(ctx, params)
		},
		"semantic_search": func(ctx context.Context, argsJSON json.RawMessage) (string, error) {
			var params tools.SemanticSearchParams
			if err := json.Unmarshal(argsJSON, &params); err != nil {
				return "", err
			}
			return semanticHandler.Handle(ctx, params)
		},
		"trace_cross_language": func(ctx context.Context, argsJSON json.RawMessage) (string, error) {
			var params tools.TraceCrossLanguageParams
			if err := json.Unmarshal(argsJSON, &params); err != nil {
				return "", err
			}
			return traceHandler.Handle(ctx, params)
		},
		"list_projects": func(ctx context.Context, argsJSON json.RawMessage) (string, error) {
			var params tools.ListProjectsParams
			if err := json.Unmarshal(argsJSON, &params); err != nil {
				return "", err
			}
			return listHandler.Handle(ctx, params)
		},
	}

	return schemas, dispatch
}
