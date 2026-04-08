package ingestion

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/valkey-io/valkey-go"
)

const (
	StreamName    = "lattice:ingest"
	GroupName     = "lattice-workers"
	MaxRetries    = 3
	ClaimTimeout  = 5 * time.Minute

	// ParseTaskStream is the Valkey stream used to distribute parse chunks to workers.
	ParseTaskStream      = "lattice:parse_tasks"
	ParseTaskGroupName   = "lattice-parse-workers"
	ParseTaskChunkSize   = 500
)

// ParseTaskMessage is a single chunk of files to be parsed by a distributed parse worker.
type ParseTaskMessage struct {
	IndexRunID  uuid.UUID `json:"index_run_id"`
	ProjectID   uuid.UUID `json:"project_id"`
	SourceID    uuid.UUID `json:"source_id"`
	SourceType  string    `json:"source_type"`
	WorkDir     string    `json:"work_dir"`
	ChunkIndex  int       `json:"chunk_index"`
	TotalChunks int       `json:"total_chunks"`
	Files       []string  `json:"files"` // relative file paths for this chunk
	// Lineage settings forwarded from the project
	LineageExcludePaths []string `json:"lineage_exclude_paths,omitempty"`
}

// EnqueueParseTask publishes a single parse-chunk message to the parse task stream.
func EnqueueParseTask(ctx context.Context, client valkey.Client, msg ParseTaskMessage) (string, error) {
	data, err := json.Marshal(msg)
	if err != nil {
		return "", fmt.Errorf("marshal parse task: %w", err)
	}

	resp := client.Do(ctx, client.B().Xadd().
		Key(ParseTaskStream).Id("*").
		FieldValue().FieldValue("data", string(data)).
		Build())
	if err := resp.Error(); err != nil {
		return "", fmt.Errorf("xadd parse task: %w", err)
	}

	id, err := resp.ToString()
	if err != nil {
		return "", fmt.Errorf("parse xadd response: %w", err)
	}
	return id, nil
}

// ParseTaskConsumer reads parse-chunk jobs from the Valkey parse task stream.
type ParseTaskConsumer struct {
	client     valkey.Client
	consumerID string
	logger     *slog.Logger
}

func NewParseTaskConsumer(client valkey.Client, consumerID string, logger *slog.Logger) *ParseTaskConsumer {
	return &ParseTaskConsumer{client: client, consumerID: consumerID, logger: logger}
}

// EnsureParseGroup creates the parse task consumer group if it doesn't exist.
func (c *ParseTaskConsumer) EnsureParseGroup(ctx context.Context) error {
	resp := c.client.Do(ctx, c.client.B().XgroupCreate().
		Key(ParseTaskStream).Group(ParseTaskGroupName).Id("0").Mkstream().Build())
	if err := resp.Error(); err != nil {
		if err.Error() != "BUSYGROUP Consumer Group name already exists" {
			return fmt.Errorf("xgroup create parse: %w", err)
		}
	}
	return nil
}

// Consume blocks reading parse task chunks, processing each via handler, and ACKs.
func (c *ParseTaskConsumer) Consume(ctx context.Context, handler func(context.Context, ParseTaskMessage) error) error {
	c.drainPending(ctx, handler)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		resp := c.client.Do(ctx, c.client.B().Xreadgroup().
			Group(ParseTaskGroupName, c.consumerID).
			Count(1).Block(5000).
			Streams().Key(ParseTaskStream).Id(">").
			Build())

		if err := resp.Error(); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			continue
		}

		results, err := resp.AsXRead()
		if err != nil {
			continue
		}

		for _, messages := range results {
			for _, msg := range messages {
				c.processParseTask(ctx, msg, handler)
			}
		}
	}
}

func (c *ParseTaskConsumer) drainPending(ctx context.Context, handler func(context.Context, ParseTaskMessage) error) {
	resp := c.client.Do(ctx, c.client.B().Xreadgroup().
		Group(ParseTaskGroupName, c.consumerID).
		Count(10).
		Streams().Key(ParseTaskStream).Id("0").
		Build())

	if err := resp.Error(); err != nil {
		c.logger.Warn("drain parse pending failed", slog.String("error", err.Error()))
		return
	}

	results, err := resp.AsXRead()
	if err != nil {
		return
	}

	for _, messages := range results {
		for _, msg := range messages {
			c.logger.Info("recovering pending parse task", slog.String("id", msg.ID))
			c.processParseTask(ctx, msg, handler)
		}
	}
}

func (c *ParseTaskConsumer) processParseTask(ctx context.Context, msg valkey.XRangeEntry, handler func(context.Context, ParseTaskMessage) error) {
	dataStr, ok := msg.FieldValues["data"]
	if !ok {
		c.logger.Warn("parse task missing data field", slog.String("id", msg.ID))
		c.ackParseTask(ctx, msg.ID)
		return
	}

	var task ParseTaskMessage
	if err := json.Unmarshal([]byte(dataStr), &task); err != nil {
		c.logger.Error("unmarshal parse task", slog.String("error", err.Error()), slog.String("id", msg.ID))
		c.ackParseTask(ctx, msg.ID)
		return
	}

	if err := handler(ctx, task); err != nil {
		c.logger.Error("handle parse task", slog.String("error", err.Error()),
			slog.String("id", msg.ID),
			slog.String("index_run_id", task.IndexRunID.String()),
			slog.Int("chunk_index", task.ChunkIndex))
	} else {
		c.ackParseTask(ctx, msg.ID)
	}
}

