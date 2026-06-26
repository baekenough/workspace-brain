package sources

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sangyi/workspace-brain/pkg/brainapi"
)

func TestLoaderLoadsFileAndEnrichesMetadata(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(path, []byte("alpha beta"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	loaded, err := NewLoader().Load(context.Background(), brainapi.SourceRef{URI: "file://" + path}, map[string]string{"keep": "yes"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Metadata[MetadataContentKey] != "alpha beta" || loaded.Metadata["keep"] != "yes" {
		t.Fatalf("metadata = %+v", loaded.Metadata)
	}
	if loaded.Source.Name != "note.txt" || !strings.HasPrefix(loaded.Source.MimeType, "text/plain") {
		t.Fatalf("source = %+v", loaded.Source)
	}
}

func TestLoaderUsesInlineContent(t *testing.T) {
	t.Parallel()
	loaded, err := NewLoader().Load(context.Background(), brainapi.SourceRef{URI: "ignored"}, map[string]string{"inline_content": "inline alpha"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Metadata[MetadataContentKey] != "inline alpha" || loaded.Source.MimeType == "" || loaded.Source.Name == "" {
		t.Fatalf("loaded = %+v", loaded)
	}
}

func TestLoaderFailureIsTyped(t *testing.T) {
	t.Parallel()
	_, err := NewLoader().Load(context.Background(), brainapi.SourceRef{URI: "file:///definitely/missing/workspace-brain.txt"}, nil)
	if !brainapi.IsKind(err, brainapi.KindNotFound) {
		t.Fatalf("kind=%q err=%v", brainapi.KindOf(err), err)
	}
}

func TestLoaderHTTPSizeLimit(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("too large"))
	}))
	defer server.Close()
	_, err := NewLoader(WithMaxBytes(3)).Load(context.Background(), brainapi.SourceRef{URI: server.URL + "/doc.txt"}, nil)
	if !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("kind=%q err=%v", brainapi.KindOf(err), err)
	}
}

func TestLoaderOptionsSetPositiveValuesAndIgnoreInvalidValues(t *testing.T) {
	t.Parallel()
	client := &http.Client{}
	loader := NewLoader(WithTimeout(2*time.Second), WithHTTPClient(client), WithMaxBytes(9))
	if loader.Timeout != 2*time.Second {
		t.Fatalf("Timeout = %v", loader.Timeout)
	}
	if loader.HTTPClient != client {
		t.Fatalf("HTTPClient was not configured")
	}
	if loader.MaxBytes != 9 {
		t.Fatalf("MaxBytes = %d", loader.MaxBytes)
	}

	loader = NewLoader(WithTimeout(0), WithHTTPClient(nil), WithMaxBytes(0))
	if loader.Timeout != DefaultTimeout {
		t.Fatalf("Timeout = %v", loader.Timeout)
	}
	if loader.HTTPClient != http.DefaultClient {
		t.Fatalf("HTTPClient = %#v", loader.HTTPClient)
	}
	if loader.MaxBytes != DefaultMaxBytes {
		t.Fatalf("MaxBytes = %d", loader.MaxBytes)
	}
}

func TestLoadReturnsExistingContentWithoutReplacingMetadata(t *testing.T) {
	t.Parallel()
	loaded, err := NewLoader().Load(context.Background(), brainapi.SourceRef{URI: "ignored"}, map[string]string{MetadataContentKey: "already loaded"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Metadata[MetadataContentKey] != "already loaded" {
		t.Fatalf("content = %q", loaded.Metadata[MetadataContentKey])
	}
}

func TestNilLoaderAndNilContextUseDefaultsForPlainText(t *testing.T) {
	t.Parallel()
	var loader *Loader
	loaded, err := loader.Load(nil, brainapi.SourceRef{Name: "fallback text"}, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Metadata[MetadataContentKey] != "fallback text" {
		t.Fatalf("content = %q", loaded.Metadata[MetadataContentKey])
	}
	if loaded.Source.Name != "fallback text" {
		t.Fatalf("source name = %q", loaded.Source.Name)
	}
}

func TestLoadRejectsMissingContent(t *testing.T) {
	t.Parallel()
	_, err := NewLoader().Load(context.Background(), brainapi.SourceRef{}, nil)
	if !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("kind=%q err=%v", brainapi.KindOf(err), err)
	}
}

func TestLoadTreatsInvalidURIAsText(t *testing.T) {
	t.Parallel()
	loaded, err := NewLoader().Load(context.Background(), brainapi.SourceRef{URI: "%zz"}, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Metadata[MetadataContentKey] != "%zz" {
		t.Fatalf("content = %q", loaded.Metadata[MetadataContentKey])
	}
}

func TestLoadRawTextSchemeUnescapesContent(t *testing.T) {
	t.Parallel()
	loaded, err := NewLoader().Load(context.Background(), brainapi.SourceRef{URI: "raw:hello%20world"}, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Metadata[MetadataContentKey] != "hello world" {
		t.Fatalf("content = %q", loaded.Metadata[MetadataContentKey])
	}
}

func TestLoadTextSchemeWithSlashesUsesSchemePrefix(t *testing.T) {
	t.Parallel()
	loaded, err := NewLoader().Load(context.Background(), brainapi.SourceRef{URI: "text://hello-again"}, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Metadata[MetadataContentKey] != "hello-again" {
		t.Fatalf("content = %q", loaded.Metadata[MetadataContentKey])
	}
}

func TestLoadRejectsEmptyRawText(t *testing.T) {
	t.Parallel()
	_, err := NewLoader().Load(context.Background(), brainapi.SourceRef{URI: "raw:"}, nil)
	if !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("kind=%q err=%v", brainapi.KindOf(err), err)
	}
}

func TestLoadUnsupportedSchemeFallsBackToText(t *testing.T) {
	t.Parallel()
	loaded, err := NewLoader().Load(context.Background(), brainapi.SourceRef{URI: "custom://value"}, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Metadata[MetadataContentKey] != "custom://value" {
		t.Fatalf("content = %q", loaded.Metadata[MetadataContentKey])
	}
}

func TestLoadHTTPPopulatesContentTypeAndName(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="from-header.md"`)
		_, _ = w.Write([]byte("http body"))
	}))
	defer server.Close()

	loaded, err := NewLoader().Load(context.Background(), brainapi.SourceRef{URI: server.URL + "/path-name.txt"}, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Metadata[MetadataContentKey] != "http body" {
		t.Fatalf("content = %q", loaded.Metadata[MetadataContentKey])
	}
	if loaded.Source.MimeType != "text/markdown" {
		t.Fatalf("mime = %q", loaded.Source.MimeType)
	}
	if loaded.Source.Name != "from-header.md" {
		t.Fatalf("name = %q", loaded.Source.Name)
	}
}

func TestLoadHTTPUsesConfiguredClient(t *testing.T) {
	t.Parallel()
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("custom client")),
			Request:    req,
		}, nil
	})}
	loaded, err := NewLoader(WithHTTPClient(client)).Load(context.Background(), brainapi.SourceRef{URI: "http://example.test/client.txt"}, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Metadata[MetadataContentKey] != "custom client" {
		t.Fatalf("content = %q", loaded.Metadata[MetadataContentKey])
	}
	if loaded.Source.Name != "client.txt" {
		t.Fatalf("name = %q", loaded.Source.Name)
	}
}

func TestLoadHTTPRejectsNonSuccessStatus(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusTeapot)
	}))
	defer server.Close()
	_, err := NewLoader().Load(context.Background(), brainapi.SourceRef{URI: server.URL}, nil)
	if !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("kind=%q err=%v", brainapi.KindOf(err), err)
	}
}

func TestLoadHTTPWrapsClientError(t *testing.T) {
	t.Parallel()
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return nil, errors.New("network down")
	})}
	_, err := NewLoader(WithHTTPClient(client)).Load(context.Background(), brainapi.SourceRef{URI: "http://example.test/fail"}, nil)
	if !brainapi.IsKind(err, brainapi.KindInternal) {
		t.Fatalf("kind=%q err=%v", brainapi.KindOf(err), err)
	}
}

func TestLoadHTTPWrapsReadError(t *testing.T) {
	t.Parallel()
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       errReadCloser{},
			Request:    req,
		}, nil
	})}
	_, err := NewLoader(WithHTTPClient(client)).Load(context.Background(), brainapi.SourceRef{URI: "http://example.test/read"}, nil)
	if !brainapi.IsKind(err, brainapi.KindInternal) {
		t.Fatalf("kind=%q err=%v", brainapi.KindOf(err), err)
	}
}

func TestLoadHTTPRejectsEmptyBody(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("   \n"))
	}))
	defer server.Close()
	_, err := NewLoader().Load(context.Background(), brainapi.SourceRef{URI: server.URL}, nil)
	if !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("kind=%q err=%v", brainapi.KindOf(err), err)
	}
}

func TestLoadHTTPPreservesExistingNameAndMimeType(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("body"))
	}))
	defer server.Close()
	loaded, err := NewLoader().Load(context.Background(), brainapi.SourceRef{URI: server.URL + "/server.txt", Name: "given", MimeType: "application/custom"}, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Source.Name != "given" || loaded.Source.MimeType != "application/custom" {
		t.Fatalf("source = %+v", loaded.Source)
	}
}

func TestLoadFileRejectsNonLocalHost(t *testing.T) {
	t.Parallel()
	_, err := NewLoader().Load(context.Background(), brainapi.SourceRef{URI: "file://remote.example/tmp/a.txt"}, nil)
	if !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("kind=%q err=%v", brainapi.KindOf(err), err)
	}
}

func TestFileURLPathRejectsInvalidEscapedPath(t *testing.T) {
	t.Parallel()
	_, err := fileURLPath(&url.URL{Path: "/tmp/%zz"})
	if !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("kind=%q err=%v", brainapi.KindOf(err), err)
	}
}

func TestLoadFileRejectsMissingPath(t *testing.T) {
	t.Parallel()
	_, err := NewLoader().Load(context.Background(), brainapi.SourceRef{URI: "file://localhost"}, nil)
	if !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("kind=%q err=%v", brainapi.KindOf(err), err)
	}
}

func TestLoadFileUsesLocalhostAndPlainTextFallback(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "note.unknownext")
	if err := os.WriteFile(path, []byte("file body"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	loaded, err := NewLoader().Load(context.Background(), brainapi.SourceRef{URI: "file://localhost" + path}, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Source.MimeType != "text/plain; charset=utf-8" {
		t.Fatalf("mime = %q", loaded.Source.MimeType)
	}
}

func TestLoadFilePreservesExistingNameAndMimeType(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(path, []byte("file body"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	loaded, err := NewLoader().Load(context.Background(), brainapi.SourceRef{URI: "file://" + path, Name: "given", MimeType: "application/custom"}, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Source.Name != "given" || loaded.Source.MimeType != "application/custom" {
		t.Fatalf("source = %+v", loaded.Source)
	}
}

func TestReadLimitedRejectsCanceledContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := readLimited(ctx, strings.NewReader("body"), 10)
	if !brainapi.IsKind(err, brainapi.KindInternal) {
		t.Fatalf("kind=%q err=%v", brainapi.KindOf(err), err)
	}
}

func TestReadLimitedWrapsReadError(t *testing.T) {
	t.Parallel()
	_, err := readLimited(context.Background(), errReader{}, 10)
	if !brainapi.IsKind(err, brainapi.KindInternal) {
		t.Fatalf("kind=%q err=%v", brainapi.KindOf(err), err)
	}
}

func TestResponseNameReturnsPathBaseAndEmptyFallback(t *testing.T) {
	t.Parallel()
	withPath := &http.Response{Request: &http.Request{URL: &url.URL{Path: "/docs/name.txt"}}}
	if got := responseName(withPath); got != "name.txt" {
		t.Fatalf("path name = %q", got)
	}
	if got := responseName(&http.Response{Header: http.Header{"Content-Disposition": []string{"not valid; ;"}}}); got != "" {
		t.Fatalf("empty fallback = %q", got)
	}
}

func TestMaxBytesTimeoutAndOptionalTimeoutFallbacks(t *testing.T) {
	t.Parallel()
	var nilLoader *Loader
	if nilLoader.maxBytes() != DefaultMaxBytes {
		t.Fatalf("nil maxBytes = %d", nilLoader.maxBytes())
	}
	if (&Loader{MaxBytes: -1}).maxBytes() != DefaultMaxBytes {
		t.Fatalf("negative maxBytes fallback failed")
	}
	if nilLoader.timeout() != DefaultTimeout {
		t.Fatalf("nil timeout = %v", nilLoader.timeout())
	}
	if (&Loader{Timeout: -1}).timeout() != DefaultTimeout {
		t.Fatalf("negative timeout fallback failed")
	}

	ctx := context.Background()
	ctxNoTimeout, cancelNoTimeout := withOptionalTimeout(ctx, 0)
	defer cancelNoTimeout()
	if ctxNoTimeout != ctx {
		t.Fatalf("zero timeout replaced context")
	}

	deadlineCtx, cancelDeadline := context.WithDeadline(ctx, time.Now().Add(time.Hour))
	defer cancelDeadline()
	ctxWithDeadline, cancelWithDeadline := withOptionalTimeout(deadlineCtx, time.Second)
	defer cancelWithDeadline()
	if ctxWithDeadline != deadlineCtx {
		t.Fatalf("deadline context replaced")
	}

	ctxWithTimeout, cancelWithTimeout := withOptionalTimeout(ctx, time.Hour)
	defer cancelWithTimeout()
	if _, ok := ctxWithTimeout.Deadline(); !ok {
		t.Fatalf("timeout context has no deadline")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) {
	return 0, errors.New("read failed")
}

type errReadCloser struct{}

func (errReadCloser) Read([]byte) (int, error) {
	return 0, errors.New("read failed")
}

func (errReadCloser) Close() error {
	return nil
}

func TestLoadFilePropagatesReadLimitedError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.txt")
	if err := os.WriteFile(path, []byte(" \n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, err := NewLoader().Load(context.Background(), brainapi.SourceRef{URI: "file://" + path}, nil)
	if !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("kind=%q err=%v", brainapi.KindOf(err), err)
	}
}

func TestLoadHTTPUsesDefaultClientWhenClientIsNil(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("default client"))
	}))
	defer server.Close()

	loaded, err := (&Loader{HTTPClient: nil}).loadHTTP(context.Background(), LoadedSource{Source: brainapi.SourceRef{URI: server.URL}})
	if err != nil {
		t.Fatalf("loadHTTP: %v", err)
	}
	if loaded.Metadata[MetadataContentKey] != "default client" {
		t.Fatalf("content = %q", loaded.Metadata[MetadataContentKey])
	}
}

func TestLoadHTTPRejectsInvalidRequestURI(t *testing.T) {
	t.Parallel()
	_, err := NewLoader().loadHTTP(context.Background(), LoadedSource{Source: brainapi.SourceRef{URI: "http://%zz"}})
	if !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("kind=%q err=%v", brainapi.KindOf(err), err)
	}
}
