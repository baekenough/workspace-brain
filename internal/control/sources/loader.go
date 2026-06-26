// Package sources loads ingestable content for the control gateway.
package sources

import (
	"context"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/sangyi/workspace-brain/pkg/brainapi"
)

const (
	// MetadataContentKey is the metadata field consumed by the memory core.
	MetadataContentKey = "content"
	// DefaultMaxBytes bounds source reads to keep gateway ingestion safe.
	DefaultMaxBytes int64 = 1 << 20 // 1 MiB
	// DefaultTimeout bounds network source loading.
	DefaultTimeout = 5 * time.Second
)

// SourceLoader resolves source bytes and metadata before core ingestion.
type SourceLoader interface {
	Load(ctx context.Context, source brainapi.SourceRef, metadata map[string]string) (LoadedSource, error)
}

// LoadedSource is a source reference plus metadata ready for core ingestion.
type LoadedSource struct {
	Source   brainapi.SourceRef
	Metadata map[string]string
}

// Loader loads file, HTTP(S), and inline/raw text sources using the standard library.
type Loader struct {
	MaxBytes   int64
	Timeout    time.Duration
	HTTPClient *http.Client
}

// Option configures Loader.
type Option func(*Loader)

// WithMaxBytes sets the maximum accepted source body size.
func WithMaxBytes(maxBytes int64) Option {
	return func(l *Loader) {
		if maxBytes > 0 {
			l.MaxBytes = maxBytes
		}
	}
}

// WithTimeout sets the timeout applied to a load operation when the context has no earlier deadline.
func WithTimeout(timeout time.Duration) Option {
	return func(l *Loader) {
		if timeout > 0 {
			l.Timeout = timeout
		}
	}
}

// WithHTTPClient sets the client used for HTTP(S) sources.
func WithHTTPClient(client *http.Client) Option {
	return func(l *Loader) {
		if client != nil {
			l.HTTPClient = client
		}
	}
}

// NewLoader creates a safe default source loader.
func NewLoader(opts ...Option) *Loader {
	l := &Loader{MaxBytes: DefaultMaxBytes, Timeout: DefaultTimeout, HTTPClient: http.DefaultClient}
	for _, opt := range opts {
		opt(l)
	}
	return l
}

// Load returns metadata with MetadataContentKey populated, preserving existing metadata.
func (l *Loader) Load(ctx context.Context, source brainapi.SourceRef, metadata map[string]string) (LoadedSource, error) {
	const op = "source_load"
	if ctx == nil {
		ctx = context.Background()
	}
	loader := l
	if loader == nil {
		loader = NewLoader()
	}
	ctx, cancel := withOptionalTimeout(ctx, loader.timeout())
	defer cancel()

	out := LoadedSource{Source: source, Metadata: cloneMap(metadata)}
	if strings.TrimSpace(out.Metadata[MetadataContentKey]) != "" {
		return out, nil
	}
	if inline := firstNonBlank(out.Metadata, "inline_content", "inline", "raw", "text"); inline != "" {
		ensureMetadata(&out)
		out.Metadata[MetadataContentKey] = inline
		fillTextDefaults(&out)
		return out, nil
	}

	u, err := url.Parse(strings.TrimSpace(source.URI))
	if err != nil || u.Scheme == "" {
		text := strings.TrimSpace(source.URI)
		if text == "" {
			text = strings.TrimSpace(source.Name)
		}
		if text == "" {
			return LoadedSource{}, brainapi.E(brainapi.KindInvalid, op, "source content is required", err)
		}
		ensureMetadata(&out)
		out.Metadata[MetadataContentKey] = text
		fillTextDefaults(&out)
		return out, nil
	}

	switch strings.ToLower(u.Scheme) {
	case "file":
		return loader.loadFile(ctx, out, u)
	case "http", "https":
		return loader.loadHTTP(ctx, out)
	case "text", "raw":
		text := strings.TrimPrefix(source.URI, u.Scheme+"://")
		if text == source.URI {
			text = strings.TrimPrefix(source.URI, u.Scheme+":")
		}
		text, _ = url.PathUnescape(text)
		if strings.TrimSpace(text) == "" {
			return LoadedSource{}, brainapi.E(brainapi.KindInvalid, op, "raw text source is empty", nil)
		}
		ensureMetadata(&out)
		out.Metadata[MetadataContentKey] = text
		fillTextDefaults(&out)
		return out, nil
	default:
		ensureMetadata(&out)
		out.Metadata[MetadataContentKey] = strings.TrimSpace(source.URI)
		fillTextDefaults(&out)
		return out, nil
	}
}

