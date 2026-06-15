package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"

	"github.com/maraichr/lattice/internal/auth"
	"github.com/maraichr/lattice/internal/mcp"
	"github.com/maraichr/lattice/internal/store"
	"github.com/maraichr/lattice/internal/store/postgres"
)

// TraceCrossLanguageParams are the parameters for the trace_cross_language tool.
type TraceCrossLanguageParams struct {
	Project    string `json:"project"`
	SymbolID   string `json:"symbol_id,omitempty"`
	SymbolName string `json:"symbol_name,omitempty"`
	Direction  string `json:"direction,omitempty"` // upstream, downstream, full (default: full)
	MaxDepth   int    `json:"max_depth,omitempty"` // default: 5
	SessionID  string `json:"session_id,omitempty"`
}

// TraceCrossLanguageHandler implements the trace_cross_language MCP tool.
type TraceCrossLanguageHandler struct {
	store  *store.Store
	logger *slog.Logger
}

// NewTraceCrossLanguageHandler creates a new handler.
func NewTraceCrossLanguageHandler(s *store.Store, logger *slog.Logger) *TraceCrossLanguageHandler {
	return &TraceCrossLanguageHandler{store: s, logger: logger}
}

// Handle traces cross-language paths from a symbol, grouping by stack layer.
// Only flow edges are followed (calls, reads/writes, joins): language bridges are
// execution/data transitions, and structural reference edges only add noise.
func (h *TraceCrossLanguageHandler) Handle(ctx context.Context, params TraceCrossLanguageParams) (string, error) {
	if params.SymbolID == "" && params.SymbolName == "" {
		return "", fmt.Errorf("symbol_id or symbol_name is required")
	}
	if params.MaxDepth <= 0 {
		params.MaxDepth = 5
	}
	if params.Direction == "" {
		params.Direction = "full"
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

	if params.Direction == "upstream" || params.Direction == "full" {
		upstream, upStats, err = TraverseEdges(ctx, h.store, seed, "upstream", params.MaxDepth, FlowEdgeTypes)
		if err != nil {
			return "", err
		}
	}
	if params.Direction == "downstream" || params.Direction == "full" {
		downstream, downStats, err = TraverseEdges(ctx, h.store, seed, "downstream", params.MaxDepth, FlowEdgeTypes)
		if err != nil {
			return "", err
		}
	}

	// Count language transitions and confidence across both directions.
	langTransitions := 0
	var totalConfidence float64
	confCount := 0
	for _, n := range append(append([]TravNode{}, upstream...), downstream...) {
		if n.FromLang != "" && n.Symbol.Language != n.FromLang {
			langTransitions++
			if n.Confidence > 0 {
				totalConfidence += n.Confidence
				confCount++
			}
		}
	}

	// Format response grouped by layer
	rb := mcp.NewResponseBuilder(4000)
	rb.AddHeader(fmt.Sprintf("**Stack Trace: %s** (%s)", seed.Name, params.Direction))
	rb.AddLine(fmt.Sprintf("Seed: `%s` (%s, %s)", seed.QualifiedName, seed.Kind, seed.Language))
	rb.AddLine("")
	AddDisambiguationNote(rb, alternates)

	if len(upstream) > 0 {
		rb.AddLine("### Upstream (callers / data sources)")
		formatLayerGrouped(rb, upstream)
		AddTraversalStats(rb, upStats)
		rb.AddLine("")
	}

	rb.AddLine(fmt.Sprintf("### Seed: `%s` [%s]", seed.Name, seed.Language))
	rb.AddLine("")

	if len(downstream) > 0 {
		rb.AddLine("### Downstream (consumers / dependencies)")
		formatLayerGrouped(rb, downstream)
		AddTraversalStats(rb, downStats)
		rb.AddLine("")
	}

	if len(upstream) == 0 && len(downstream) == 0 {
		rb.AddLine("No cross-language connections found for this symbol (flow edges only: calls, reads, writes, joins).")
	}

	// Bridge summary
	avgConf := 0.0
	if confCount > 0 {
		avgConf = totalConfidence / float64(confCount)
	}
	rb.AddLine("")
	rb.AddLine(fmt.Sprintf("**Bridge Summary:** %d language transitions", langTransitions))
	if confCount > 0 {
		rb.AddLine(fmt.Sprintf("Average confidence: %.2f", avgConf))
	}

	return rb.Finalize(len(upstream)+len(downstream), len(upstream)+len(downstream)), nil
}

// formatLayerGrouped groups nodes by inferred layer and language.
func formatLayerGrouped(rb *mcp.ResponseBuilder, nodes []TravNode) {
	// Group by layer
	type layerGroup struct {
		layer string
		lang  string
		nodes []TravNode
	}

	var groups []layerGroup
	groupMap := make(map[string]int)

	for _, n := range nodes {
		layer := inferLayer(n.Symbol)
		key := layer + "|" + n.Symbol.Language
		if idx, ok := groupMap[key]; ok {
			groups[idx].nodes = append(groups[idx].nodes, n)
		} else {
			groupMap[key] = len(groups)
			groups = append(groups, layerGroup{layer: layer, lang: n.Symbol.Language, nodes: []TravNode{n}})
		}
	}

	for i, g := range groups {
		rb.AddLine(fmt.Sprintf("**%s Layer** [%s]", capitalize(g.layer), g.lang))
		for _, n := range g.nodes {
			confStr := ""
			if n.Confidence > 0 {
				confStr = fmt.Sprintf(", confidence: %.2f", n.Confidence)
			}
			crossStr := ""
			if n.Symbol.Language != n.FromLang && n.FromLang != "" {
				crossStr = fmt.Sprintf(" (%s → %s)", n.FromLang, n.Symbol.Language)
			}
			rb.AddLine(fmt.Sprintf("- %s `%s` via %s%s%s",
				n.Symbol.Kind, n.Symbol.Name, n.Via, crossStr, confStr))
		}
		if i < len(groups)-1 {
			rb.AddLine("")
		}
	}
}

// inferLayer determines the architectural layer from symbol metadata or language.
func inferLayer(sym postgres.Symbol) string {
	// Check metadata for pre-computed layer; "unknown" falls through to the
	// language heuristic rather than producing an "Unknown Layer" group.
	if len(sym.Metadata) > 0 {
		var meta map[string]interface{}
		if json.Unmarshal(sym.Metadata, &meta) == nil {
			if layer, ok := meta["layer"].(string); ok && layer != "" && !strings.EqualFold(layer, "unknown") {
				return layer
			}
		}
	}

	// Infer from language
	switch strings.ToLower(sym.Language) {
	case "tsql", "pgsql":
		return "database"
	case "csharp", "java":
		return "api"
	case "javascript", "typescript":
		return "ui"
	case "asp", "aspx":
		return "api"
	case "delphi", "pascal":
		return "api"
	default:
		return "unknown"
	}
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func extractEdgeConfidence(metadata []byte) float64 {
	if len(metadata) == 0 {
		return 0
	}
	var meta map[string]interface{}
	if json.Unmarshal(metadata, &meta) != nil {
		return 0
	}
	if conf, ok := meta["confidence"].(float64); ok {
		return conf
	}
	return 0
}

func (h *TraceCrossLanguageHandler) resolveSeed(ctx context.Context, project postgres.Project, params TraceCrossLanguageParams) (postgres.Symbol, []postgres.Symbol, error) {
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
