package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/maraichr/lattice/internal/auth"
	"github.com/maraichr/lattice/internal/embedding"
	"github.com/maraichr/lattice/internal/llm"
	"github.com/maraichr/lattice/internal/mcp"
	"github.com/maraichr/lattice/internal/mcp/session"
	"github.com/maraichr/lattice/internal/store"
	"github.com/maraichr/lattice/internal/store/postgres"
)

const (
	maxAgentIterations = 5
	agentLoopTimeout   = 90 * time.Second
	maxToolResultLen   = 4000 // truncate tool results to keep context manageable
)

// AskCodebaseParams are the parameters for the ask_codebase meta-tool.
type AskCodebaseParams struct {
	Project           string   `json:"project"`
	Question          string   `json:"question"`
	Kinds             []string `json:"kinds,omitempty"`
	Languages         []string `json:"languages,omitempty"`
	MaxResponseTokens int      `json:"max_response_tokens,omitempty"`
	SessionID         string   `json:"session_id,omitempty"`
	Verbosity         string   `json:"verbosity,omitempty"`
}

// AskCodebaseHandler routes natural language questions to appropriate tool chains.
type AskCodebaseHandler struct {
	store     *store.Store
	session   *session.Manager
	llm       *llm.Client
	embedder  embedding.Embedder
	search    *SearchSymbolsHandler
	semantic  *SemanticSearchHandler
	subgraph  *ExtractSubgraphHandler
	impact    *AnalyzeImpactHandler
	lineage   *GetLineageHandler
	trace     *TraceCrossLanguageHandler
	analytics *GetProjectAnalyticsHandler
	logger    *slog.Logger
}

// NewAskCodebaseHandler creates a new intent router handler.
func NewAskCodebaseHandler(s *store.Store, sm *session.Manager, embedder embedding.Embedder, llmClient *llm.Client, logger *slog.Logger) *AskCodebaseHandler {
	return &AskCodebaseHandler{
		store:     s,
		session:   sm,
		llm:       llmClient,
		embedder:  embedder,
		search:    NewSearchSymbolsHandler(s, sm, logger),
		semantic:  NewSemanticSearchHandler(s, embedder, logger),
		subgraph:  NewExtractSubgraphHandler(s, sm, embedder, logger),
		impact:    NewAnalyzeImpactHandler(s, logger),
		lineage:   NewGetLineageHandler(s, logger),
		trace:     NewTraceCrossLanguageHandler(s, logger),
		analytics: NewGetProjectAnalyticsHandler(s, logger),
		logger:    logger,
	}
}

// Intent represents a classified question intent.
type Intent string

const (
	IntentSearch        Intent = "search"
	IntentImpact        Intent = "impact"
	IntentLineage       Intent = "lineage"
	IntentOverview      Intent = "overview"
	IntentSubgraph      Intent = "subgraph"
	IntentDeps          Intent = "dependencies"
	IntentRanking       Intent = "ranking"
	IntentRelationships Intent = "relationships"
	IntentBridges       Intent = "bridges"
	IntentAnalytics     Intent = "analytics"
	IntentCrossLanguage Intent = "cross_language"
)

// Handle classifies the question intent and routes to the appropriate tool chain.
// When an LLM client is available, it runs a multi-step agent loop that can call
// multiple tools and synthesize results. Falls back to keyword heuristics.
func (h *AskCodebaseHandler) Handle(ctx context.Context, params AskCodebaseParams) (string, error) {
	if params.MaxResponseTokens <= 0 {
		params.MaxResponseTokens = 4000
	}

	if h.llm != nil {
		result, err := h.runAgentLoop(ctx, params)
		if err != nil {
			h.logger.Warn("agent loop failed, falling back to keywords",
				slog.String("error", err.Error()),
				slog.String("question", params.Question))
		} else {
			return result, nil
		}
	}

	return h.keywordDispatch(ctx, params)
}

// --- Agent loop ---