func (c *ParseTaskConsumer) ackParseTask(ctx context.Context, msgID string) {
	resp := c.client.Do(ctx, c.client.B().Xack().
		Key(ParseTaskStream).Group(ParseTaskGroupName).Id(msgID).Build())
	if err := resp.Error(); err != nil {
		c.logger.Error("xack parse task failed", slog.String("error", err.Error()), slog.String("id", msgID))
	}
}

// IngestMessage is the payload enqueued for worker processing.
type IngestMessage struct {
	IndexRunID uuid.UUID `json:"index_run_id"`
	ProjectID  uuid.UUID `json:"project_id"`
	SourceID   uuid.UUID `json:"source_id"`
	SourceType string    `json:"source_type"`
	Trigger    string    `json:"trigger"` // "manual", "webhook", "schedule"
}

// Producer enqueues ingestion jobs to the Valkey stream.
type Producer struct {
	client valkey.Client
}

func NewProducer(client valkey.Client) *Producer {
	return &Producer{client: client}
}

func (p *Producer) Enqueue(ctx context.Context, msg IngestMessage) (string, error) {
	data, err := json.Marshal(msg)
	if err != nil {
		return "", fmt.Errorf("marshal message: %w", err)
	}

	resp := p.client.Do(ctx, p.client.B().Xadd().
		Key(StreamName).Id("*").
		FieldValue().FieldValue("data", string(data)).
		Build())
	if err := resp.Error(); err != nil {
		return "", fmt.Errorf("xadd: %w", err)
	}

	id, err := resp.ToString()
	if err != nil {
		return "", fmt.Errorf("parse xadd response: %w", err)
	}
	return id, nil
}

// Consumer reads ingestion jobs from the Valkey stream.
type Consumer struct {
	client     valkey.Client
	consumerID string
	logger     *slog.Logger
}

func NewConsumer(client valkey.Client, consumerID string, logger *slog.Logger) *Consumer {
	return &Consumer{client: client, consumerID: consumerID, logger: logger}
}

// EnsureGroup creates the consumer group if it doesn't exist.
func (c *Consumer) EnsureGroup(ctx context.Context) error {
	resp := c.client.Do(ctx, c.client.B().XgroupCreate().
		Key(StreamName).Group(GroupName).Id("0").Mkstream().Build())
	if err := resp.Error(); err != nil {
		// BUSYGROUP means group already exists — that's fine
		if err.Error() != "BUSYGROUP Consumer Group name already exists" {
			return fmt.Errorf("xgroup create: %w", err)
		}
	}
	return nil
}

// Consume blocks until a message is available, processes it via handler, and ACKs.
// On startup, it first drains any pending messages from a previous crash.
func (c *Consumer) Consume(ctx context.Context, handler func(context.Context, IngestMessage) error) error {
	// First, drain pending messages from previous runs (Id "0" returns pending)
	c.drainPending(ctx, handler)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		resp := c.client.Do(ctx, c.client.B().Xreadgroup().
			Group(GroupName, c.consumerID).
			Count(1).Block(5000).
			Streams().Key(StreamName).Id(">").
			Build())

		if err := resp.Error(); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// Timeout is normal for BLOCK reads
			continue
		}

		results, err := resp.AsXRead()
		if err != nil {
			continue
		}

		for _, messages := range results {
			for _, msg := range messages {
				c.processMessage(ctx, msg, handler)
			}
		}
	}
}

// drainPending reads messages previously delivered to this consumer but not ACKed.
func (c *Consumer) drainPending(ctx context.Context, handler func(context.Context, IngestMessage) error) {
	// XREADGROUP with Id "0" returns pending messages for this consumer
	resp := c.client.Do(ctx, c.client.B().Xreadgroup().
		Group(GroupName, c.consumerID).
		Count(10).
		Streams().Key(StreamName).Id("0").
		Build())

	if err := resp.Error(); err != nil {
		c.logger.Warn("drain pending failed", slog.String("error", err.Error()))
		return
	}

	results, err := resp.AsXRead()
	if err != nil {
		return
	}

	for _, messages := range results {
		for _, msg := range messages {
			c.logger.Info("recovering pending message", slog.String("id", msg.ID))
			c.processMessage(ctx, msg, handler)
		}
	}
}

func (c *Consumer) processMessage(ctx context.Context, msg valkey.XRangeEntry, handler func(context.Context, IngestMessage) error) {
	dataStr, ok := msg.FieldValues["data"]
	if !ok {
		c.logger.Warn("message missing data field", slog.String("id", msg.ID))
		c.ack(ctx, msg.ID)
		return
	}

	var ingestMsg IngestMessage
	if err := json.Unmarshal([]byte(dataStr), &ingestMsg); err != nil {
		c.logger.Error("unmarshal message", slog.String("error", err.Error()), slog.String("id", msg.ID))
		c.ack(ctx, msg.ID)
		return
	}

	if err := handler(ctx, ingestMsg); err != nil {
		c.logger.Error("handle message", slog.String("error", err.Error()),
			slog.String("id", msg.ID),
			slog.String("index_run_id", ingestMsg.IndexRunID.String()))
	} else {
		c.ack(ctx, msg.ID)
	}
}

func (c *Consumer) ack(ctx context.Context, msgID string) {
	resp := c.client.Do(ctx, c.client.B().Xack().
		Key(StreamName).Group(GroupName).Id(msgID).Build())
	if err := resp.Error(); err != nil {
		c.logger.Error("xack failed", slog.String("error", err.Error()), slog.String("id", msgID))
	}
}