func (l *Loader) loadFile(ctx context.Context, out LoadedSource, u *url.URL) (LoadedSource, error) {
	const op = "source_load_file"
	filePath, err := fileURLPath(u)
	if err != nil {
		return LoadedSource{}, err
	}
	f, err := os.Open(filePath)
	if err != nil {
		return LoadedSource{}, brainapi.E(brainapi.KindNotFound, op, "file source is not readable", err)
	}
	defer f.Close()
	content, err := readLimited(ctx, f, l.maxBytes())
	if err != nil {
		return LoadedSource{}, err
	}
	ensureMetadata(&out)
	out.Metadata[MetadataContentKey] = content
	if out.Source.Name == "" {
		out.Source.Name = filepath.Base(filePath)
	}
	if out.Source.MimeType == "" {
		out.Source.MimeType = mime.TypeByExtension(filepath.Ext(filePath))
	}
	if out.Source.MimeType == "" {
		out.Source.MimeType = "text/plain; charset=utf-8"
	}
	return out, nil
}

func (l *Loader) loadHTTP(ctx context.Context, out LoadedSource) (LoadedSource, error) {
	const op = "source_load_http"
	client := l.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, out.Source.URI, nil)
	if err != nil {
		return LoadedSource{}, brainapi.E(brainapi.KindInvalid, op, "http source request is invalid", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return LoadedSource{}, brainapi.E(brainapi.KindInternal, op, "http source request failed", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return LoadedSource{}, brainapi.E(brainapi.KindInvalid, op, "http source returned non-success status", nil)
	}
	content, err := readLimited(ctx, resp.Body, l.maxBytes())
	if err != nil {
		return LoadedSource{}, err
	}
	ensureMetadata(&out)
	out.Metadata[MetadataContentKey] = content
	if out.Source.MimeType == "" {
		out.Source.MimeType = strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0])
	}
	if out.Source.Name == "" {
		out.Source.Name = responseName(resp)
	}
	return out, nil
}

func fileURLPath(u *url.URL) (string, error) {
	if u.Host != "" && u.Host != "localhost" {
		return "", brainapi.E(brainapi.KindInvalid, "source_load_file", "file source host is not local", nil)
	}
	p, err := url.PathUnescape(u.Path)
	if err != nil {
		return "", brainapi.E(brainapi.KindInvalid, "source_load_file", "file source path is invalid", err)
	}
	if strings.TrimSpace(p) == "" {
		return "", brainapi.E(brainapi.KindInvalid, "source_load_file", "file source path is required", nil)
	}
	return p, nil
}

func readLimited(ctx context.Context, r io.Reader, maxBytes int64) (string, error) {
	const op = "source_read"
	if err := ctx.Err(); err != nil {
		return "", brainapi.E(brainapi.KindInternal, op, "source read canceled", err)
	}
	limited := io.LimitReader(r, maxBytes+1)
	b, err := io.ReadAll(limited)
	if err != nil {
		return "", brainapi.E(brainapi.KindInternal, op, "source read failed", err)
	}
	if int64(len(b)) > maxBytes {
		return "", brainapi.E(brainapi.KindInvalid, op, "source exceeds size limit", nil)
	}
	if strings.TrimSpace(string(b)) == "" {
		return "", brainapi.E(brainapi.KindInvalid, op, "source content is empty", nil)
	}
	return string(b), nil
}

func responseName(resp *http.Response) string {
	if cd := resp.Header.Get("Content-Disposition"); cd != "" {
		_, params, err := mime.ParseMediaType(cd)
		if err == nil && strings.TrimSpace(params["filename"]) != "" {
			return params["filename"]
		}
	}
	if resp.Request != nil && resp.Request.URL != nil {
		name := path.Base(resp.Request.URL.Path)
		if name != "." && name != "/" && name != "" {
			return name
		}
	}
	return ""
}

func fillTextDefaults(out *LoadedSource) {
	if out.Source.MimeType == "" {
		out.Source.MimeType = "text/plain; charset=utf-8"
	}
	if out.Source.Name == "" {
		out.Source.Name = "inline text"
	}
}

func ensureMetadata(out *LoadedSource) {
	if out.Metadata == nil {
		out.Metadata = make(map[string]string, 1)
	}
}

func cloneMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func firstNonBlank(m map[string]string, keys ...string) string {
	for _, key := range keys {
		if strings.TrimSpace(m[key]) != "" {
			return m[key]
		}
	}
	return ""
}

func (l *Loader) maxBytes() int64 {
	if l == nil || l.MaxBytes <= 0 {
		return DefaultMaxBytes
	}
	return l.MaxBytes
}

func (l *Loader) timeout() time.Duration {
	if l == nil || l.Timeout <= 0 {
		return DefaultTimeout
	}
	return l.Timeout
}

func withOptionalTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return ctx, func() {}
	}
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, timeout)
}

var _ SourceLoader = (*Loader)(nil)
