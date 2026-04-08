package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/maraichr/lattice/internal/analytics"
	"github.com/maraichr/lattice/internal/config"
	"github.com/maraichr/lattice/internal/embedding"
	"github.com/maraichr/lattice/internal/graph"
	"github.com/maraichr/lattice/internal/ingestion"
	"github.com/maraichr/lattice/internal/ingestion/connectors"
	"github.com/maraichr/lattice/internal/lineage"
	"github.com/maraichr/lattice/internal/parser"
	csharpp "github.com/maraichr/lattice/internal/parser/csharp"
	"github.com/maraichr/lattice/internal/parser/asp"
	"github.com/maraichr/lattice/internal/parser/delphi"
	javap "github.com/maraichr/lattice/internal/parser/java"
	jsts "github.com/maraichr/lattice/internal/parser/javascript"
	mysqlp "github.com/maraichr/lattice/internal/parser/mysql"
	phpp "github.com/maraichr/lattice/internal/parser/php"
	"github.com/maraichr/lattice/internal/parser/pgsql"
	"github.com/maraichr/lattice/internal/parser/tsql"
	"github.com/maraichr/lattice/internal/resolver"
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

	// Valkey
	vkClient, err := vk.NewClient(cfg.Valkey)
	if err != nil {
		logger.Error("failed to connect to valkey", slog.String("error", err.Error()))
		os.Exit(1)
	}
	defer vkClient.Close()
	logger.Info("connected to valkey")

	// MinIO
	minioClient, err := minioclient.NewClient(cfg.MinIO)
	if err != nil {
		logger.Error("failed to connect to minio", slog.String("error", err.Error()))
		os.Exit(1)
	}
	logger.Info("connected to minio")

	// Neo4j
	graphClient, err := graph.NewClient(cfg.Neo4j)
	if err != nil {
		logger.Error("failed to connect to neo4j", slog.String("error", err.Error()))
		os.Exit(1)
	}
	defer graphClient.Close(ctx)
	if err := graphClient.EnsureIndexes(ctx); err != nil {
		logger.Warn("neo4j ensure indexes failed, sync may be slow", slog.String("error", err.Error()))
	}
	logger.Info("connected to neo4j")

	// Connectors
	zipConn := connectors.NewZipConnector(minioClient)
	gitConn := connectors.NewGitLabConnector()

	// S3 connector (optional)
	var s3Conn *connectors.S3Connector
	if cfg.S3.Bucket != "" {
		s3Conn, err = connectors.NewS3Connector(cfg.S3)
		if err != nil {
			logger.Warn("s3 connector init failed", slog.String("error", err.Error()))
		} else {
			logger.Info("s3 connector enabled", slog.String("bucket", cfg.S3.Bucket))
		}
	}

	// Parser registry
	registry := parser.NewRegistry()
	sqlRouter := parser.NewSQLRouter(tsql.New(), pgsql.New(), mysqlp.New())
	registry.Register(".sql", sqlRouter)
	registry.Register(".sqldataprovider", sqlRouter)
	aspParser := asp.New()
	registry.Register(".asp", aspParser)
	registry.Register(".aspx", aspParser)
	registry.Register(".ascx", aspParser)
	registry.Register(".ashx", aspParser)
	registry.Register(".master", aspParser)
	delphiParser := delphi.New()
	registry.Register(".pas", delphiParser)
	registry.Register(".dfm", delphiParser)
	registry.Register(".dpr", delphiParser)
	registry.Register(".java", javap.New())
	registry.Register(".cs", csharpp.New())
	jsParser := jsts.NewJS()
	registry.Register(".js", jsParser)
	registry.Register(".jsx", jsParser)
	registry.Register(".mjs", jsParser)
	tsParser := jsts.NewTS()
	registry.Register(".ts", tsParser)
	registry.Register(".tsx", tsParser)
	phpParser := phpp.New()
	registry.Register(".php", phpParser)
	registry.Register(".phtml", phpParser)

	// Embeddings (auto-selects: OpenRouter > Bedrock > disabled)
	var embedStage ingestion.Stage
	embedder, err := embedding.NewEmbedder(cfg)
	if err != nil {
		logger.Warn("embedder init failed, embedding stage disabled", slog.String("error", err.Error()))
		embedStage = ingestion.NewNoOpStage("embed")
	} else if embedder != nil {
		embedStage = ingestion.NewEmbedStage(embedder, s, logger)
		logger.Info("embeddings enabled", slog.String("provider", fmt.Sprintf("%T", embedder)), slog.String("model", embedder.ModelID()))
	} else {
		embedStage = ingestion.NewNoOpStage("embed")
	}

	// Resolver engine
	resolverEngine := resolver.NewEngine(s, logger)

	// Lineage engine
	lineageEngine := lineage.NewEngine(s, graphClient, logger)

	// Analytics engine (degree, PageRank, layers, summaries, bridges)
	analyticsEngine := analytics.NewEngine(s, logger)

	// Pipeline stages
	// ParseStage now only chunks and enqueues files; parse workers do the actual parsing.
	stages := []ingestion.Stage{
		ingestion.NewCloneStage(s, zipConn, gitConn, s3Conn),
		ingestion.NewParseStage(s, vkClient),
		// ResolveStage, Lineage, Graph, Embed, Analytics run after all parse chunks complete.
		// The orchestrator (pipeline.go) gates these stages on TotalChunks == 0, meaning
		// the parse_complete resume message drives the post-parse stages.
		ingestion.NewResolveStage(resolverEngine),
		ingestion.NewLineageStage(lineageEngine, s, logger),
		ingestion.NewGraphStage(s, graphClient, logger),
		embedStage,
		ingestion.NewAnalyticsStage(analyticsEngine, logger),
	}

	pipeline := ingestion.NewPipeline(s, stages, logger)

	// Main ingest consumer (orchestrator).
	consumer := ingestion.NewConsumer(vkClient, "worker-1", logger)
	if err := consumer.EnsureGroup(ctx); err != nil {
		logger.Error("failed to ensure consumer group", slog.String("error", err.Error()))
		os.Exit(1)
	}

	// Parse task consumer — processes individual file chunks in parallel with the orchestrator.
	parseWorker := ingestion.NewParseWorker(registry, s, vkClient, logger)
	parseTaskConsumer := ingestion.NewParseTaskConsumer(vkClient, "parse-worker-1", logger)
	if err := parseTaskConsumer.EnsureParseGroup(ctx); err != nil {
		logger.Error("failed to ensure parse task consumer group", slog.String("error", err.Error()))
		os.Exit(1)
	}

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		logger.Info("starting parse worker, consuming from stream", slog.String("stream", ingestion.ParseTaskStream))
		if err := parseTaskConsumer.Consume(ctx, parseWorker.Handle); err != nil {
			if ctx.Err() == nil {
				logger.Error("parse task consumer error", slog.String("error", err.Error()))
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		logger.Info("starting orchestrator worker, consuming from stream", slog.String("stream", ingestion.StreamName))
		if err := consumer.Consume(ctx, pipeline.Run); err != nil {
			if ctx.Err() == nil {
				logger.Error("consumer error", slog.String("error", err.Error()))
			}
		}
	}()

	wg.Wait()
	logger.Info("worker stopped")
}
