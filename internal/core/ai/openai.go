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
	defaultMaxRetries    = 3
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
	BaseURL             string
	APIKey              string
	EmbeddingModel      string
	ResponseModel       string
	OrganizationID      string
	ProjectID           string
	EmbeddingDimensions int // when > 0, vectors are MRL-truncated and validated to this size
	HTTPClient          *http.Client
	// MaxRetries is the number of retries for 429/5xx responses.
	// Zero or negative uses the default of 3.
	MaxRetries int
	// Sleep is the backoff function called between retries.
	// Nil uses the default time-based implementation.
	Sleep func(ctx context.Context, d time.Duration) error
}

// OpenAIClient is an optional standard-library adapter for OpenAI-compatible APIs.
type OpenAIClient struct {
	baseURL             string
	apiKey              string
	embeddingModel      string
	responseModel       string
	organizationID      string
	projectID           string
	embeddingDimensions int
	httpClient          *http.Client
	maxRetries          int
	sleep               func(ctx context.Context, d time.Duration) error
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
	maxRetries := cfg.MaxRetries
	if maxRetries <= 0 {
		maxRetries = defaultMaxRetries
	}
	slp := cfg.Sleep
	if slp == nil {
		slp = defaultSleep
	}
	return &OpenAIClient{
		baseURL:             baseURL,
		apiKey:              strings.TrimSpace(cfg.APIKey),
		embeddingModel:      strings.TrimSpace(cfg.EmbeddingModel),
		responseModel:       strings.TrimSpace(cfg.ResponseModel),
		organizationID:      strings.TrimSpace(cfg.OrganizationID),
		projectID:           strings.TrimSpace(cfg.ProjectID),
		embeddingDimensions: cfg.EmbeddingDimensions,
		httpClient:          client,
		maxRetries:          maxRetries,
		sleep:               slp,
	}, nil
}

// defaultSleep blocks for d or until ctx is done.
func defaultSleep(ctx context.Context, d time.Duration) error {
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Embed calls /embeddings on an OpenAI-compatible endpoint.
// When EmbeddingDimensions > 0, each vector is validated: vectors longer than
// the configured dimension are MRL-truncated; shorter vectors return a typed error.
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
		vec := resp.Data[i].Embedding
		if c.embeddingDimensions > 0 {
			if len(vec) < c.embeddingDimensions {
				return nil, brainapi.E(brainapi.KindInternal, "openai_embed",
					fmt.Sprintf("embedding dimension mismatch: got %d, want %d", len(vec), c.embeddingDimensions), nil)
			}
			if len(vec) > c.embeddingDimensions {
				vec = vec[:c.embeddingDimensions]
			}
		}
		vectors[i] = vec
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

// doJSON marshals body, POSTs/PATCHes/etc to endpoint, and decodes the response
// into out. It retries 429 and 5xx responses up to c.maxRetries times with
// exponential backoff, respecting ctx cancellation between attempts.
func (c *OpenAIClient) doJSON(ctx context.Context, method, endpoint string, body any, out any) error {
	const op = "openai_http"
	if c.apiKey == "" {
		return brainapi.E(brainapi.KindInvalid, op, "api key is required", nil)
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return brainapi.E(brainapi.KindInvalid, op, "request payload is invalid", err)
	}
	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			delay := time.Duration(1<<uint(attempt-1)) * time.Second
			if sleepErr := c.sleep(ctx, delay); sleepErr != nil {
				return brainapi.E(brainapi.KindInternal, op, "retry cancelled", sleepErr)
			}
		}
		retry, err := c.doOnce(ctx, method, endpoint, payload, out)
		if err == nil {
			return nil
		}
		lastErr = err
		if !retry {
			return err
		}
	}
	return lastErr
}

// doOnce performs a single HTTP round-trip. It returns (retry=true) for 429 and
// 5xx status codes so the caller can apply backoff and retry.
func (c *OpenAIClient) doOnce(ctx context.Context, method, endpoint string, payload []byte, out any) (retry bool, err error) {
	const op = "openai_http"
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+endpoint, bytes.NewReader(payload))
	if err != nil {
		return false, brainapi.E(brainapi.KindInvalid, op, "request is invalid", err)
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
		return false, brainapi.E(brainapi.KindInternal, op, "request failed", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return false, brainapi.E(brainapi.KindInternal, op, "response read failed", err)
	}
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return true, brainapi.E(brainapi.KindInternal, op,
			fmt.Sprintf("provider returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(data))), nil)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return false, brainapi.E(brainapi.KindInternal, op,
			fmt.Sprintf("provider returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(data))), nil)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return false, brainapi.E(brainapi.KindInternal, op, "response json is invalid", err)
	}
	return false, nil
}

var _ Embedder = (*OpenAIClient)(nil)
var _ Responder = (*OpenAIClient)(nil)
var _ Embedder = LocalClient{}
var _ Responder = LocalClient{}
