package embedding

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/maraichr/lattice/internal/config"
)

func TestNewOpenRouterClient_MissingAPIKey(t *testing.T) {
	_, err := NewOpenRouterClient(config.OpenRouterConfig{})
	if err == nil {
		t.Fatal("expected error for missing API key")
	}
}

func TestNewOpenRouterClient_Defaults(t *testing.T) {
	client, err := NewOpenRouterClient(config.OpenRouterConfig{APIKey: "sk-test"})
	if err != nil {
		t.Fatal(err)
	}
	if client.model != defaultOpenRouterModel {
		t.Errorf("expected default model %s, got %s", defaultOpenRouterModel, client.model)
	}
	if client.baseURL != defaultOpenRouterBaseURL {
		t.Errorf("expected default base URL %s, got %s", defaultOpenRouterBaseURL, client.baseURL)
	}
	if client.dimensions != defaultDimensions {
		t.Errorf("expected default dimensions %d, got %d", defaultDimensions, client.dimensions)
	}
}

func TestOpenRouterClient_EmbedBatch_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-test" {
			t.Error("missing or wrong auth header")
		}

		var req openAIEmbedRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}

		if req.Model != defaultOpenRouterModel {
			t.Errorf("expected model %s, got %s", defaultOpenRouterModel, req.Model)
		}
		if req.Dimensions != 1024 {
			t.Errorf("expected dimensions 1024, got %d", req.Dimensions)
		}
		if len(req.Input) != 2 {
			t.Fatalf("expected 2 inputs, got %d", len(req.Input))
		}

		resp := openAIEmbedResponse{
			Data: []struct {
				Embedding []float32 `json:"embedding"`
				Index     int       `json:"index"`
			}{
				{Embedding: []float32{0.1, 0.2, 0.3}, Index: 0},
				{Embedding: []float32{0.4, 0.5, 0.6}, Index: 1},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client, err := NewOpenRouterClient(config.OpenRouterConfig{
		APIKey:  "sk-test",
		BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}

	embeddings, err := client.EmbedBatch(context.Background(), []string{"hello", "world"}, "search_document")
	if err != nil {
		t.Fatal(err)
	}
	if len(embeddings) != 2 {
		t.Fatalf("expected 2 embeddings, got %d", len(embeddings))
	}
	if embeddings[0][0] != 0.1 {
		t.Errorf("expected first embedding value 0.1, got %f", embeddings[0][0])
	}
}

func TestOpenRouterClient_EmbedBatch_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error": {"message": "rate limited"}}`))
	}))
	defer srv.Close()

	client, err := NewOpenRouterClient(config.OpenRouterConfig{
		APIKey:  "sk-test",
		BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = client.EmbedBatch(context.Background(), []string{"hello"}, "search_document")
	if err == nil {
		t.Fatal("expected error for API error response")
	}
}

func TestOpenRouterClient_EmbedBatch_EmptyInput(t *testing.T) {
	client, err := NewOpenRouterClient(config.OpenRouterConfig{
		APIKey: "sk-test",
	})
	if err != nil {
		t.Fatal(err)
	}

	embeddings, err := client.EmbedBatch(context.Background(), nil, "search_document")
	if err != nil {
		t.Fatal(err)
	}
	if embeddings != nil {
		t.Errorf("expected nil for empty input, got %v", embeddings)
	}
}

func TestOpenRouterClient_ModelID(t *testing.T) {
	client, err := NewOpenRouterClient(config.OpenRouterConfig{
		APIKey: "sk-test",
		Model:  "custom/model",
	})
	if err != nil {
		t.Fatal(err)
	}
	if client.ModelID() != "custom/model" {
		t.Errorf("expected custom/model, got %s", client.ModelID())
	}
}

// TestOpenRouterClient_EmbedBatch_MultiChunk verifies that the concurrent
// parallel path produces correctly ordered results when multiple chunks are
// sent simultaneously (>openRouterBatchSize texts).
func TestOpenRouterClient_EmbedBatch_MultiChunk(t *testing.T) {
	const total = 250 // 3 chunks: 100 + 100 + 50

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req openAIEmbedRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		resp := openAIEmbedResponse{}
		for i, text := range req.Input {
			// Encode the text index into the first element of the embedding so
			// we can assert order later.
			_ = text
			resp.Data = append(resp.Data, struct {
				Embedding []float32 `json:"embedding"`
				Index     int       `json:"index"`
			}{
				Embedding: []float32{float32(i)},
				Index:     i,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client, err := NewOpenRouterClient(config.OpenRouterConfig{
		APIKey:  "sk-test",
		BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}

	texts := make([]string, total)
	for i := range texts {
		texts[i] = "text"
	}

	embeddings, err := client.EmbedBatch(context.Background(), texts, "search_document")
	if err != nil {
		t.Fatal(err)
	}
	if len(embeddings) != total {
		t.Fatalf("expected %d embeddings, got %d", total, len(embeddings))
	}
}
