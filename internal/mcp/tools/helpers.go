package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/maraichr/lattice/internal/mcp"
	"github.com/maraichr/lattice/internal/store"
	"github.com/maraichr/lattice/internal/store/postgres"
)

// ToolHandler is the interface that all tool handlers implement.
type ToolHandler[P any] interface {
	Handle(ctx context.Context, params P) (string, error)
}

// WrapHandler adapts a ToolHandler into the SDK's AddTool callback.
// It handles nil params by using a zero value and maps errors to CallToolResult.
func WrapHandler[P any](h ToolHandler[P]) func(context.Context, *sdkmcp.CallToolRequest, *P) (*sdkmcp.CallToolResult, any, error) {
	return func(ctx context.Context, req *sdkmcp.CallToolRequest, params *P) (*sdkmcp.CallToolResult, any, error) {
		if params == nil {
			params = new(P)
		}
		result, err := h.Handle(ctx, *params)
		if err != nil {
			return &sdkmcp.CallToolResult{
				IsError: true,
				Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: err.Error()}},
			}, nil, nil
		}
		return &sdkmcp.CallToolResult{
			Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: result}},
		}, nil, nil
	}
}

// WrapProjectError translates database errors from GetProject into user-friendly messages.
func WrapProjectError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("project not found")
	}
	return fmt.Errorf("get project: %w", err)
}

// WrapSymbolError translates database errors from GetSymbol into user-friendly messages.
func WrapSymbolError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("symbol not found")
	}
	return fmt.Errorf("get symbol: %w", err)
}

// ResolveSymbolByName searches for a symbol by name using ranked search and returns the best match.
func ResolveSymbolByName(ctx context.Context, s *store.Store, projectSlug, name string) (postgres.Symbol, error) {
	sym, _, err := ResolveSymbolByNameDetailed(ctx, s, projectSlug, name)
	return sym, err
}

// resolutionRankConfig weights for picking a traversal seed from an ambiguous name.
// Kind priority is deliberately zero: a well-connected method is a better seed than
// an isolated class that happens to share the name. Centrality dominates ties.
func resolutionRankConfig() mcp.RankConfig {
	return mcp.RankConfig{
		QueryRelevance: 0.6,
		Centrality:     0.4,
	}
}

// ResolveSymbolByNameDetailed resolves a name to its best-matching symbol and also
// returns the runner-up candidates so callers can surface a disambiguation note.
func ResolveSymbolByNameDetailed(ctx context.Context, s *store.Store, projectSlug, name string) (postgres.Symbol, []postgres.Symbol, error) {
	results, err := s.SearchSymbolsRanked(ctx, postgres.SearchSymbolsRankedParams{
		ProjectSlug: projectSlug,
		Query:       &name,
		Kinds:       []string{},
		Languages:   []string{},
		Lim:         int32(10),
	})
	if err != nil {
		return postgres.Symbol{}, nil, fmt.Errorf("search symbol: %w", err)
	}
	if len(results) == 0 {
		return postgres.Symbol{}, nil, fmt.Errorf("no symbol found matching '%s'", name)
	}
	ranked := mcp.RankSymbols(results, name, resolutionRankConfig(), nil)

	best := ranked[0].Symbol
	var alternates []postgres.Symbol
	for _, r := range ranked[1:] {
		// Only exact name matches are plausible alternates worth flagging.
		if strings.EqualFold(r.Symbol.Name, name) || strings.EqualFold(r.Symbol.QualifiedName, name) {
			alternates = append(alternates, r.Symbol)
		}
	}
	return best, alternates, nil
}

// AddDisambiguationNote appends a note listing alternate symbols that matched the
// name, so the caller (an LLM) can retry with an explicit symbol_id.
func AddDisambiguationNote(rb *mcp.ResponseBuilder, alternates []postgres.Symbol) {
	if len(alternates) == 0 {
		return
	}
	rb.AddLine(fmt.Sprintf("_Name was ambiguous; %d other match(es). To target a different one, pass symbol_id:_", len(alternates)))
	for i, alt := range alternates {
		if i >= 5 {
			rb.AddLine(fmt.Sprintf("_… and %d more_", len(alternates)-i))
			break
		}
		rb.AddLine(fmt.Sprintf("- `%s` (%s, %s) — `%s`", alt.QualifiedName, alt.Kind, alt.Language, alt.ID))
	}
	rb.AddLine("")
}

// FlowEdgeTypes are the edge types that represent execution or data flow. Traversal
// tools follow these by default; structural edges (references, imports, inherits,
// implements, contains) fan out enormously and drown flow results in noise.
var FlowEdgeTypes = map[string]bool{
	"calls":      true,
	"calls_api":  true,
	"reads_from": true,
	"writes_to":  true,
	"uses_table": true,
	"joins":      true,
	"uses":       true,
}