const agentSystemPrompt = `You are a codebase analysis agent. You answer questions about a software project by calling tools to gather data, then synthesizing a clear markdown answer.

Available tools let you search for symbols, trace data lineage, analyze impact of changes, extract connected subgraphs, trace cross-language flows, and get project analytics.

TURN BUDGET: You have a maximum of %d tool-calling turns. Plan your calls carefully.
- Use turn 1 to search/discover symbols. You may call multiple tools in a single turn.
- Use middle turns for deeper analysis (lineage, impact, cross-language traces).
- You MUST reserve your final turn to respond with a text answer — do NOT call tools on your last turn.
- If you only need one tool call, do it on turn 1, then answer on turn 2.

Guidelines:
- The project is already set — do not ask for it.
- Start by searching or exploring to find relevant symbols before calling lineage/impact tools.
- If a symbol name is ambiguous, search first to find the exact name.
- When you have enough information, respond with a final markdown answer (no tool calls).
- Be concise but thorough. Use markdown formatting.
- If no results are found, say so clearly rather than guessing.`

func (h *AskCodebaseHandler) runAgentLoop(ctx context.Context, params AskCodebaseParams) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, agentLoopTimeout)
	defer cancel()

	var sessionRecap string
	if h.session != nil && params.SessionID != "" {
		if sess, err := h.session.Load(ctx, params.SessionID); err == nil {
			sessionRecap = sess.RecapText()
		}
	}

	userContent := params.Question
	if sessionRecap != "" {
		userContent = fmt.Sprintf("Prior context:\n%s\n\nQuestion: %s", sessionRecap, params.Question)
	}

	systemContent := fmt.Sprintf(agentSystemPrompt, maxAgentIterations)
	messages := []llm.Message{
		{Role: "system", Content: systemContent},
		{Role: "user", Content: userContent},
	}

	catalog := agentToolCatalog()

	for i := range maxAgentIterations {
		resp, err := h.llm.CompleteWithTools(ctx, messages, catalog)
		if err != nil {
			return "", fmt.Errorf("agent iteration %d: %w", i, err)
		}

		// If the LLM responded with text (no tool calls), it's the final answer.
		if len(resp.ToolCalls) == 0 {
			if resp.Content == "" {
				return "", fmt.Errorf("agent returned empty response at iteration %d", i)
			}
			return resp.Content, nil
		}

		// Append the assistant message with tool calls.
		messages = append(messages, *resp)

		// Execute each tool call and add results as tool messages.
		for _, tc := range resp.ToolCalls {
			h.logger.Info("agent tool call",
				slog.String("tool", tc.Function.Name),
				slog.Int("iteration", i),
				slog.Int("remaining_turns", maxAgentIterations-i-1))

			result := h.dispatchToolCall(ctx, tc.Function.Name, tc.Function.Arguments, params.Project)
			if len(result) > maxToolResultLen {
				result = result[:maxToolResultLen] + "\n\n... (truncated)"
			}
			messages = append(messages, llm.Message{
				Role:       "tool",
				Content:    result,
				ToolCallID: tc.ID,
			})
		}

		// On the penultimate turn, nudge the LLM to answer next.
		if i == maxAgentIterations-2 {
			messages = append(messages, llm.Message{
				Role:    "user",
				Content: "This is your last turn — respond with your final answer now. Do NOT call any more tools.",
			})
		}
	}

	// If we still got tool calls on the last iteration, force a text-only completion.
	messages = append(messages, llm.Message{
		Role:    "user",
		Content: "You have used all your tool-calling turns. Synthesize everything you've gathered into a final answer now.",
	})
	final, err := h.llm.Complete(ctx, messages)
	if err != nil {
		return "", fmt.Errorf("agent final synthesis: %w", err)
	}
	return final, nil
}

