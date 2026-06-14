package tools

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/maraichr/lattice/internal/auth"
	"github.com/maraichr/lattice/internal/mcp"
	"github.com/maraichr/lattice/internal/store"
	"github.com/maraichr/lattice/internal/store/postgres"
)

// AnalyzeImpactParams are the parameters for the analyze_impact tool.
type AnalyzeImpactParams struct {
	Project    string `json:"project"`
	SymbolID   string `json:"symbol_id,omitempty"`
	SymbolName string `json:"symbol_name,omitempty"`
	ChangeType string `json:"change_type,omitempty"` // modify, delete, rename
	MaxDepth   int    `json:"max_depth,omitempty"`
}

// AnalyzeImpactHandler implements the analyze_impact MCP tool.
type AnalyzeImpactHandler struct {
	store  *store.Store
	logger *slog.Logger
}

// NewAnalyzeImpactHandler creates a new handler.
func NewAnalyzeImpactHandler(s *store.Store, logger *slog.Logger) *AnalyzeImpactHandler {
	return &AnalyzeImpactHandler{store: s, logger: logger}
}

// Handle performs downstream impact analysis from a symbol.
func (h *AnalyzeImpactHandler) Handle(ctx context.Context, params AnalyzeImpactParams) (string, error) {
	if params.SymbolID == "" && params.SymbolName == "" {
		return "", fmt.Errorf("symbol_id or symbol_name is required")
	}
	if params.MaxDepth <= 0 {
		params.MaxDepth = 3
	}
	if params.ChangeType == "" {
		params.ChangeType = "modify"
	}

	project, err := h.store.GetProject(ctx, params.Project)
	if err != nil {
		return "", WrapProjectError(err)
	}
	if p, ok := auth.PrincipalFrom(ctx); ok && !p.IsAdmin() && project.TenantID != p.TenantID {
		return "", fmt.Errorf("access denied to project %s", params.Project)
	}

	// Resolve seed symbol (reuse lineage's resolveSeed pattern)
	seed, alternates, err := h.resolveSeed(ctx, project, params)
	if err != nil {
		return "", err
	}

	// Blast radius = everything that depends on the seed, i.e. BFS over INCOMING
	// edges. All edge types count: a rename breaks referencing code just as much
	// as calling code.
	affected, stats, err := TraverseEdges(ctx, h.store, seed, "upstream", params.MaxDepth, nil)
	if err != nil {
		return "", err
	}

	var direct, transitive []TravNode
	for _, n := range affected {
		if n.Depth == 1 {
			direct = append(direct, n)
		} else {
			transitive = append(transitive, n)
		}
	}

	// Format response
	rb := mcp.NewResponseBuilder(4000)
	rb.AddHeader(fmt.Sprintf("**Impact Analysis: %s %s**", params.ChangeType, seed.Name))
	rb.AddLine(fmt.Sprintf("Symbol: `%s` (%s, %s)", seed.QualifiedName, seed.Kind, seed.Language))
	total := len(direct) + len(transitive)
	rb.AddLine(fmt.Sprintf("Total affected: %d direct, %d transitive", len(direct), len(transitive)))
	rb.AddLine("")
	AddDisambiguationNote(rb, alternates)

	if len(direct) > 0 {
		rb.AddLine("### Direct Impact (depends on this symbol)")
		for _, n := range direct {
			severity := classifyImpactSeverity(params.ChangeType, n.Via)
			confStr := ""
			if n.Confidence > 0 {
				confStr = fmt.Sprintf(", confidence: %.2f", n.Confidence)
			}
			rb.AddLine(fmt.Sprintf("- %s `%s` [%s] via %s%s — **%s**",
				n.Symbol.Kind, n.Symbol.Name, n.Symbol.Language, n.Via, confStr, severity))
		}
		rb.AddLine("")
	}

	if len(transitive) > 0 {
		rb.AddLine("### Transitive Impact")
		for _, n := range transitive {
			confStr := ""
			if n.Confidence > 0 {
				confStr = fmt.Sprintf(", confidence: %.2f", n.Confidence)
			}
			rb.AddLine(fmt.Sprintf("- %s `%s` [%s] (depth %d, via %s%s)",
				n.Symbol.Kind, n.Symbol.Name, n.Symbol.Language, n.Depth, n.Via, confStr))
		}
		rb.AddLine("")
	}

	AddTraversalStats(rb, stats)

	if total == 0 {
		rb.AddLine("Nothing depends on this symbol. It can be changed without breaking other code in the indexed graph.")
	}

	return rb.Finalize(total, total), nil
}

func classifyImpactSeverity(changeType, edgeType string) string {
	switch changeType {
	case "delete":
		switch edgeType {
		case "calls", "references", "inherits", "implements":
			return "BREAKING"
		default:
			return "HIGH"
		}
	case "rename":
		switch edgeType {
		case "calls", "references":
			return "BREAKING"
		default:
			return "MEDIUM"
		}
	default: // modify
		switch edgeType {
		case "calls", "inherits", "implements":
			return "HIGH"
		default:
			return "LOW"
		}
	}
}

func (h *AnalyzeImpactHandler) resolveSeed(ctx context.Context, project postgres.Project, params AnalyzeImpactParams) (postgres.Symbol, []postgres.Symbol, error) {
	if params.SymbolID != "" {
		id, err := uuid.Parse(params.SymbolID)
		if err != nil {
			return postgres.Symbol{}, nil, fmt.Errorf("invalid symbol_id: %w", err)
		}
		sym, err := h.store.GetSymbol(ctx, id)
		if err != nil {
			return postgres.Symbol{}, nil, WrapSymbolError(err)
		}
		return sym, nil, nil
	}

	// Search by name with ranking
	return ResolveSymbolByNameDetailed(ctx, h.store, project.Slug, params.SymbolName)
}