// Traversal bounds. MaxFanout is per-node: a node with more qualifying edges than
// this is a hub; it is reported but not expanded further, since expanding hubs is
// what turns a focused trace into a project-wide dump. The seed node is exempt
// (callers asked about it specifically). NodeBudget bounds total results.
const (
	traversalMaxFanout  = 50
	traversalNodeBudget = 300
)

// TravNode is one symbol reached during a graph traversal.
type TravNode struct {
	Symbol     postgres.Symbol
	Depth      int
	Via        string // edge type that led here
	Confidence float64
	ParentID   uuid.UUID // symbol this node was reached from
	FromLang   string    // language of the parent symbol
}

// TravStats reports what a traversal had to leave out.
type TravStats struct {
	SkippedHubs []string // qualified names of nodes too connected to expand
	BudgetHit   bool
}

// TraverseEdges runs a bounded BFS from seed following incoming ("upstream") or
// outgoing ("downstream") edges. Only edge types in allowedEdges are followed
// (nil means all). Symbols are fetched in one batch per BFS level.
func TraverseEdges(ctx context.Context, s *store.Store, seed postgres.Symbol, direction string, maxDepth int, allowedEdges map[string]bool) ([]TravNode, TravStats, error) {
	visited := map[uuid.UUID]bool{seed.ID: true}
	var result []TravNode
	var stats TravStats

	type frontierEntry struct {
		sym   postgres.Symbol
		depth int
	}
	frontier := []frontierEntry{{sym: seed, depth: 0}}

	for len(frontier) > 0 && !stats.BudgetHit {
		type pendingEdge struct {
			parent     postgres.Symbol
			depth      int
			edgeType   string
			confidence float64
			neighborID uuid.UUID
		}
		var pending []pendingEdge

		for _, cur := range frontier {
			if cur.depth >= maxDepth {
				continue
			}

			var edges []postgres.SymbolEdge
			var err error
			if direction == "upstream" {
				edges, err = s.GetIncomingEdges(ctx, cur.sym.ID)
			} else {
				edges, err = s.GetOutgoingEdges(ctx, cur.sym.ID)
			}
			if err != nil {
				continue
			}

			var qualifying []postgres.SymbolEdge
			for _, e := range edges {
				if allowedEdges != nil && !allowedEdges[e.EdgeType] {
					continue
				}
				qualifying = append(qualifying, e)
			}

			// Hub guard: don't expand mega-connected non-seed nodes.
			if cur.depth > 0 && len(qualifying) > traversalMaxFanout {
				stats.SkippedHubs = append(stats.SkippedHubs, cur.sym.QualifiedName)
				continue
			}

			for _, e := range qualifying {
				neighborID := e.TargetID
				if direction == "upstream" {
					neighborID = e.SourceID
				}
				if visited[neighborID] {
					continue
				}
				visited[neighborID] = true
				pending = append(pending, pendingEdge{
					parent:     cur.sym,
					depth:      cur.depth + 1,
					edgeType:   e.EdgeType,
					confidence: extractEdgeConfidence(e.Metadata),
					neighborID: neighborID,
				})
			}
		}

		if len(pending) == 0 {
			break
		}

		// Batch-fetch this level's symbols.
		ids := make([]uuid.UUID, len(pending))
		for i, p := range pending {
			ids[i] = p.neighborID
		}
		syms, err := s.ListSymbolsByIDs(ctx, ids)
		if err != nil {
			return result, stats, fmt.Errorf("fetch traversal symbols: %w", err)
		}
		symByID := make(map[uuid.UUID]postgres.Symbol, len(syms))
		for _, sym := range syms {
			symByID[sym.ID] = sym
		}

		frontier = frontier[:0]
		for _, p := range pending {
			sym, ok := symByID[p.neighborID]
			if !ok {
				continue
			}
			result = append(result, TravNode{
				Symbol:     sym,
				Depth:      p.depth,
				Via:        p.edgeType,
				Confidence: p.confidence,
				ParentID:   p.parent.ID,
				FromLang:   p.parent.Language,
			})
			if len(result) >= traversalNodeBudget {
				stats.BudgetHit = true
				break
			}
			frontier = append(frontier, frontierEntry{sym: sym, depth: p.depth})
		}
	}

	return result, stats, nil
}

// AddTraversalStats appends notes about truncation so the reader knows the
// result is bounded rather than exhaustive.
func AddTraversalStats(rb *mcp.ResponseBuilder, stats TravStats) {
	if len(stats.SkippedHubs) > 0 {
		rb.AddLine(fmt.Sprintf("_%d highly-connected hub(s) not expanded (>%d connections): %s_",
			len(stats.SkippedHubs), traversalMaxFanout, summarizeList(stats.SkippedHubs, 3)))
	}
	if stats.BudgetHit {
		rb.AddLine(fmt.Sprintf("_Result truncated at %d nodes; narrow with max_depth or a more specific seed._", traversalNodeBudget))
	}
}

func summarizeList(items []string, max int) string {
	if len(items) <= max {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:max], ", ") + fmt.Sprintf(", … (+%d)", len(items)-max)
}
