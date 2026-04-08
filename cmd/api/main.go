package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/maraichr/lattice/internal/api"
	"github.com/maraichr/lattice/internal/auth"
	"github.com/maraichr/lattice/internal/config"
	"github.com/maraichr/lattice/internal/embedding"
	"github.com/maraichr/lattice/internal/graph"
	"github.com/maraichr/lattice/internal/impact"
	"github.com/maraichr/lattice/internal/ingestion"
	"github.com/maraichr/lattice/internal/lineage"
	"github.com/maraichr/lattice/internal/llm"
	"github.com/maraichr/lattice/internal/mcp/session"
	"github.com/maraichr/lattice/internal/oracle"
	"github.com/maraichr/lattice/internal/store"
	minioclient "github.com/maraichr/lattice/internal/store/minio"
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

	// Initialize database pool
	ctx := context.Background()
	pool, err := postgres.NewPool(ctx, cfg.Database.DSN(), cfg.Database.MaxConns, cfg.Database.MinConns)
	if err != nil {
		logger.Error("failed to connect to database", slog.String("error", err.Error()))
		os.Exit(1)
	}
	defer pool.Close()
	logger.Info("connected to database")

	s := store.New(pool)

	deps := &api.RouterDeps{}

	// Neo4j (optional)
	graphClient, err := graph.NewClient(cfg.Neo4j)
	if err != nil {
		logger.Warn("neo4j connection failed, lineage queries disabled", slog.String("error", err.Error()))
	} else {
		if err := graphClient.EnsureIndexes(ctx); err != nil {
			logger.Warn("neo4j ensure indexes failed", slog.String("error", err.Error()))
		}
		deps.Graph = graphClient
		deps.Lineage = lineage.NewEngine(s, graphClient, logger)
		deps.Impact = impact.NewEngine(graphClient, s, logger)
		defer graphClient.Close(ctx)
		logger.Info("connected to neo4j")
	}

	// MinIO (optional — enables uploads)
	mc, err := minioclient.NewClient(cfg.MinIO)
	if err != nil {
		logger.Warn("minio connection failed, uploads disabled", slog.String("error", err.Error()))
	} else {
		deps.MinIO = mc
		logger.Info("connected to minio")
	}

	// Valkey (optional — enables job queue)
	vkClient, err := vk.NewClient(cfg.Valkey)
	if err != nil {
		logger.Warn("valkey connection failed, job queue disabled", slog.String("error", err.Error()))
	} else {
		deps.Producer = ingestion.NewProducer(vkClient)
		defer vkClient.Close()
		logger.Info("connected to valkey")
	}

	// Embeddings (auto-selects: OpenRouter > Bedrock > disabled)
	embedder, err := embedding.NewEmbedder(cfg)
	if err != nil {
		logger.Warn("embedder init failed, semantic search disabled", slog.String("error", err.Error()))
	} else if embedder != nil {
		deps.Embed = embedder
		logger.Info("embeddings enabled", slog.String("provider", fmt.Sprintf("%T", embedder)), slog.String("model", embedder.ModelID()))
	}

	// Auth (optional — requires AUTH_ENABLED=true + valid issuer URL)
	deps.AuthEnabled = cfg.Auth.Enabled
	if cfg.Auth.Enabled {
		if cfg.Auth.IssuerURL == "" {
			logger.Error("AUTH_ENABLED=true but AUTH_ISSUER_URL is empty")
			os.Exit(1)
		}
		verifier, err := auth.NewVerifier(ctx, cfg.Auth.IssuerURL, cfg.Auth.PublicIssuer, cfg.Auth.Audience)
		if err != nil {
			logger.Error("failed to init OIDC verifier", slog.String("error", err.Error()))
			os.Exit(1)
		}
		deps.Verifier = verifier
		logger.Info("OIDC auth enabled", slog.String("issuer", cfg.Auth.IssuerURL))
	}

	// Oracle (optional — requires ORACLE_ENABLED=true + OpenRouter API key + Valkey)
	if cfg.Oracle.Enabled && cfg.OpenRouter.APIKey != "" && vkClient != nil {
		llmClient := llm.NewClient(cfg.OpenRouter.APIKey, cfg.Oracle.Model, cfg.OpenRouter.BaseURL)
		sessionMgr := session.NewManager(vkClient)
		deps.Oracle = oracle.NewEngine(s, sessionMgr, llmClient, deps.Embed, logger)
		logger.Info("oracle enabled", slog.String("model", cfg.Oracle.Model))
	}

	router := api.NewRouter(logger, s, deps)

	srv := &http.Server{
		Addr:         fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port),
		Handler:      router,
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
	}

	// Graceful shutdown
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		logger.Info("starting API server", slog.String("addr", srv.Addr))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("server error", slog.String("error", err.Error()))
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down server")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("shutdown error", slog.String("error", err.Error()))
	}

	logger.Info("server stopped")
}
