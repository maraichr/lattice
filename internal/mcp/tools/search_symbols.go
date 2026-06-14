package tools

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/maraichr/lattice/internal/auth"
	"github.com/maraichr/lattice/internal/mcp"
	"github.com/maraichr/lattice/internal/mcp/session"
	"github.com/maraichr/lattice/internal/store"
	"github.com/maraichr/lattice/internal/store/postgres"
)

// SearchSymbolsParams are the parameters for the search_symbols tool.
type SearchSymbolsParams struct {
	Project           string   `json:"project"`
	Query             string   `json:"query"`
	Kinds             []string `json:"kinds,omitempty"`
	Languages         []string `json:"languages,omitempty"`
	Limit             int32    `json:"limit,omitempty"`
	Verbosity         string   `json:"verbosity,omitempty"`
	MaxResponseTokens int      `json:"max_response_tokens,omitempty"`
	SessionID         string   `json:"session_id,omitempty"`
}

// SearchSymbolsHandler implements the search_symbols MCP tool.
type SearchSymbolsHandler struct {
	store   *store.Store
	session *session.Manager
	logger  *slog.Logger
}

// NewSearchSymbolsHandler creates a new handler.
func NewSearchSymbolsHandler(s *store.Store, sm *session.Manager, logger *slog.Logger) *SearchSymbolsHandler {
	return &SearchSymbolsHandler{store: s, session: sm, logger: logger}
}

// Handle searches for symbols by name/query within a project.
func (h *SearchSymbolsHandler) Handle(ctx context.Context, params SearchSymbolsParams) (string, error) {
	if params.Query == "" {
		return "", fmt.Errorf("query is required")
	}
	if params.Limit <= 0 {
		params.Limit = 20
	}
	if params.MaxResponseTokens <= 0 {
		params.MaxResponseTokens = 4000
	}

	project, err := h.store.GetProject(ctx, params.Project)
	if err != nil {
		return "", WrapProjectError(err)
	}
	if p, ok := auth.PrincipalFrom(ctx); ok && !p.IsAdmin() && project.TenantID != p.TenantID {
		return "", fmt.Errorf("access denied to project %s", params.Project)
	}

	kinds := params.Kinds
	if kinds == nil {
		kinds = []string{}
	}
	languages := params.Languages
	if languages == nil {
		languages = []string{}
	}

	// SearchSymbolsRanked orders exact name matches first (then prefix, then
	// substring, with in-degree as tiebreak) BEFORE applying LIMIT. The plain
	// SearchSymbols query orders alphabetically, which truncates away the most
	// relevant matches on common substrings.
	query := params.Query
	results, err := h.store.SearchSymbolsRanked(ctx, postgres.SearchSymbolsRankedParams{
		ProjectSlug: project.Slug,
		Query:       &query,
		Kinds:       kinds,
		Languages:   languages,
		Lim:         params.Limit,
	})
	if err != nil {
		return "", fmt.Errorf("search symbols: %w", err)
	}

	if len(results) == 0 {
		return fmt.Sprintf("No symbols found matching '%s'.", params.Query), nil
	}

	var sess *session.Session
	if h.session != nil && params.SessionID != "" {
		sess, _ = h.session.Load(ctx, params.SessionID)
	}

	verbosity := mcp.ParseVerbosity(params.Verbosity)
	ranked := mcp.RankSymbols(results, params.Query, mcp.DefaultRankConfig(), sess)

	rb := mcp.NewResponseBuilder(params.MaxResponseTokens)
	rb.AddHeader(fmt.Sprintf("**Search results for: %s** (%d matches)", params.Query, len(results)))

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
