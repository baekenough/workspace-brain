// Package ai contains optional LLM provider seams that stay decoupled from the core.
package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/sangyi/workspace-brain/pkg/brainapi"
)

const (
	defaultOpenAIBaseURL = "https://api.openai.com/v1"
	defaultHTTPTimeout   = 30 * time.Second
)

// Embedder converts text inputs into vectors.
type Embedder interface {
	Embed(ctx context.Context, inputs []string) ([][]float64, error)
}

// Responder asks a model for text output.
type Responder interface {
	Respond(ctx context.Context, input string) (string, error)
}

// LocalClient is a deterministic no-network provider for development and tests.
type LocalClient struct{}

// Embed returns deterministic bag-of-character vectors for each input.
func (LocalClient) Embed(_ context.Context, inputs []string) ([][]float64, error) {
	vectors := make([][]float64, len(inputs))
	for i, input := range inputs {
		vec := make([]float64, 16)
		for _, r := range strings.ToLower(input) {
			vec[int(r)%len(vec)]++
		}
		vectors[i] = vec
	}
	return vectors, nil
}

// Respond returns a deterministic local response without requiring API credentials.
func (LocalClient) Respond(_ context.Context, input string) (string, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", brainapi.E(brainapi.KindInvalid, "local_respond", "input is required", nil)
	}
	return "local: " + input, nil
}

// OpenAIConfig configures an OpenAI-compatible HTTP endpoint.
type OpenAIConfig struct {
	BaseURL        string
	APIKey         string
	EmbeddingModel string
	ResponseModel  string
	OrganizationID string
	ProjectID      string
	HTTPClient     *http.Client
}

// OpenAIClient is an optional standard-library adapter for OpenAI-compatible APIs.
type OpenAIClient struct {
	baseURL        string
	apiKey         string
	embeddingModel string
	responseModel  string
	organizationID string
	projectID      string
	httpClient     *http.Client
}

// OpenAIConfigFromEnv reads conventional OpenAI-compatible environment variables.
func OpenAIConfigFromEnv() OpenAIConfig {
	return OpenAIConfig{
		BaseURL:        os.Getenv("OPENAI_BASE_URL"),
		APIKey:         os.Getenv("OPENAI_API_KEY"),
		EmbeddingModel: os.Getenv("OPENAI_EMBEDDING_MODEL"),
		ResponseModel:  os.Getenv("OPENAI_RESPONSE_MODEL"),
		OrganizationID: os.Getenv("OPENAI_ORG_ID"),
		ProjectID:      os.Getenv("OPENAI_PROJECT_ID"),
	}
}

// NewOpenAIClient creates a client without making the data core depend on it.
func NewOpenAIClient(cfg OpenAIConfig) (*OpenAIClient, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if baseURL == "" {
		baseURL = defaultOpenAIBaseURL
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: defaultHTTPTimeout}
	}
	return &OpenAIClient{
		baseURL:        baseURL,
		apiKey:         strings.TrimSpace(cfg.APIKey),
		embeddingModel: strings.TrimSpace(cfg.EmbeddingModel),
		responseModel:  strings.TrimSpace(cfg.ResponseModel),
		organizationID: strings.TrimSpace(cfg.OrganizationID),
		projectID:      strings.TrimSpace(cfg.ProjectID),
		httpClient:     client,
	}, nil
}

// Embed calls /embeddings on an OpenAI-compatible endpoint.
func (c *OpenAIClient) Embed(ctx context.Context, inputs []string) ([][]float64, error) {
	if c == nil {
		return nil, brainapi.E(brainapi.KindInvalid, "openai_embed", "client is required", nil)
	}
	if c.embeddingModel == "" {
		return nil, brainapi.E(brainapi.KindInvalid, "openai_embed", "embedding model is required", nil)
	}
	var resp struct {
		Data []struct {
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/embeddings", map[string]any{"model": c.embeddingModel, "input": inputs}, &resp); err != nil {
		return nil, err
	}
	vectors := make([][]float64, len(resp.Data))
	for i := range resp.Data {
		vectors[i] = resp.Data[i].Embedding
	}
	return vectors, nil
}

// Respond calls /responses on an OpenAI-compatible endpoint and extracts text output.
func (c *OpenAIClient) Respond(ctx context.Context, input string) (string, error) {
	if c == nil {
		return "", brainapi.E(brainapi.KindInvalid, "openai_respond", "client is required", nil)
	}
	if c.responseModel == "" {
		return "", brainapi.E(brainapi.KindInvalid, "openai_respond", "response model is required", nil)
	}
	var resp struct {
		OutputText string `json:"output_text"`
		Output     []struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/responses", map[string]any{"model": c.responseModel, "input": input}, &resp); err != nil {
		return "", err
	}
	if strings.TrimSpace(resp.OutputText) != "" {
		return resp.OutputText, nil
	}
	for _, out := range resp.Output {
		for _, content := range out.Content {
			if content.Type == "output_text" || content.Type == "text" {
				if strings.TrimSpace(content.Text) != "" {
					return content.Text, nil
				}
			}
		}
	}
	return "", brainapi.E(brainapi.KindInternal, "openai_respond", "response did not include text", nil)
}

func (c *OpenAIClient) doJSON(ctx context.Context, method, endpoint string, body any, out any) error {
	const op = "openai_http"
	if c.apiKey == "" {
		return brainapi.E(brainapi.KindInvalid, op, "api key is required", nil)
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return brainapi.E(brainapi.KindInvalid, op, "request payload is invalid", err)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+endpoint, bytes.NewReader(payload))
	if err != nil {
		return brainapi.E(brainapi.KindInvalid, op, "request is invalid", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	if c.organizationID != "" {
		req.Header.Set("OpenAI-Organization", c.organizationID)
	}
	if c.projectID != "" {
		req.Header.Set("OpenAI-Project", c.projectID)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return brainapi.E(brainapi.KindInternal, op, "request failed", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return brainapi.E(brainapi.KindInternal, op, "response read failed", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return brainapi.E(brainapi.KindInternal, op, fmt.Sprintf("provider returned status %d", resp.StatusCode), nil)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return brainapi.E(brainapi.KindInternal, op, "response json is invalid", err)
	}
	return nil
}

var _ Embedder = (*OpenAIClient)(nil)
var _ Responder = (*OpenAIClient)(nil)
var _ Embedder = LocalClient{}
var _ Responder = LocalClient{}
