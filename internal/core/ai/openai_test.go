package ai

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/sangyi/workspace-brain/pkg/brainapi"
)

func TestLocalClientDoesNotRequireAPIKey(t *testing.T) {
	t.Parallel()
	client := LocalClient{}
	vectors, err := client.Embed(context.Background(), []string{"alpha"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vectors) != 1 || len(vectors[0]) != 16 {
		t.Fatalf("vectors = %+v", vectors)
	}
	answer, err := client.Respond(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if answer != "local: hello" {
		t.Fatalf("answer = %q", answer)
	}
}

func TestLocalClientRespondReturnsErrorWhenInputIsBlank(t *testing.T) {
	t.Parallel()

	_, err := (LocalClient{}).Respond(context.Background(), " \t\n ")
	if err == nil {
		t.Fatal("expected blank input to be rejected")
	}
	if !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("expected KindInvalid, got %v", err)
	}
	if !strings.Contains(err.Error(), "input is required") {
		t.Fatalf("expected 'input is required' in error: %v", err)
	}
}

func TestOpenAIConfigFromEnvReturnsConfiguredValues(t *testing.T) {
	t.Setenv("OPENAI_BASE_URL", "https://example.test/v1")
	t.Setenv("OPENAI_API_KEY", "key")
	t.Setenv("OPENAI_EMBEDDING_MODEL", "embed")
	t.Setenv("OPENAI_RESPONSE_MODEL", "respond")
	t.Setenv("OPENAI_ORG_ID", "org")
	t.Setenv("OPENAI_PROJECT_ID", "project")

	cfg := OpenAIConfigFromEnv()
	if cfg.BaseURL != "https://example.test/v1" || cfg.APIKey != "key" || cfg.EmbeddingModel != "embed" || cfg.ResponseModel != "respond" || cfg.OrganizationID != "org" || cfg.ProjectID != "project" {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestOpenAIConfigFromEnvReturnsEmptyValuesWhenUnset(t *testing.T) {
	for _, key := range []string{"OPENAI_BASE_URL", "OPENAI_API_KEY", "OPENAI_EMBEDDING_MODEL", "OPENAI_RESPONSE_MODEL", "OPENAI_ORG_ID", "OPENAI_PROJECT_ID"} {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("Unsetenv(%s): %v", key, err)
		}
	}

	cfg := OpenAIConfigFromEnv()
	if cfg != (OpenAIConfig{}) {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestNewOpenAIClientAppliesDefaultsAndTrimsValues(t *testing.T) {
	t.Parallel()

	client, err := NewOpenAIClient(OpenAIConfig{
		BaseURL:        "  https://example.test/v1///  ",
		APIKey:         " key ",
		EmbeddingModel: " embed ",
		ResponseModel:  " respond ",
		OrganizationID: " org ",
		ProjectID:      " project ",
	})
	if err != nil {
		t.Fatalf("NewOpenAIClient: %v", err)
	}
	if client.baseURL != "https://example.test/v1" || client.apiKey != "key" || client.embeddingModel != "embed" || client.responseModel != "respond" || client.organizationID != "org" || client.projectID != "project" {
		t.Fatalf("client = %+v", client)
	}
	if client.httpClient == nil || client.httpClient.Timeout != defaultHTTPTimeout {
		t.Fatalf("http client = %+v", client.httpClient)
	}
}

func TestNewOpenAIClientUsesDefaultBaseURLAndProvidedHTTPClient(t *testing.T) {
	t.Parallel()
	provided := &http.Client{}

	client, err := NewOpenAIClient(OpenAIConfig{HTTPClient: provided})
	if err != nil {
		t.Fatalf("NewOpenAIClient: %v", err)
	}
	if client.baseURL != defaultOpenAIBaseURL {
		t.Fatalf("baseURL = %q", client.baseURL)
	}
	if client.httpClient != provided {
		t.Fatal("expected provided HTTP client to be preserved")
	}
}

func TestOpenAIClientEmbedRejectsNilClient(t *testing.T) {
	t.Parallel()

	var client *OpenAIClient
	_, err := client.Embed(context.Background(), []string{"alpha"})
	if err == nil {
		t.Fatal("expected nil client to be rejected")
	}
	if !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("expected KindInvalid, got %v", err)
	}
	if !strings.Contains(err.Error(), "client is required") {
		t.Fatalf("expected 'client is required' in error: %v", err)
	}
}

func TestOpenAIClientEmbedRequiresEmbeddingModel(t *testing.T) {
	t.Parallel()

	client, err := NewOpenAIClient(OpenAIConfig{APIKey: "key"})
	if err != nil {
		t.Fatalf("NewOpenAIClient: %v", err)
	}
	_, err = client.Embed(context.Background(), []string{"alpha"})
	if err == nil {
		t.Fatal("expected missing embedding model to be rejected")
	}
	if !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("expected KindInvalid, got %v", err)
	}
	if !strings.Contains(err.Error(), "embedding model is required") {
		t.Fatalf("expected 'embedding model is required' in error: %v", err)
	}
}

func TestOpenAIClientEmbedReturnsDoJSONError(t *testing.T) {
	t.Parallel()

	client, err := NewOpenAIClient(OpenAIConfig{EmbeddingModel: "embed-model"})
	if err != nil {
		t.Fatalf("NewOpenAIClient: %v", err)
	}
	_, err = client.Embed(context.Background(), []string{"alpha"})
	if err == nil {
		t.Fatal("expected doJSON error to be returned")
	}
	if !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("expected KindInvalid, got %v", err)
	}
	if !strings.Contains(err.Error(), "api key is required") {
		t.Fatalf("expected 'api key is required' in error: %v", err)
	}
}

func TestOpenAIClientEmbeddingsHTTP(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings" || r.Header.Get("Authorization") != "Bearer test-key" {
			t.Fatalf("unexpected request path=%q auth=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"embedding":[1,2,3]}]}`))
	}))
	defer server.Close()
	client, err := NewOpenAIClient(OpenAIConfig{BaseURL: server.URL, APIKey: "test-key", EmbeddingModel: "embed-model", HTTPClient: server.Client()})
	if err != nil {
		t.Fatalf("NewOpenAIClient: %v", err)
	}
	vectors, err := client.Embed(context.Background(), []string{"alpha"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vectors) != 1 || len(vectors[0]) != 3 || vectors[0][2] != 3 {
		t.Fatalf("vectors = %+v", vectors)
	}
}