// dispatchToolCall routes a tool call to the appropriate handler and returns the result string.
func (h *AskCodebaseHandler) dispatchToolCall(ctx context.Context, toolName, argsJSON, project string) string {
	switch toolName {
	case "search_symbols":
		var p SearchSymbolsParams
		if err := json.Unmarshal([]byte(argsJSON), &p); err != nil {
			return fmt.Sprintf("Error parsing search_symbols args: %s", err)
		}
		p.Project = project
		if p.Limit <= 0 {
			p.Limit = 20
		}
		result, err := h.search.Handle(ctx, p)
		if err != nil {
			return fmt.Sprintf("search_symbols error: %s", err)
		}
		return result

	case "get_lineage":
		var p GetLineageParams
		if err := json.Unmarshal([]byte(argsJSON), &p); err != nil {
			return fmt.Sprintf("Error parsing get_lineage args: %s", err)
		}
		p.Project = project
		if p.MaxDepth <= 0 {
			p.MaxDepth = 5
		}
		result, err := h.lineage.Handle(ctx, p)
		if err != nil {
			return fmt.Sprintf("get_lineage error: %s", err)
		}
		return result

	case "analyze_impact":
		var p AnalyzeImpactParams
		if err := json.Unmarshal([]byte(argsJSON), &p); err != nil {
			return fmt.Sprintf("Error parsing analyze_impact args: %s", err)
		}
		p.Project = project
		if p.MaxDepth <= 0 {
			p.MaxDepth = 3
		}
		result, err := h.impact.Handle(ctx, p)
		if err != nil {
			return fmt.Sprintf("analyze_impact error: %s", err)
		}
		return result

	case "extract_subgraph":
		var p ExtractSubgraphParams
		if err := json.Unmarshal([]byte(argsJSON), &p); err != nil {
			return fmt.Sprintf("Error parsing extract_subgraph args: %s", err)
		}
		p.Project = project
		if p.MaxNodes <= 0 {
			p.MaxNodes = 30
		}
		if p.MaxDepth <= 0 {
			p.MaxDepth = 2
		}
		result, err := h.subgraph.Handle(ctx, p)
		if err != nil {
			return fmt.Sprintf("extract_subgraph error: %s", err)
		}
		return result

	case "trace_cross_language":
		var p TraceCrossLanguageParams
		if err := json.Unmarshal([]byte(argsJSON), &p); err != nil {
			return fmt.Sprintf("Error parsing trace_cross_language args: %s", err)
		}
		p.Project = project
		if p.MaxDepth <= 0 {
			p.MaxDepth = 5
		}
		result, err := h.trace.Handle(ctx, p)
		if err != nil {
			return fmt.Sprintf("trace_cross_language error: %s", err)
		}
		return result

	case "get_project_analytics":
		var p GetProjectAnalyticsParams
		if err := json.Unmarshal([]byte(argsJSON), &p); err != nil {
			return fmt.Sprintf("Error parsing get_project_analytics args: %s", err)
		}
		p.Project = project
		if p.Scope == "" {
			p.Scope = "summary"
		}
		result, err := h.analytics.Handle(ctx, p)
		if err != nil {
			return fmt.Sprintf("get_project_analytics error: %s", err)
		}
		return result

	case "semantic_search":
		var p SemanticSearchParams
		if err := json.Unmarshal([]byte(argsJSON), &p); err != nil {
			return fmt.Sprintf("Error parsing semantic_search args: %s", err)
		}
		p.Project = project
		if p.TopK <= 0 {
			p.TopK = 10
		}
		result, err := h.semantic.Handle(ctx, p)
		if err != nil {
			return fmt.Sprintf("semantic_search error: %s", err)
		}
		return result

	default:
		return fmt.Sprintf("Unknown tool: %s", toolName)
	}
}

