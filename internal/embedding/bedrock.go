package embedding

import (
	"context"
	"encoding/json"
	"fmt"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"golang.org/x/sync/errgroup"

	"github.com/maraichr/lattice/internal/config"
)

const (
	maxBatchSize      = 96 // Cohere embed API limit
	bedrockConcurrency = 8  // max simultaneous in-flight Bedrock requests
)

// Client wraps the AWS Bedrock runtime for embedding generation.
type Client struct {
	bedrock *bedrockruntime.Client
	modelID string
}

// NewClient creates a new Bedrock embedding client.
func NewClient(cfg config.BedrockConfig) (*Client, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion(cfg.Region),
	)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}

	client := bedrockruntime.NewFromConfig(awsCfg)
	return &Client{bedrock: client, modelID: cfg.ModelID}, nil
}

// cohereEmbedRequest is the Cohere Embed v4 API request format.
type cohereEmbedRequest struct {
	Texts     []string `json:"texts"`
	InputType string   `json:"input_type"`
}

// cohereEmbedResponse is the Cohere Embed v4 API response format.
type cohereEmbedResponse struct {
	Embeddings [][]float32 `json:"embeddings"`
}

// EmbedBatch generates embeddings for a batch of texts via AWS Bedrock.
//
// Texts are split into sub-batches of maxBatchSize and up to bedrockConcurrency
// requests are sent in parallel using errgroup. Each chunk writes into a
// pre-allocated slot in the result slice.
func (c *Client) EmbedBatch(ctx context.Context, texts []string, inputType string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	type chunk struct {
		start int
		end   int
	}
	var chunks []chunk
	for i := 0; i < len(texts); i += maxBatchSize {
		chunks = append(chunks, chunk{i, min(i+maxBatchSize, len(texts))})
	}

	// Pre-allocate one slot per chunk; each goroutine owns its own slot.
	chunkResults := make([][][]float32, len(chunks))

	eg, egCtx := errgroup.WithContext(ctx)
	eg.SetLimit(bedrockConcurrency)

	for idx, ch := range chunks {
		idx, ch := idx, ch // capture loop vars
		eg.Go(func() error {
			embeddings, err := c.embedSingle(egCtx, texts[ch.start:ch.end], inputType)
			if err != nil {
				return fmt.Errorf("chunk %d: %w", idx, err)
			}
			chunkResults[idx] = embeddings
			return nil
		})
	}

	if err := eg.Wait(); err != nil {
		return nil, err
	}

	allEmbeddings := make([][]float32, 0, len(texts))
	for _, r := range chunkResults {
		allEmbeddings = append(allEmbeddings, r...)
	}
	return allEmbeddings, nil
}

func (c *Client) embedSingle(ctx context.Context, texts []string, inputType string) ([][]float32, error) {
	reqBody, err := json.Marshal(cohereEmbedRequest{
		Texts:     texts,
		InputType: inputType,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	resp, err := c.bedrock.InvokeModel(ctx, &bedrockruntime.InvokeModelInput{
		ModelId:     &c.modelID,
		ContentType: strPtr("application/json"),
		Body:        reqBody,
	})
	if err != nil {
		return nil, fmt.Errorf("invoke model: %w", err)
	}

	var result cohereEmbedResponse
	if err := json.Unmarshal(resp.Body, &result); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}

	return result.Embeddings, nil
}

// ModelID returns the Bedrock model identifier.
func (c *Client) ModelID() string { return c.modelID }

func strPtr(s string) *string { return &s }