func TestOpenAIClientRespondRejectsNilClient(t *testing.T) {
	t.Parallel()

	var client *OpenAIClient
	_, err := client.Respond(context.Background(), "hello")
	if err == nil {
		t.Fatal("expected nil client to be rejected")
	}
	if !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("expected KindInvalid, got %v", err)
	}
	if !strings.Contains(err.Error(), "client is required") {
		t.Fatalf("expected 'client is required' in error: %v", err)
	}
}

func TestOpenAIClientRespondRequiresResponseModel(t *testing.T) {
	t.Parallel()

	client, err := NewOpenAIClient(OpenAIConfig{APIKey: "key"})
	if err != nil {
		t.Fatalf("NewOpenAIClient: %v", err)
	}
	_, err = client.Respond(context.Background(), "hello")
	if err == nil {
		t.Fatal("expected missing response model to be rejected")
	}
	if !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("expected KindInvalid, got %v", err)
	}
	if !strings.Contains(err.Error(), "response model is required") {
		t.Fatalf("expected 'response model is required' in error: %v", err)
	}
}

func TestOpenAIClientRespondReturnsDoJSONError(t *testing.T) {
	t.Parallel()

	client, err := NewOpenAIClient(OpenAIConfig{ResponseModel: "response-model"})
	if err != nil {
		t.Fatalf("NewOpenAIClient: %v", err)
	}
	_, err = client.Respond(context.Background(), "hello")
	if err == nil {
		t.Fatal("expected doJSON error to be returned")
	}
	if !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("expected KindInvalid, got %v", err)
	}
	if !strings.Contains(err.Error(), "api key is required") {
		t.Fatalf("expected 'api key is required' in error: %v", err)
	}
}

func TestOpenAIClientRespondReturnsOutputText(t *testing.T) {
	t.Parallel()

	client := newResponseClient(t, `{"output_text":" direct answer "}`, func(r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Fatalf("path = %q", r.URL.Path)
		}
	})
	answer, err := client.Respond(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if answer != " direct answer " {
		t.Fatalf("answer = %q", answer)
	}
}