// agentToolCatalog returns the tool definitions exposed to the agent LLM.
func agentToolCatalog() []llm.ToolDef {
	return []llm.ToolDef{
		{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        "search_symbols",
				Description: "Find symbols (tables, procedures, classes, functions, etc.) by name or keyword. Use this first to discover exact symbol names before calling other tools.",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {
						"query": {"type": "string", "description": "Search query (name or keyword)"},
						"kinds": {"type": "array", "items": {"type": "string"}, "description": "Filter by kind: table, procedure, class, method, function, column, interface, enum"},
						"languages": {"type": "array", "items": {"type": "string"}, "description": "Filter by language: tsql, csharp, java, javascript, etc."},
						"limit": {"type": "integer", "description": "Max results (default 20)"}
					},
					"required": ["query"]
				}`),
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        "get_lineage",
				Description: "Trace the upstream (data sources, callers) or downstream (consumers, dependents) lineage of a symbol. Shows data flow and call chains.",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {
						"symbol_name": {"type": "string", "description": "Name of the symbol to trace"},
						"direction": {"type": "string", "enum": ["upstream", "downstream", "both"], "description": "Direction to trace (default: both)"},
						"max_depth": {"type": "integer", "description": "Max traversal depth (default 5)"}
					},
					"required": ["symbol_name"]
				}`),
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        "analyze_impact",
				Description: "Analyze the blast radius of modifying, deleting, or renaming a symbol. Shows direct and transitive impacts.",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {
						"symbol_name": {"type": "string", "description": "Name of the symbol to analyze"},
						"change_type": {"type": "string", "enum": ["modify", "delete", "rename"], "description": "Type of change (default: modify)"},
						"max_depth": {"type": "integer", "description": "Max impact depth (default 3)"}
					},
					"required": ["symbol_name"]
				}`),
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        "extract_subgraph",
				Description: "Extract a connected subgraph of symbols and relationships around a topic or set of seed symbols. Good for understanding modules and workflows.",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {
						"topic": {"type": "string", "description": "Topic to explore (e.g. 'authentication', 'order processing')"},
						"kinds": {"type": "array", "items": {"type": "string"}, "description": "Filter by symbol kinds"},
						"seed_symbols": {"type": "array", "items": {"type": "string"}, "description": "Starting symbol names to expand from"},
						"max_nodes": {"type": "integer", "description": "Max nodes to return (default 30)"}
					}
				}`),
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        "trace_cross_language",
				Description: "Trace cross-language paths from a symbol, showing how code flows across language boundaries (e.g. C# -> SQL). Groups results by stack layer.",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {
						"symbol_name": {"type": "string", "description": "Name of the symbol to trace"},
						"direction": {"type": "string", "enum": ["upstream", "downstream", "full"], "description": "Direction to trace (default: full)"},
						"max_depth": {"type": "integer", "description": "Max traversal depth (default 5)"}
					},
					"required": ["symbol_name"]
				}`),
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        "get_project_analytics",
				Description: "Get project-level analytics: summary stats, language distribution, symbol kind counts, architectural layer distribution, or cross-language bridges.",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {
						"scope": {"type": "string", "enum": ["summary", "languages", "kinds", "layers", "bridges"], "description": "Analytics scope (default: summary)"}
					}
				}`),
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        "semantic_search",
				Description: "Search symbols using natural language via vector embeddings. Finds conceptually similar symbols even without exact name matches.",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {
						"query": {"type": "string", "description": "Natural language search query"},
						"kinds": {"type": "array", "items": {"type": "string"}, "description": "Filter by symbol kinds"},
						"top_k": {"type": "integer", "description": "Number of results (default 10)"}
					},
					"required": ["query"]
				}`),
			},
		},
	}
}

// --- Keyword fallback dispatch ---

// keywordDispatch uses keyword heuristics to route to a single handler.
func (h *AskCodebaseHandler) keywordDispatch(ctx context.Context, params AskCodebaseParams) (string, error) {
	intent := classifyIntent(params.Question)
	h.logger.Info("keyword dispatch",
		slog.String("question", params.Question),
		slog.String("intent", string(intent)))

	switch intent {
	case IntentOverview:
		return h.handleOverview(ctx, params)
	case IntentRanking:
		return h.handleRanking(ctx, params)
	case IntentImpact:
		return h.handleImpact(ctx, params)
	case IntentLineage:
		return h.handleLineage(ctx, params)
	case IntentSubgraph:
		return h.handleSubgraph(ctx, params)
	case IntentDeps:
		return h.handleDependencies(ctx, params)
	case IntentRelationships:
		return h.handleRelationships(ctx, params)
	case IntentBridges:
		return h.handleBridges(ctx, params)
	case IntentAnalytics:
		return h.handleAnalytics(ctx, params)
	case IntentCrossLanguage:
		return h.handleCrossLanguage(ctx, params)
	default:
		return h.handleSearch(ctx, params)
	}
}

func classifyIntent(question string) Intent {
	q := strings.ToLower(question)

	rankingPatterns := []string{
		"most used", "most important", "most referenced", "most connected",
		"top ", "busiest", "highest", "largest", "most common",
		"most frequent", "most popular", "heavily used",
	}
	for _, p := range rankingPatterns {
		if strings.Contains(q, p) {
			return IntentRanking
		}
	}

	impactPatterns := []string{
		"what breaks", "what happens if", "impact", "blast radius",
		"change", "rename", "delete", "remove", "modify", "affected",
	}
	for _, p := range impactPatterns {
		if strings.Contains(q, p) {
			return IntentImpact
		}
	}

	lineagePatterns := []string{
		"data flow", "lineage", "where does", "data come from",
		"written to", "read from", "transforms", "populates",
	}
	for _, p := range lineagePatterns {
		if strings.Contains(q, p) {
			return IntentLineage
		}
	}

	crossLangPatterns := []string{
		"what tables does", "tables does this endpoint",
		"full stack", "stack trace", "stack slice", "end to end",
		"calls this stored proc", "calls this procedure",
		"from app code", "from the frontend", "from the api",
		"what touches", "who calls",
		"cross-language trace", "cross language trace",
	}
	for _, p := range crossLangPatterns {
		if strings.Contains(q, p) {
			return IntentCrossLanguage
		}
	}

	bridgePatterns := []string{
		"cross-language", "bridge", "bridges", "between languages",
		"polyglot", "multi-language",
	}
	for _, p := range bridgePatterns {
		if strings.Contains(q, p) {
			return IntentBridges
		}
	}

	analyticsPatterns := []string{
		"statistics", "stats", "distribution", "breakdown",
		"how many", "count", "metrics", "layer", "layers",
	}
	for _, p := range analyticsPatterns {
		if strings.Contains(q, p) {
			return IntentAnalytics
		}
	}

	overviewPatterns := []string{
		"overview", "what is this", "describe", "summary",
		"architecture", "structure", "languages", "how big",
	}
	for _, p := range overviewPatterns {
		if strings.Contains(q, p) {
			return IntentOverview
		}
	}

	relPatterns := []string{
		"foreign key", "foreign keys", "relationship", "relationships",
		"related to", "joins", "references between", "missing fk",
		"data access pattern",
	}
	for _, p := range relPatterns {
		if strings.Contains(q, p) {
			return IntentRelationships
		}
	}

	depPatterns := []string{
		"depends on", "dependency", "dependencies", "uses",
		"calls", "imports", "references",
	}
	for _, p := range depPatterns {
		if strings.Contains(q, p) {
			return IntentDeps
		}
	}

	subgraphPatterns := []string{
		"everything about", "all related", "module", "system",
		"pipeline", "workflow", "process",
	}
	for _, p := range subgraphPatterns {
		if strings.Contains(q, p) {
			return IntentSubgraph
		}
	}

	return IntentSearch
}

// --- Keyword handler methods ---

func (h *AskCodebaseHandler) handleOverview(ctx context.Context, params AskCodebaseParams) (string, error) {
	project, err := h.store.GetProject(ctx, params.Project)
	if err != nil {
		return "", WrapProjectError(err)
	}
	if p, ok := auth.PrincipalFrom(ctx); ok && !p.IsAdmin() && project.TenantID != p.TenantID {
		return "", fmt.Errorf("access denied to project %s", params.Project)
	}

	analytics, err := h.store.GetProjectAnalytics(ctx, postgres.GetProjectAnalyticsParams{
		ProjectID: project.ID,
		Scope:     "project",
		ScopeID:   "overview",
	})
	if err != nil {
		return fmt.Sprintf("Project '%s' found but no analytics computed yet. Run an indexing job first.", params.Project), nil
	}

	rb := mcp.NewResponseBuilder(params.MaxResponseTokens)
	rb.AddHeader(fmt.Sprintf("**Project Overview: %s**", project.Name))

	if analytics.Summary != nil {
		rb.AddLine(*analytics.Summary)
		rb.AddLine("")
	}

	layers, err := h.store.GetProjectAnalytics(ctx, postgres.GetProjectAnalyticsParams{
		ProjectID: project.ID,
		Scope:     "project",
		ScopeID:   "layers",
	})
	if err == nil && layers.Summary != nil {
		rb.AddLine(*layers.Summary)
	}

	bridges, err := h.store.ListProjectAnalyticsByScope(ctx, postgres.ListProjectAnalyticsByScopeParams{
		ProjectID: project.ID,
		Scope:     "bridge",
	})
	if err == nil && len(bridges) > 0 {
		rb.AddLine("")
		rb.AddLine("**Cross-language bridges:**")
		for _, b := range bridges {
			if b.Summary != nil {
				rb.AddLine(fmt.Sprintf("- %s", *b.Summary))
			}
		}
	}

	nav := mcp.NewNavigator(h.store.Queries)
	hints := nav.SuggestNextSteps("list_project_overview", nil, nil)
	return rb.FinalizeWithHints(1, 1, hints), nil
}

func (h *AskCodebaseHandler) handleRanking(ctx context.Context, params AskCodebaseParams) (string, error) {
	project, err := h.store.GetProject(ctx, params.Project)
	if err != nil {
		return "", WrapProjectError(err)
	}
	if p, ok := auth.PrincipalFrom(ctx); ok && !p.IsAdmin() && project.TenantID != p.TenantID {
		return "", fmt.Errorf("access denied to project %s", params.Project)
	}

	kinds := params.Kinds
	if len(kinds) == 0 {
		kinds = extractKindsFromQuestion(params.Question)
	}

	results, err := h.store.ListTopSymbolsByKind(ctx, postgres.ListTopSymbolsByKindParams{
		ProjectSlug: project.Slug,
		Kinds:       kinds,
		Languages:   params.Languages,
		Lim:         10,
	})
	if err != nil {
		return "", fmt.Errorf("list top symbols: %w", err)
	}

	if len(results) == 0 {
		return fmt.Sprintf("No symbols found matching the criteria (kinds=%v).", kinds), nil
	}

	verbosity := mcp.ParseVerbosity(params.Verbosity)
	rb := mcp.NewResponseBuilder(params.MaxResponseTokens)

	kindLabel := "symbols"
	if len(kinds) > 0 {
		kindLabel = strings.Join(kinds, "/") + "s"
	}
	rb.AddHeader(fmt.Sprintf("**Top %s by usage (in-degree)**", kindLabel))

	var sess *session.Session
	if h.session != nil && params.SessionID != "" {
		sess, _ = h.session.Load(ctx, params.SessionID)
	}

	returned := 0
	for _, sym := range results {
		if !rb.AddSymbolCard(sym, verbosity, sess) {
			break
		}
		returned++
	}

	nav := mcp.NewNavigator(h.store.Queries)
	hints := nav.SuggestNextSteps("search_symbols", results, sess)
	return rb.FinalizeWithHints(len(results), returned, hints), nil
}

func (h *AskCodebaseHandler) handleSearch(ctx context.Context, params AskCodebaseParams) (string, error) {
	project, err := h.store.GetProject(ctx, params.Project)
	if err != nil {
		return "", WrapProjectError(err)
	}
	if p, ok := auth.PrincipalFrom(ctx); ok && !p.IsAdmin() && project.TenantID != p.TenantID {
		return "", fmt.Errorf("access denied to project %s", params.Project)
	}

	searchTerms := extractSearchTerms(params.Question)
	kinds := params.Kinds
	if kinds == nil {
		kinds = []string{}
	}
	results, err := h.store.SearchSymbols(ctx, postgres.SearchSymbolsParams{
		ProjectSlug: project.Slug,
		Query:       &searchTerms,
		Kinds:       kinds,
		Languages:   params.Languages,
		Lim:         20,
	})
	if err != nil {
		return "", fmt.Errorf("search symbols: %w", err)
	}

	if len(results) == 0 {
		return fmt.Sprintf("No symbols found matching '%s'.", params.Question), nil
	}

	var sess *session.Session
	if h.session != nil && params.SessionID != "" {
		sess, _ = h.session.Load(ctx, params.SessionID)
	}

	verbosity := mcp.ParseVerbosity(params.Verbosity)
	ranked := mcp.RankSymbols(results, extractSearchTerms(params.Question), mcp.DefaultRankConfig(), sess)

	rb := mcp.NewResponseBuilder(params.MaxResponseTokens)
	rb.AddHeader(fmt.Sprintf("**Search results for: %s**", params.Question))

	returned := 0
	for _, r := range ranked {
		if !rb.AddSymbolCard(r.Symbol, verbosity, sess) {
			break
		}
		returned++
	}

	nav := mcp.NewNavigator(h.store.Queries)
	symbols := make([]postgres.Symbol, 0, len(ranked))
	for _, r := range ranked {
		symbols = append(symbols, r.Symbol)
	}
	hints := nav.SuggestNextSteps("search_symbols", symbols, sess)

	return rb.FinalizeWithHints(len(results), returned, hints), nil
}

func (h *AskCodebaseHandler) handleImpact(ctx context.Context, params AskCodebaseParams) (string, error) {
	symbolName := extractSearchTerms(params.Question)
	changeType := "modify"
	q := strings.ToLower(params.Question)
	if strings.Contains(q, "delete") || strings.Contains(q, "remove") || strings.Contains(q, "drop") {
		changeType = "delete"
	} else if strings.Contains(q, "rename") {
		changeType = "rename"
	}
	result, err := h.impact.Handle(ctx, AnalyzeImpactParams{
		Project:    params.Project,
		SymbolName: symbolName,
		ChangeType: changeType,
		MaxDepth:   3,
	})
	if err != nil {
		h.logger.Info("impact symbol not found, falling back to search",
			slog.String("symbol", symbolName),
			slog.String("error", err.Error()))
		return h.handleSearch(ctx, params)
	}
	return result, nil
}

func (h *AskCodebaseHandler) handleLineage(ctx context.Context, params AskCodebaseParams) (string, error) {
	symbolName := extractSearchTerms(params.Question)
	direction := "both"
	q := strings.ToLower(params.Question)
	if strings.Contains(q, "come from") || strings.Contains(q, "upstream") || strings.Contains(q, "data source") {
		direction = "upstream"
	} else if strings.Contains(q, "written to") || strings.Contains(q, "downstream") || strings.Contains(q, "populates") {
		direction = "downstream"
	}
	result, err := h.lineage.Handle(ctx, GetLineageParams{
		Project:    params.Project,
		SymbolName: symbolName,
		Direction:  direction,
		MaxDepth:   5,
	})
	if err != nil {
		h.logger.Info("lineage symbol not found, falling back to search",
			slog.String("symbol", symbolName),
			slog.String("error", err.Error()))
		return h.handleSearch(ctx, params)
	}
	return result, nil
}

func (h *AskCodebaseHandler) handleCrossLanguage(ctx context.Context, params AskCodebaseParams) (string, error) {
	symbolName := extractSearchTerms(params.Question)
	result, err := h.trace.Handle(ctx, TraceCrossLanguageParams{
		Project:    params.Project,
		SymbolName: symbolName,
		Direction:  "full",
		MaxDepth:   5,
		SessionID:  params.SessionID,
	})
	if err != nil {
		h.logger.Info("cross_language symbol not found, falling back to bridges",
			slog.String("symbol", symbolName),
			slog.String("error", err.Error()))
		return h.handleBridges(ctx, params)
	}
	return result, nil
}

func (h *AskCodebaseHandler) handleSubgraph(ctx context.Context, params AskCodebaseParams) (string, error) {
	topic := extractSearchTerms(params.Question)
	return h.subgraph.Handle(ctx, ExtractSubgraphParams{
		Project:           params.Project,
		Topic:             topic,
		MaxDepth:          2,
		MaxNodes:          30,
		MaxResponseTokens: params.MaxResponseTokens,
		SessionID:         params.SessionID,
		Verbosity:         params.Verbosity,
	})
}

func (h *AskCodebaseHandler) handleRelationships(ctx context.Context, params AskCodebaseParams) (string, error) {
	kinds := params.Kinds
	if len(kinds) == 0 {
		kinds = extractKindsFromQuestion(params.Question)
	}
	if len(kinds) == 0 {
		kinds = []string{"table"}
	}
	return h.subgraph.Handle(ctx, ExtractSubgraphParams{
		Project:           params.Project,
		Kinds:             kinds,
		MaxDepth:          1,
		MaxNodes:          100,
		MaxResponseTokens: params.MaxResponseTokens,
		SessionID:         params.SessionID,
		Verbosity:         "summary",
	})
}

func (h *AskCodebaseHandler) handleDependencies(ctx context.Context, params AskCodebaseParams) (string, error) {
	symbolName := extractSearchTerms(params.Question)
	result, err := h.lineage.Handle(ctx, GetLineageParams{
		Project:    params.Project,
		SymbolName: symbolName,
		Direction:  "downstream",
		MaxDepth:   5,
	})
	if err != nil {
		h.logger.Info("dependency symbol not found, falling back to search",
			slog.String("symbol", symbolName),
			slog.String("error", err.Error()))
		return h.handleSearch(ctx, params)
	}
	return result, nil
}

func (h *AskCodebaseHandler) handleBridges(ctx context.Context, params AskCodebaseParams) (string, error) {
	project, err := h.store.GetProject(ctx, params.Project)
	if err != nil {
		return "", WrapProjectError(err)
	}
	if p, ok := auth.PrincipalFrom(ctx); ok && !p.IsAdmin() && project.TenantID != p.TenantID {
		return "", fmt.Errorf("access denied to project %s", params.Project)
	}

	rows, err := h.store.GetCrossLanguageBridges(ctx, project.ID)
	if err != nil {
		return "", fmt.Errorf("get bridges: %w", err)
	}

	rb := mcp.NewResponseBuilder(params.MaxResponseTokens)
	rb.AddHeader(fmt.Sprintf("**Cross-Language Bridges: %s**", project.Name))

	if len(rows) == 0 {
		rb.AddLine("No cross-language bridges found.")
		return rb.Finalize(0, 0), nil
	}

	for _, r := range rows {
		rb.AddLine(fmt.Sprintf("- **%s → %s** via `%s`: %d edges",
			r.SourceLanguage, r.TargetLanguage, r.EdgeType, r.EdgeCount))
	}

	return rb.Finalize(len(rows), len(rows)), nil
}

func (h *AskCodebaseHandler) handleAnalytics(ctx context.Context, params AskCodebaseParams) (string, error) {
	q := strings.ToLower(params.Question)

	scope := "summary"
	if strings.Contains(q, "layer") || strings.Contains(q, "layers") {
		scope = "layers"
	} else if strings.Contains(q, "language") || strings.Contains(q, "languages") {
		scope = "languages"
	} else if strings.Contains(q, "kind") || strings.Contains(q, "kinds") || strings.Contains(q, "type") {
		scope = "kinds"
	}

	return h.analytics.Handle(ctx, GetProjectAnalyticsParams{
		Project: params.Project,
		Scope:   scope,
	})
}

// --- Utility functions ---

func extractKindsFromQuestion(question string) []string {
	q := strings.ToLower(question)
	kindMap := map[string]string{
		"table":     "table",
		"tables":    "table",
		"procedure": "procedure",
		"proc":      "procedure",
		"procs":     "procedure",
		"class":     "class",
		"classes":   "class",
		"method":    "method",
		"methods":   "method",
		"function":  "function",
		"functions": "function",
		"column":    "column",
		"columns":   "column",
		"interface": "interface",
		"field":     "field",
		"property":  "property",
		"enum":      "enum",
	}
	seen := make(map[string]bool)
	var kinds []string
	for word, kind := range kindMap {
		if strings.Contains(q, word) && !seen[kind] {
			seen[kind] = true
			kinds = append(kinds, kind)
		}
	}
	return kinds
}

func extractSearchTerms(question string) string {
	stopWords := map[string]bool{
		"what": true, "where": true, "how": true, "does": true, "is": true,
		"the": true, "a": true, "an": true, "are": true, "can": true,
		"do": true, "if": true, "i": true, "to": true, "of": true,
		"in": true, "for": true, "it": true, "this": true, "that": true,
		"about": true, "show": true, "me": true, "find": true, "get": true,
		"tell": true, "breaks": true, "happens": true, "everything": true,
	}

	words := strings.Fields(strings.ToLower(question))
	var terms []string
	for _, w := range words {
		w = strings.Trim(w, "?.,!\"'")
		if !stopWords[w] && len(w) > 1 {
			terms = append(terms, w)
		}
	}

	if len(terms) == 0 {
		return question
	}
	return strings.Join(terms, " ")
}
