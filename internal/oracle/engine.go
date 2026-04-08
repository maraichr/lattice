package oracle

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/maraichr/lattice/internal/embedding"
	"github.com/maraichr/lattice/internal/llm"
	"github.com/maraichr/lattice/internal/mcp/session"
	"github.com/maraichr/lattice/internal/mcp/tools"
	"github.com/maraichr/lattice/internal/store"
	"github.com/maraichr/lattice/internal/store/postgres"
)

// NOTE: LLM-based intent routing now lives in tools.AskCodebaseHandler.
// The Oracle engine is a thin wrapper that adds session management and
// structured block responses on top of ask_codebase.

// Request is the input to the Oracle engine.
type Request struct {
	Question  string `json:"question"`
	SessionID string `json:"session_id,omitempty"`
	Verbosity string `json:"verbosity,omitempty"`
}

// Engine is the Oracle core: delegates to ask_codebase MCP handler with session management.
type Engine struct {
	store   *store.Store
	session *session.Manager
	ask     *tools.AskCodebaseHandler
	logger  *slog.Logger
}

// NewEngine creates a new Oracle engine.
func NewEngine(s *store.Store, sm *session.Manager, llmClient *llm.Client, embedder embedding.Embedder, logger *slog.Logger) *Engine {
	return &Engine{
		store:   s,
		session: sm,
		ask:     tools.NewAskCodebaseHandler(s, sm, embedder, llmClient, logger),
		logger:  logger,
	}
}

// Store returns the underlying store for project lookups in the handler.
func (e *Engine) Store() *store.Store {
	return e.store
}

// Ask processes a user question for a given project.
// Routing is handled by ask_codebase (LLM when available, keyword fallback).
func (e *Engine) Ask(ctx context.Context, project postgres.Project, req Request) (*Response, error) {
	// 1. Load/create session
	sess, err := e.session.Load(ctx, req.SessionID)
	if err != nil {
		e.logger.Warn("failed to load session, creating new", slog.String("error", err.Error()))
		sess, _ = e.session.Load(ctx, "")
	}

	e.logger.Info("oracle query",
		slog.String("question", req.Question),
		slog.String("session", sess.ID))

	// 2. Delegate to ask_codebase (handles LLM routing + keyword fallback internally)
	markdown, err := e.ask.Handle(ctx, tools.AskCodebaseParams{
		Project:   project.Slug,
		Question:  req.Question,
		SessionID: sess.ID,
		Verbosity: req.Verbosity,
	})
	if err != nil {
		return nil, fmt.Errorf("ask_codebase: %w", err)
	}

	// 3. Build Oracle response
	blocks := []Block{
		textBlock(markdown),
	}
	hints := generateHints("ask_codebase")

	// 4. Update session
	sess.AddQuery(req.Question)
	sess.AddRecap(fmt.Sprintf("Asked about: %s", req.Question))
	if err := e.session.Save(ctx, sess); err != nil {
		e.logger.Warn("failed to save session", slog.String("error", err.Error()))
	}

	return &Response{
		SessionID: sess.ID,
		Tool:      "ask_codebase",
		Blocks:    blocks,
		Hints:     hints,
		Meta: ResponseMeta{
			ToolSelected: "ask_codebase",
			TotalResults: 1,
			Shown:        1,
		},
	}, nil
}

// generateHints produces follow-up question suggestions based on the tool used.
func generateHints(tool string) []Hint {
	switch tool {
	case "search":
		return []Hint{
			{Label: "Impact", Question: "What happens if I change this symbol?"},
			{Label: "Lineage", Question: "Show data flow for this symbol"},
			{Label: "Related", Question: "Show everything related to this symbol"},
		}
	case "ranking":
		return []Hint{
			{Label: "Overview", Question: "Give me a project overview"},
			{Label: "Relationships", Question: "Show table relationships"},
		}
	case "overview", "ask_codebase":
		return []Hint{
			{Label: "Top tables", Question: "What are the most important tables?"},
			{Label: "Architecture", Question: "Show table relationships"},
			{Label: "Cross-language", Question: "Trace the full stack from app code to database"},
		}
	case "subgraph", "relationships":
		return []Hint{
			{Label: "Top symbols", Question: "What are the most connected symbols?"},
		}
	case "lineage":
		return []Hint{
			{Label: "Impact", Question: "What would break if this changes?"},
		}
	case "impact":
		return []Hint{
			{Label: "Lineage", Question: "Show data flow for this symbol"},
		}
	case "cross_language":
		return []Hint{
			{Label: "Impact", Question: "What would break if this changes?"},
			{Label: "Overview", Question: "Give me a project overview"},
		}
	default:
		return nil
	}
}

