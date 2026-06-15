package tools

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"

	"github.com/maraichr/lattice/internal/auth"
	"github.com/maraichr/lattice/internal/mcp"
	"github.com/maraichr/lattice/internal/store"
	"github.com/maraichr/lattice/internal/store/postgres"
)

// GetLineageParams are the parameters for the get_lineage tool.
type GetLineageParams struct {
	Project    string `json:"project"`
	SymbolID   string `json:"symbol_id,omitempty"`
	SymbolName string `json:"symbol_name,omitempty"`
	Direction  string `json:"direction,omitempty"` // upstream, downstream, both
	MaxDepth   int    `json:"max_depth,omitempty"`
}

// GetLineageHandler implements the get_lineage MCP tool.
type GetLineageHandler struct {
	store  *store.Store
	logger *slog.Logger
}

// NewGetLineageHandler creates a new handler.
func NewGetLineageHandler(s *store.Store, logger *slog.Logger) *GetLineageHandler {
	return &GetLineageHandler{store: s, logger: logger}
}

// Handle traces upstream or downstream lineage from a symbol. Only flow edges
// (calls, reads/writes, joins) are followed: lineage answers "where does data/control
// come from or go to", which structural reference edges do not.
func (h *GetLineageHandler) Handle(ctx context.Context, params GetLineageParams) (string, error) {
	if params.SymbolID == "" && params.SymbolName == "" {
		return "", fmt.Errorf("symbol_id or symbol_name is required")
	}
	if params.MaxDepth <= 0 {
		params.MaxDepth = 3
	}
	if params.Direction == "" {
		params.Direction = "both"
	}

	project, err := h.store.GetProject(ctx, params.Project)
	if err != nil {
		return "", WrapProjectError(err)
	}
	if p, ok := auth.PrincipalFrom(ctx); ok && !p.IsAdmin() && project.TenantID != p.TenantID {
		return "", fmt.Errorf("access denied to project %s", params.Project)
	}

	seed, alternates, err := h.resolveSeed(ctx, project, params)
	if err != nil {
		return "", err
	}

	var upstream, downstream []TravNode
	var upStats, downStats TravStats

	if params.Direction == "upstream" || params.Direction == "both" {
		upstream, upStats, err = TraverseEdges(ctx, h.store, seed, "upstream", params.MaxDepth, FlowEdgeTypes)
		if err != nil {
			return "", err
		}
	}
	if params.Direction == "downstream" || params.Direction == "both" {
		downstream, downStats, err = TraverseEdges(ctx, h.store, seed, "downstream", params.MaxDepth, FlowEdgeTypes)
		if err != nil {
			return "", err
		}
	}

	rb := mcp.NewResponseBuilder(4000)
	rb.AddHeader(fmt.Sprintf("**Lineage for: %s** (%s)", seed.Name, params.Direction))
	rb.AddLine(fmt.Sprintf("Seed: `%s` (%s, %s)", seed.QualifiedName, seed.Kind, seed.Language))
	rb.AddLine("")
	AddDisambiguationNote(rb, alternates)

	if len(upstream) > 0 {
		rb.AddLine("### Upstream (data sources / callers)")
		renderLineageTree(rb, seed.ID, upstream)
		AddTraversalStats(rb, upStats)
		rb.AddLine("")
	}

	if len(downstream) > 0 {
		rb.AddLine("### Downstream (consumers / dependents)")
		renderLineageTree(rb, seed.ID, downstream)
		AddTraversalStats(rb, downStats)
		rb.AddLine("")
	}

	if len(upstream) == 0 && len(downstream) == 0 {
		rb.AddLine("No lineage connections found for this symbol (flow edges only: calls, reads, writes, joins).")
	}

	total := len(upstream) + len(downstream)
	return rb.Finalize(total, total), nil
}

// renderLineageTree prints nodes in DFS order so children appear under the node
// they were reached from, rather than in interleaved BFS order.
func renderLineageTree(rb *mcp.ResponseBuilder, rootID uuid.UUID, nodes []TravNode) {
	children := make(map[uuid.UUID][]TravNode, len(nodes))
	for _, n := range nodes {
		children[n.ParentID] = append(children[n.ParentID], n)
	}

	var render func(parentID uuid.UUID)
	render = func(parentID uuid.UUID) {
		for _, n := range children[parentID] {
			indent := strings.Repeat("  ", n.Depth-1)
			confStr := ""
			if n.Confidence > 0 {
				confStr = fmt.Sprintf(", confidence: %.2f", n.Confidence)
			}
			rb.AddLine(fmt.Sprintf("%s- %s `%s` [%s] (via %s%s)",
				indent, n.Symbol.Kind, n.Symbol.Name, n.Symbol.Language, n.Via, confStr))
			render(n.Symbol.ID)
		}
	}
	render(rootID)
}

func (h *GetLineageHandler) resolveSeed(ctx context.Context, project postgres.Project, params GetLineageParams) (postgres.Symbol, []postgres.Symbol, error) {
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

	return ResolveSymbolByNameDetailed(ctx, h.store, project.Slug, params.SymbolName)
}
