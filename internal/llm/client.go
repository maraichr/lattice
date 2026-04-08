package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	defaultBaseURL     = "https://openrouter.ai/api/v1/chat/completions"
	defaultModel       = "minimax/minimax-m1"
	maxRetries         = 3
	retryDelay         = 2 * time.Second
	defaultMaxTokens   = 4096
	defaultTemperature = 0.0
	defaultHTTPTimeout = 120 * time.Second
)

// Client is a lightweight OpenAI-compatible chat completions client.
type Client struct {
	apiKey  string
	model   string
	baseURL string
	http    *http.Client
}

// Message represents a chat message.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ToolDef describes a tool available to the LLM (OpenAI function calling format).
type ToolDef struct {
	Type     string       `json:"type"` // "function"
	Function ToolFunction `json:"function"`
}

// ToolFunction describes a function tool.
type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// ToolCall represents a tool call from the LLM response.
type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"` // "function"
	Function ToolCallFunction `json:"function"`
}

// ToolCallFunction contains the function name and arguments.
type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type chatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	MaxTokens   int       `json:"max_tokens"`
	Temperature float64   `json:"temperature"`
}

type chatRequestWithTools struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	MaxTokens   int       `json:"max_tokens"`
	Temperature float64   `json:"temperature"`
	Tools       []ToolDef `json:"tools"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content   string     `json:"content"`
			ToolCalls []ToolCall `json:"tool_calls,omitempty"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// NewClient creates a new LLM chat client.
func NewClient(apiKey, model, baseURL string) *Client {
	if model == "" {
		model = defaultModel
	}
	if baseURL == "" {
		baseURL = defaultBaseURL
	} else {
		baseURL = strings.TrimRight(baseURL, "/")
		if !strings.HasSuffix(baseURL, "/chat/completions") {
			baseURL += "/chat/completions"
		}
	}
	return &Client{
		apiKey:  apiKey,
		model:   model,
		baseURL: baseURL,
		http:    &http.Client{Timeout: defaultHTTPTimeout},
	}
}

// Complete sends messages to the LLM and returns the response content.
func (c *Client) Complete(ctx context.Context, messages []Message) (string, error) {
	payload := chatRequest{
		Model:       c.model,
		Messages:    messages,
		MaxTokens:   defaultMaxTokens,
		Temperature: defaultTemperature,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(retryDelay * time.Duration(attempt)):
			}
		}

		result, err := c.doRequest(ctx, body)
		if err == nil {
			return result.Content, nil
		}
		lastErr = err
		errStr := err.Error()
		if !strings.Contains(errStr, "status 429") &&
			!strings.Contains(errStr, "status 529") &&
			!strings.Contains(errStr, "status 503") {
			return "", err
		}
	}
	return "", fmt.Errorf("after %d retries: %w", maxRetries, lastErr)
}

// CompleteWithTools sends messages with tool definitions and returns the full
// response message, which may contain Content (text) or ToolCalls.
func (c *Client) CompleteWithTools(ctx context.Context, messages []Message, tools []ToolDef) (*Message, error) {
	payload := chatRequestWithTools{
		Model:       c.model,
		Messages:    messages,
		MaxTokens:   defaultMaxTokens,
		Temperature: defaultTemperature,
		Tools:       tools,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(retryDelay * time.Duration(attempt)):
			}
		}

		msg, err := c.doRequest(ctx, body)
		if err == nil {
			return msg, nil
		}
		lastErr = err
		errStr := err.Error()
		if !strings.Contains(errStr, "status 429") &&
			!strings.Contains(errStr, "status 529") &&
			!strings.Contains(errStr, "status 503") {
			return nil, err
		}
	}
	return nil, fmt.Errorf("after %d retries: %w", maxRetries, lastErr)
}

func (c *Client) doRequest(ctx context.Context, body []byte) (*Message, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("LLM API error (status %d): %s", resp.StatusCode, string(respBody))
	}

	var result chatResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}

	if result.Error != nil {
		return nil, fmt.Errorf("LLM error: %s", result.Error.Message)
	}

	if len(result.Choices) == 0 {
		return nil, fmt.Errorf("LLM returned no choices")
	}

	choice := result.Choices[0].Message
	msg := &Message{
		Role:      "assistant",
		Content:   strings.TrimSpace(choice.Content),
		ToolCalls: choice.ToolCalls,
	}
	return msg, nil
}

// Model returns the model identifier.
func (c *Client) Model() string {
	return c.model
}
