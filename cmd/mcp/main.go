package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"

	"github.com/maraichr/lattice/internal/auth"
	"github.com/maraichr/lattice/internal/config"
	"github.com/maraichr/lattice/internal/embedding"
	"github.com/maraichr/lattice/internal/llm"
	"github.com/maraichr/lattice/internal/mcp"
	"github.com/maraichr/lattice/internal/mcp/tools"
	"github.com/maraichr/lattice/internal/store"
	"github.com/maraichr/lattice/internal/store/postgres"
	vk "github.com/maraichr/lattice/internal/store/valkey"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	cfg, err := config.Load()
	if err != nil {
		logger.Error("failed to load config", slog.String("error", err.Error()))
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Database
	pool, err := postgres.NewPool(ctx, cfg.Database.DSN(), cfg.Database.MaxConns, cfg.Database.MinConns)
	if err != nil {
		logger.Error("failed to connect to database", slog.String("error", err.Error()))
		os.Exit(1)
	}
	defer pool.Close()
	logger.Info("connected to database")

	s := store.New(pool)

	// Valkey (optional for sessions)
	vkClient, err := vk.NewClient(cfg.Valkey)
	if err != nil {
		logger.Warn("valkey unavailable, sessions disabled", slog.String("error", err.Error()))
	} else {
		defer vkClient.Close()
		logger.Info("connected to valkey")
	}

	// Embedder (optional for semantic search)
	embedder, err := embedding.NewEmbedder(cfg)
	if err != nil {
		logger.Warn("embedder unavailable, semantic search disabled", slog.String("error", err.Error()))
	} else if embedder != nil {
		logger.Info("embedder configured", slog.String("model", embedder.ModelID()))
	}

	// LLM client (optional — enables intelligent intent routing in ask_codebase)
	var llmClient *llm.Client
	if cfg.OpenRouter.APIKey != "" {
		llmClient = llm.NewClient(cfg.OpenRouter.APIKey, cfg.Oracle.Model, cfg.OpenRouter.BaseURL)
		logger.Info("LLM routing enabled for ask_codebase", slog.String("model", cfg.Oracle.Model))
	}

	// Create MCP server with infrastructure
	mcpServer := mcp.NewServer(mcp.ServerDeps{
		Store:        s,
		ValkeyClient: vkClient,
		Embedder:     embedder,
		LLM:          llmClient,
		Logger:       logger,
	})

	// Wire tool handlers (in cmd to avoid import cycle mcp <-> mcp/tools)
	extractSubgraph := tools.NewExtractSubgraphHandler(s, mcpServer.Session, embedder, logger)
	askCodebase := tools.NewAskCodebaseHandler(s, mcpServer.Session, embedder, mcpServer.LLM, logger)
	listProjects := tools.NewListProjectsHandler(s, logger)
	searchSymbols := tools.NewSearchSymbolsHandler(s, mcpServer.Session, logger)
	getLineage := tools.NewGetLineageHandler(s, logger)
	analyzeImpact := tools.NewAnalyzeImpactHandler(s, logger)
	getProjectAnalytics := tools.NewGetProjectAnalyticsHandler(s, logger)
	semanticSearch := tools.NewSemanticSearchHandler(s, embedder, logger)
	traceCrossLang := tools.NewTraceCrossLanguageHandler(s, logger)

	// SDK MCP server
	sdkServer := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "lattice", Version: "1.0.0"}, nil)

	// Register all tools using WrapHandler
	sdkmcp.AddTool(sdkServer, &sdkmcp.Tool{
		Name:        "extract_subgraph",
		Description: "Extract a subgraph of symbols and relationships around a topic or set of seed symbols. Returns symbol cards with metadata, edges, and navigation hints.",
	}, tools.WrapHandler[tools.ExtractSubgraphParams](extractSubgraph))

	sdkmcp.AddTool(sdkServer, &sdkmcp.Tool{
		Name:        "ask_codebase",
		Description: "Ask a natural language question about the codebase. Routes to overview, search, ranking, impact analysis, lineage tracing, or subgraph exploration.",
	}, tools.WrapHandler[tools.AskCodebaseParams](askCodebase))

	sdkmcp.AddTool(sdkServer, &sdkmcp.Tool{
		Name:        "list_projects",
		Description: "List all projects accessible to the authenticated user. Returns project slug, name, and description.",
	}, tools.WrapHandler[tools.ListProjectsParams](listProjects))

	sdkmcp.AddTool(sdkServer, &sdkmcp.Tool{
		Name:        "search_symbols",
		Description: "Search for symbols (tables, procedures, classes, functions, etc.) by name or keyword within a project. Supports filtering by kind and language.",
	}, tools.WrapHandler[tools.SearchSymbolsParams](searchSymbols))

	sdkmcp.AddTool(sdkServer, &sdkmcp.Tool{
		Name:        "get_lineage",
		Description: "Trace the upstream (data sources, callers) or downstream (consumers, dependents) lineage of a symbol. Useful for understanding data flow and call chains.",
	}, tools.WrapHandler[tools.GetLineageParams](getLineage))

	sdkmcp.AddTool(sdkServer, &sdkmcp.Tool{
		Name:        "analyze_impact",
		Description: "Analyze the blast radius of modifying, deleting, or renaming a symbol. Shows direct and transitive impacts with severity classification.",
	}, tools.WrapHandler[tools.AnalyzeImpactParams](analyzeImpact))

	sdkmcp.AddTool(sdkServer, &sdkmcp.Tool{
		Name:        "get_project_analytics",
		Description: "Get project-level analytics: summary stats, language distribution, symbol kind counts, architectural layer distribution, or cross-language bridges.",
	}, tools.WrapHandler[tools.GetProjectAnalyticsParams](getProjectAnalytics))

	sdkmcp.AddTool(sdkServer, &sdkmcp.Tool{
		Name:        "semantic_search",
		Description: "Search symbols using natural language via vector embeddings. Finds conceptually similar symbols even without exact name matches. Requires embedding provider to be configured.",
	}, tools.WrapHandler[tools.SemanticSearchParams](semanticSearch))

	sdkmcp.AddTool(sdkServer, &sdkmcp.Tool{
		Name:        "trace_cross_language",
		Description: "Trace cross-language paths from a symbol, showing how code flows across language boundaries (e.g., TypeScript → C# → SQL). Groups results by stack layer with confidence scores.",
	}, tools.WrapHandler[tools.TraceCrossLanguageParams](traceCrossLang))

	// Use Stateless mode so that stale session IDs from server restarts (hot-reload)
	// are ignored rather than returning 404. Each request gets a pre-initialized
	// temporary session. App-level sessions use Valkey via the session_id tool param.
	sdkHandler := sdkmcp.NewStreamableHTTPHandler(
		func(*http.Request) *sdkmcp.Server { return sdkServer },
		&sdkmcp.StreamableHTTPOptions{Stateless: true},
	)

	// HTTP mux for multiple endpoints
	mux := http.NewServeMux()

	// Wrap MCP handler with auth middleware
	var mcpHandler http.Handler = sdkHandler
	if cfg.Auth.Enabled {
		if cfg.Auth.IssuerURL == "" {
			logger.Error("AUTH_ENABLED=true but AUTH_ISSUER_URL is empty")
			os.Exit(1)
		}
		verifier, err := auth.NewVerifier(ctx, cfg.Auth.IssuerURL, cfg.Auth.PublicIssuer, cfg.Auth.Audience)
		if err != nil {
			logger.Error("failed to init OIDC verifier for MCP", slog.String("error", err.Error()))
			os.Exit(1)
		}

		// SDK auth middleware with RFC 9728 support
		resourceMetadataURL := ""
		if cfg.MCP.BaseURL != "" {
			resourceMetadataURL = cfg.MCP.BaseURL + "/.well-known/oauth-protected-resource"

			// Determine the Keycloak authorization server URL
			authServerURL := cfg.Auth.PublicIssuer
			if authServerURL == "" {
				authServerURL = cfg.Auth.IssuerURL
			}

			// Serve RFC 9728 Protected Resource Metadata
			prm := &oauthex.ProtectedResourceMetadata{
				Resource:             cfg.MCP.BaseURL,
				AuthorizationServers: []string{authServerURL},
				ScopesSupported:      []string{"openid", "lattice:read", "lattice:write"},
				BearerMethodsSupported: []string{"header"},
				ResourceName:         "Lattice MCP Server",
			}
			mux.Handle("/.well-known/oauth-protected-resource", sdkauth.ProtectedResourceMetadataHandler(prm))
			logger.Info("RFC 9728 metadata endpoint enabled", slog.String("url", resourceMetadataURL))
		}

		mcpVerifier := auth.NewMCPTokenVerifier(verifier)
		mcpHandler = sdkauth.RequireBearerToken(mcpVerifier, &sdkauth.RequireBearerTokenOptions{
			ResourceMetadataURL: resourceMetadataURL,
		})(sdkHandler)
		logger.Info("MCP OIDC auth enabled", slog.String("issuer", cfg.Auth.IssuerURL))
	} else {
		mcpHandler = auth.DevModeMiddleware(logger)(sdkHandler)
	}

	mux.Handle("/mcp", mcpHandler)
	// Also serve on root for backwards compat
	mux.Handle("/", mcpHandler)

	httpServer := &http.Server{Addr: cfg.MCP.Addr, Handler: mux}

	go func() {
		logger.Info("MCP server listening", slog.String("addr", cfg.MCP.Addr))
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("MCP HTTP server error", slog.String("error", err.Error()))
		}
	}()

	<-ctx.Done()
	logger.Info("MCP server shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Warn("MCP HTTP shutdown", slog.String("error", err.Error()))
	}
	logger.Info("MCP server stopped")
}
