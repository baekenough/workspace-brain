// Package sources loads ingestable content for the control gateway.
package sources

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
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
	MaxBytes    int64
	Timeout     time.Duration
	HTTPClient  *http.Client
	FileBaseDir string // When empty, file:// sources are denied.
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

// WithFileBaseDir sets the directory that file:// URIs must reside within.
// Requests for paths outside this directory, or for file:// when unset, are
// rejected with KindInvalid to prevent local-file-inclusion attacks.
func WithFileBaseDir(dir string) Option {
	return func(l *Loader) {
		if dir != "" {
			l.FileBaseDir = filepath.Clean(dir)
		}
	}
}

// NewLoader creates a safe default source loader.
func NewLoader(opts ...Option) *Loader {
	l := &Loader{
		MaxBytes:   DefaultMaxBytes,
		Timeout:    DefaultTimeout,
		HTTPClient: newSSRFSafeClient(DefaultTimeout),
	}
	for _, opt := range opts {
		opt(l)
	}
	return l
}

// isBlockedIP reports whether ip falls within any address range that must not
// be reachable from an externally-supplied URI (SSRF prevention).
//
// Blocked ranges: loopback (127/8, ::1), link-local (169.254/16, fe80::/10),
// private / unique-local (RFC 1918, fc00::/7), and unspecified (0.0.0.0, ::).
func isBlockedIP(ip net.IP) bool {
	return ip.IsLoopback() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsPrivate() || // RFC 1918 + fc00::/7 (unique-local)
		ip.IsUnspecified()
}

// ssrfDialContext returns a DialContext function that validates all resolved IPs
// against SSRF-blocked ranges before connecting.
//
// rawDial performs the actual TCP connection.
// lookupIP resolves hostnames to IP addresses; pass net.DefaultResolver.LookupIPAddr
// in production and a mock in tests.
//
// Hostnames are resolved by lookupIP before dialing so that:
//   - every IP in the resolved set is validated, and
//   - the final dial uses the first resolved IP directly, preventing DNS rebinding.
func ssrfDialContext(
	rawDial func(ctx context.Context, network, address string) (net.Conn, error),
	lookupIP func(ctx context.Context, host string) ([]net.IPAddr, error),
) func(ctx context.Context, network, address string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("ssrf: invalid address %q: %w", address, err)
		}
		// Fast path: address is already a numeric IP.
		if ip := net.ParseIP(host); ip != nil {
			if isBlockedIP(ip) {
				return nil, fmt.Errorf("ssrf: connection to %s is not allowed", ip)
			}
			return rawDial(ctx, network, address)
		}
		// Hostname path: resolve, check each IP, then dial the first resolved
		// address directly to prevent DNS rebinding between check and connect.
		addrs, err := lookupIP(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("ssrf: failed to resolve %q: %w", host, err)
		}
		if len(addrs) == 0 {
			return nil, fmt.Errorf("ssrf: no addresses resolved for %q", host)
		}
		for _, a := range addrs {
			if isBlockedIP(a.IP) {
				return nil, fmt.Errorf("ssrf: connection to %s (%s) is not allowed", host, a.IP)
			}
		}
		return rawDial(ctx, network, net.JoinHostPort(addrs[0].IP.String(), port))
	}
}

// newSSRFSafeClient returns an *http.Client whose DialContext rejects any
// connection whose resolved IP falls within a blocked range.
func newSSRFSafeClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	d := &net.Dialer{}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: ssrfDialContext(d.DialContext, net.DefaultResolver.LookupIPAddr),
		},
		// CheckRedirect caps hop count.  DialContext validates the resolved IP
		// for every redirect target, so internal addresses are blocked even
		// after a 302 to an internal host.
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("ssrf: too many redirects")
			}
			return nil
		},
	}
}

// osStat and osOpenFile are package-level variables so tests can inject stubs
// that simulate errors after a successful filepath.EvalSymlinks.
var osStat = os.Stat
var osOpenFile = os.OpenFile

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
	// Deny file:// entirely when no safe base directory has been configured.
	if l.FileBaseDir == "" {
		return LoadedSource{}, brainapi.E(brainapi.KindInvalid, op, "file:// sources are not configured", nil)
	}
	filePath, err := fileURLPath(u)
	if err != nil {
		return LoadedSource{}, err
	}
	cleaned := filepath.Clean(filePath)

	// Phase 1: lexical containment check on the cleaned path.
	// This rejects path traversals and obviously-out-of-scope paths (e.g.,
	// /etc/passwd, ../secret) without requiring the target file to exist.
	// The separator suffix prevents "/allowed/dir" matching "/allowed/dirfoo/…".
	sep := string(filepath.Separator)
	if !strings.HasPrefix(cleaned+sep, l.FileBaseDir+sep) {
		return LoadedSource{}, brainapi.E(brainapi.KindInvalid, op, "file source is outside allowed directory", nil)
	}

	// Phase 2: symlink-resolved containment check.
	// Resolve symlinks on BOTH sides so that a symlink INSIDE FileBaseDir that
	// points outside is caught here rather than silently followed by the open.
	realBase, err := filepath.EvalSymlinks(l.FileBaseDir)
	if err != nil {
		return LoadedSource{}, brainapi.E(brainapi.KindInvalid, op, "file:// base directory is not accessible", err)
	}
	realPath, err := filepath.EvalSymlinks(cleaned)
	if err != nil {
		// File does not exist or is not reachable; phase 1 already ruled out
		// traversal, so this is a missing-file condition, not an attack.
		return LoadedSource{}, brainapi.E(brainapi.KindNotFound, op, "file source is not readable", err)
	}
	if !strings.HasPrefix(realPath+sep, realBase+sep) {
		return LoadedSource{}, brainapi.E(brainapi.KindInvalid, op, "file source is outside allowed directory", nil)
	}

	// Stat the resolved path to detect non-regular files (FIFO, device, socket)
	// before attempting to open: os.Open on a FIFO blocks indefinitely.
	info, statErr := osStat(realPath)
	if statErr != nil {
		return LoadedSource{}, brainapi.E(brainapi.KindNotFound, op, "file source is not readable", statErr)
	}
	if !info.Mode().IsRegular() {
		return LoadedSource{}, brainapi.E(brainapi.KindInvalid, op, "file source must be a regular file", nil)
	}

	// Open with O_NOFOLLOW to defeat the TOCTOU window between EvalSymlinks and
	// open: if the file at realPath is swapped for a symlink in that interval,
	// the kernel refuses the open call with ELOOP.
	f, err := osOpenFile(realPath, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
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
		out.Source.Name = filepath.Base(realPath)
	}
	if out.Source.MimeType == "" {
		out.Source.MimeType = mime.TypeByExtension(filepath.Ext(realPath))
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
		// Fall back to a fresh SSRF-safe client rather than http.DefaultClient,
		// so callers that construct Loader directly still get SSRF protection.
		client = newSSRFSafeClient(l.timeout())
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

// withOptionalTimeout applies timeout as an upper bound on ctx.  When the
// parent context already carries a deadline that fires sooner than timeout, the
// parent context is returned unchanged so the earlier deadline is honoured.
// This replaces the previous behaviour that silently ignored the timeout
// whenever any deadline existed at all, which could allow long-lived parent
// deadlines to bypass the intended per-load time cap.
func withOptionalTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return ctx, func() {}
	}
	if d, ok := ctx.Deadline(); ok && d.Before(time.Now().Add(timeout)) {
		// Parent deadline fires sooner; keep it as-is.
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, timeout)
}

var _ SourceLoader = (*Loader)(nil)