func TestOpenAIClientRespondReturnsNestedOutputText(t *testing.T) {
	t.Parallel()

	client := newResponseClient(t, `{"output":[{"content":[{"type":"ignored","text":"no"},{"type":"output_text","text":"nested answer"}]}]}`, nil)
	answer, err := client.Respond(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if answer != "nested answer" {
		t.Fatalf("answer = %q", answer)
	}
}

func TestOpenAIClientRespondReturnsNestedTextType(t *testing.T) {
	t.Parallel()

	client := newResponseClient(t, `{"output":[{"content":[{"type":"text","text":"plain nested answer"}]}]}`, nil)
	answer, err := client.Respond(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if answer != "plain nested answer" {
		t.Fatalf("answer = %q", answer)
	}
}

func TestOpenAIClientRespondErrorsWhenResponseHasNoText(t *testing.T) {
	t.Parallel()

	client := newResponseClient(t, `{"output_text":"   ","output":[{"content":[{"type":"output_text","text":"  "},{"type":"other","text":"ignored"}]}]}`, nil)
	_, err := client.Respond(context.Background(), "hello")
	if err == nil {
		t.Fatal("expected response without text to fail")
	}
	if !brainapi.IsKind(err, brainapi.KindInternal) {
		t.Fatalf("expected KindInternal, got %v", err)
	}
	if !strings.Contains(err.Error(), "response did not include text") {
		t.Fatalf("expected 'response did not include text' in error: %v", err)
	}
}

func TestOpenAIClientDoJSONRequiresAPIKey(t *testing.T) {
	t.Parallel()

	client, err := NewOpenAIClient(OpenAIConfig{})
	if err != nil {
		t.Fatalf("NewOpenAIClient: %v", err)
	}
	err = client.doJSON(context.Background(), http.MethodPost, "/anything", map[string]any{}, &struct{}{})
	if err == nil {
		t.Fatal("expected missing api key to fail")
	}
	if !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("expected KindInvalid, got %v", err)
	}
	if !strings.Contains(err.Error(), "api key is required") {
		t.Fatalf("expected 'api key is required' in error: %v", err)
	}
}

func TestOpenAIClientDoJSONRejectsInvalidPayload(t *testing.T) {
	t.Parallel()

	client, err := NewOpenAIClient(OpenAIConfig{APIKey: "key"})
	if err != nil {
		t.Fatalf("NewOpenAIClient: %v", err)
	}
	err = client.doJSON(context.Background(), http.MethodPost, "/anything", map[string]any{"bad": func() {}}, &struct{}{})
	if err == nil {
		t.Fatal("expected invalid payload to fail")
	}
	if !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("expected KindInvalid, got %v", err)
	}
	if !strings.Contains(err.Error(), "request payload is invalid") {
		t.Fatalf("expected 'request payload is invalid' in error: %v", err)
	}
}

func TestOpenAIClientDoJSONRejectsInvalidRequest(t *testing.T) {
	t.Parallel()

	client, err := NewOpenAIClient(OpenAIConfig{BaseURL: "http://[::1", APIKey: "key"})
	if err != nil {
		t.Fatalf("NewOpenAIClient: %v", err)
	}
	err = client.doJSON(context.Background(), http.MethodPost, "/anything", map[string]any{}, &struct{}{})
	if err == nil {
		t.Fatal("expected invalid request URL to fail")
	}
	if !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("expected KindInvalid, got %v", err)
	}
	if !strings.Contains(err.Error(), "request is invalid") {
		t.Fatalf("expected 'request is invalid' in error: %v", err)
	}
}

func TestOpenAIClientDoJSONReturnsRequestFailure(t *testing.T) {
	t.Parallel()

	client, err := NewOpenAIClient(OpenAIConfig{
		BaseURL:    "https://example.test",
		APIKey:     "key",
		HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("network down") })},
	})
	if err != nil {
		t.Fatalf("NewOpenAIClient: %v", err)
	}
	err = client.doJSON(context.Background(), http.MethodPost, "/anything", map[string]any{}, &struct{}{})
	if err == nil {
		t.Fatal("expected request failure")
	}
	if !brainapi.IsKind(err, brainapi.KindInternal) {
		t.Fatalf("expected KindInternal, got %v", err)
	}
	if !strings.Contains(err.Error(), "request failed") {
		t.Fatalf("expected 'request failed' in error: %v", err)
	}
}

func TestOpenAIClientDoJSONReturnsReadFailure(t *testing.T) {
	t.Parallel()

	client, err := NewOpenAIClient(OpenAIConfig{
		BaseURL: "https://example.test",
		APIKey:  "key",
		HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: errorReadCloser{}}, nil
		})},
	})
	if err != nil {
		t.Fatalf("NewOpenAIClient: %v", err)
	}
	err = client.doJSON(context.Background(), http.MethodPost, "/anything", map[string]any{}, &struct{}{})
	if err == nil {
		t.Fatal("expected response read failure")
	}
	if !brainapi.IsKind(err, brainapi.KindInternal) {
		t.Fatalf("expected KindInternal, got %v", err)
	}
	if !strings.Contains(err.Error(), "response read failed") {
		t.Fatalf("expected 'response read failed' in error: %v", err)
	}
}

func TestOpenAIClientDoJSONReturnsNon2xxStatus(t *testing.T) {
	t.Parallel()

	client := newClientForServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte(`{"error":"short and stout"}`))
	}))
	err := client.doJSON(context.Background(), http.MethodPost, "/anything", map[string]any{}, &struct{}{})
	if err == nil {
		t.Fatal("expected non-2xx response to fail")
	}
	if !brainapi.IsKind(err, brainapi.KindInternal) {
		t.Fatalf("expected KindInternal, got %v", err)
	}
	if !strings.Contains(err.Error(), "418") {
		t.Fatalf("expected status code 418 in error: %v", err)
	}
	if !strings.Contains(err.Error(), "short and stout") {
		t.Fatalf("expected provider body in error: %v", err)
	}
}

func TestOpenAIClientDoJSONReturnsInvalidJSON(t *testing.T) {
	t.Parallel()

	client := newClientForServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`not-json`))
	}))
	err := client.doJSON(context.Background(), http.MethodPost, "/anything", map[string]any{}, &struct{}{})
	if err == nil {
		t.Fatal("expected invalid json to fail")
	}
	if !brainapi.IsKind(err, brainapi.KindInternal) {
		t.Fatalf("expected KindInternal, got %v", err)
	}
	if !strings.Contains(err.Error(), "response json is invalid") {
		t.Fatalf("expected 'response json is invalid' in error: %v", err)
	}
}

func TestOpenAIClientDoJSONSendsHeadersAndDecodesResponse(t *testing.T) {
	t.Parallel()

	client := newClientForServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %q", r.Method)
		}
		if r.URL.Path != "/anything" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		if r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Authorization") != "Bearer key" || r.Header.Get("OpenAI-Organization") != "org" || r.Header.Get("OpenAI-Project") != "project" {
			t.Fatalf("headers = %+v", r.Header)
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	client.organizationID = "org"
	client.projectID = "project"

	var out struct {
		OK bool `json:"ok"`
	}
	if err := client.doJSON(context.Background(), http.MethodPost, "/anything", map[string]any{"hello": "world"}, &out); err != nil {
		t.Fatalf("doJSON: %v", err)
	}
	if !out.OK {
		t.Fatalf("out = %+v", out)
	}
}

func newResponseClient(t *testing.T, response string, assert func(*http.Request)) *OpenAIClient {
	t.Helper()
	return newClientForServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if assert != nil {
			assert(r)
		}
		_, _ = w.Write([]byte(response))
	}))
}

func newClientForServer(t *testing.T, handler http.Handler) *OpenAIClient {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := NewOpenAIClient(OpenAIConfig{
		BaseURL:       server.URL,
		APIKey:        "key",
		ResponseModel: "response-model",
		HTTPClient:    server.Client(),
	})
	if err != nil {
		t.Fatalf("NewOpenAIClient: %v", err)
	}
	return client
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

type errorReadCloser struct{}

func (errorReadCloser) Read([]byte) (int, error) {
	return 0, errors.New("read failed")
}

func (errorReadCloser) Close() error {
	return nil
}

var _ io.ReadCloser = errorReadCloser{}
