package sources

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sangyi/workspace-brain/pkg/brainapi"
)

// testHTTPLoader returns a Loader whose HTTP client is a plain &http.Client{}.
// Tests that talk to httptest servers must use this helper because the default
// SSRF-safe client blocks loopback addresses (127.0.0.1) used by httptest.
func testHTTPLoader(opts ...Option) *Loader {
	return NewLoader(append([]Option{WithHTTPClient(&http.Client{})}, opts...)...)
}

// ── FIX 1: isBlockedIP unit tests ────────────────────────────────────────────

func TestIsBlockedIPCoversAllRanges(t *testing.T) {
	t.Parallel()
	blocked := []string{
		"127.0.0.1",       // loopback
		"127.255.255.255", // loopback tail
		"::1",             // IPv6 loopback
		"169.254.1.1",     // IPv4 link-local
		"fe80::1",         // IPv6 link-local
		"10.0.0.1",        // RFC1918 10/8
		"10.255.255.255",  // RFC1918 10/8 tail
		"172.16.0.1",      // RFC1918 172.16/12
		"172.31.255.255",  // RFC1918 172.16/12 tail
		"192.168.0.1",     // RFC1918 192.168/16
		"192.168.255.255", // RFC1918 192.168/16 tail
		"fc00::1",         // unique-local
		"fd00::1",         // unique-local
		"0.0.0.0",         // unspecified
		"::",              // IPv6 unspecified
	}
	notBlocked := []string{
		"8.8.8.8",
		"1.1.1.1",
		"2001:db8::1",
		"172.15.255.255", // just below 172.16/12
		"172.32.0.0",     // just above 172.16/12
		"192.169.0.0",    // just above 192.168/16
		"11.0.0.0",       // just above 10/8
	}
	for _, addr := range blocked {
		ip := net.ParseIP(addr)
		if ip == nil {
			t.Fatalf("invalid test IP: %s", addr)
		}
		if !isBlockedIP(ip) {
			t.Errorf("isBlockedIP(%s) = false, want true", addr)
		}
	}
	for _, addr := range notBlocked {
		ip := net.ParseIP(addr)
		if ip == nil {
			t.Fatalf("invalid test IP: %s", addr)
		}
		if isBlockedIP(ip) {
			t.Errorf("isBlockedIP(%s) = true, want false", addr)
		}
	}
}

// TestSSRFDialContextBranches exercises all branches of ssrfDialContext using
// injectable mock functions, avoiding real network connections.
func TestSSRFDialContextBranches(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var lastDialedAddr string
	mockDial := func(_ context.Context, _, addr string) (net.Conn, error) {
		lastDialedAddr = addr
		return nil, errors.New("mock-dial")
	}
	mockLookup := func(_ context.Context, host string) ([]net.IPAddr, error) {
		switch host {
		case "blocked.example":
			return []net.IPAddr{{IP: net.ParseIP("10.0.0.1")}}, nil
		case "ok.example":
			return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
		case "empty.example":
			return nil, nil // zero addresses, no error
		case "fail.example":
			return nil, errors.New("dns error")
		default:
			return nil, errors.New("unknown host")
		}
	}
	dial := ssrfDialContext(mockDial, mockLookup)

	// 1. SplitHostPort error: address without a port.
	_, err := dial(ctx, "tcp", "noport")
	if err == nil || !strings.Contains(err.Error(), "ssrf: invalid address") {
		t.Errorf("case 1 (invalid address): got %v", err)
	}

	// 2. Fast path: numeric IP that is blocked.
	_, err = dial(ctx, "tcp", "10.0.0.1:80")
	if err == nil || !strings.Contains(err.Error(), "ssrf:") {
		t.Errorf("case 2 (blocked IP): got %v", err)
	}

	// 3. Fast path: numeric IP that is allowed → rawDial is called.
	lastDialedAddr = ""
	_, err = dial(ctx, "tcp", "8.8.8.8:80")
	if err == nil || err.Error() != "mock-dial" {
		t.Errorf("case 3 (allowed IP): got %v", err)
	}
	if lastDialedAddr != "8.8.8.8:80" {
		t.Errorf("case 3: expected dial to 8.8.8.8:80, got %q", lastDialedAddr)
	}

	// 4. Hostname path: DNS lookup fails.
	_, err = dial(ctx, "tcp", "fail.example:80")
	if err == nil || !strings.Contains(err.Error(), "ssrf: failed to resolve") {
		t.Errorf("case 4 (DNS error): got %v", err)
	}

	// 5. Hostname path: zero addresses resolved (empty slice, nil error).
	_, err = dial(ctx, "tcp", "empty.example:80")
	if err == nil || !strings.Contains(err.Error(), "ssrf: no addresses resolved") {
		t.Errorf("case 5 (no addrs): got %v", err)
	}

	// 6. Hostname path: resolved IP is blocked.
	_, err = dial(ctx, "tcp", "blocked.example:80")
	if err == nil || !strings.Contains(err.Error(), "ssrf:") {
		t.Errorf("case 6 (blocked hostname): got %v", err)
	}

	// 7. Hostname path: resolved IP is allowed → rawDial receives the resolved IP:port.
	lastDialedAddr = ""
	_, err = dial(ctx, "tcp", "ok.example:80")
	if err == nil || err.Error() != "mock-dial" {
		t.Errorf("case 7 (allowed hostname): got %v", err)
	}
	if lastDialedAddr != "8.8.8.8:80" {
		t.Errorf("case 7: expected dial to 8.8.8.8:80, got %q", lastDialedAddr)
	}
}

// TestNewSSRFSafeClientTimeoutFallback verifies that a zero/negative timeout
// is replaced with DefaultTimeout.
func TestNewSSRFSafeClientTimeoutFallback(t *testing.T) {
	t.Parallel()
	c := newSSRFSafeClient(0)
	if c.Timeout != DefaultTimeout {
		t.Fatalf("timeout = %v, want %v", c.Timeout, DefaultTimeout)
	}
}

// TestNewSSRFSafeClientCheckRedirect verifies the hop-count limit on redirects.
func TestNewSSRFSafeClientCheckRedirect(t *testing.T) {
	t.Parallel()
	c := newSSRFSafeClient(DefaultTimeout)
	if c.CheckRedirect == nil {
		t.Fatal("CheckRedirect must be set")
	}
	// Fewer than 10 hops: allowed.
	if err := c.CheckRedirect(nil, make([]*http.Request, 5)); err != nil {
		t.Fatalf("unexpected error for 5 redirects: %v", err)
	}
	// Exactly 10 hops: rejected.
	if err := c.CheckRedirect(nil, make([]*http.Request, 10)); err == nil {
		t.Fatal("expected error for 10 redirects, got nil")
	}
}

// TestDefaultLoaderBlocksLoopbackHTTP verifies that the default loader's
// SSRF-safe HTTP client rejects requests to loopback addresses end-to-end.
func TestDefaultLoaderBlocksLoopbackHTTP(t *testing.T) {
	t.Parallel()
	_, err := NewLoader().Load(context.Background(), brainapi.SourceRef{URI: "http://127.0.0.1:65530/"}, nil)
	if err == nil {
		t.Fatal("expected error for loopback URI, got nil")
	}
	if !brainapi.IsKind(err, brainapi.KindInternal) {
		t.Fatalf("kind=%q err=%v", brainapi.KindOf(err), err)
	}
}

// TestLoadHTTPUsesSSRFSafeClientWhenClientIsNil verifies that loadHTTP creates
// an SSRF-safe client when Loader.HTTPClient is nil rather than using the global
// http.DefaultClient.
func TestLoadHTTPUsesSSRFSafeClientWhenClientIsNil(t *testing.T) {
	t.Parallel()
	_, err := (&Loader{}).loadHTTP(context.Background(), LoadedSource{
		Source: brainapi.SourceRef{URI: "http://127.0.0.1:1/"},
	})
	if err == nil {
		t.Fatal("expected error from SSRF-safe client for loopback address")
	}
	if !brainapi.IsKind(err, brainapi.KindInternal) {
		t.Fatalf("kind=%q err=%v", brainapi.KindOf(err), err)
	}
}

// ── FIX 2: file:// restriction tests ─────────────────────────────────────────

// TestFileLoadRejectsWhenNoDirConfigured verifies that file:// is denied when
// FileBaseDir is empty (the default), preventing unrestricted local-file reads.
func TestFileLoadRejectsWhenNoDirConfigured(t *testing.T) {
	t.Parallel()
	_, err := NewLoader().Load(context.Background(), brainapi.SourceRef{URI: "file:///etc/passwd"}, nil)
	if !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("kind=%q err=%v", brainapi.KindOf(err), err)
	}
}

// TestFileLoadEtcPasswdRejected verifies that /etc/passwd is rejected even when
// FileBaseDir is configured (phase-1 lexical containment check).
func TestFileLoadEtcPasswdRejected(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_, err := NewLoader(WithFileBaseDir(dir)).Load(context.Background(), brainapi.SourceRef{URI: "file:///etc/passwd"}, nil)
	if !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("kind=%q err=%v", brainapi.KindOf(err), err)
	}
}

// TestFileLoadRejectsOutsideBaseDir verifies that a path-traversal attempt
// reaching outside FileBaseDir is blocked by the phase-1 lexical check.
func TestFileLoadRejectsOutsideBaseDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	parent := filepath.Dir(dir)
	escapedURI := "file://" + filepath.Join(parent, "secret.txt")
	_, err := NewLoader(WithFileBaseDir(dir)).Load(context.Background(), brainapi.SourceRef{URI: escapedURI}, nil)
	if !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("kind=%q err=%v", brainapi.KindOf(err), err)
	}
}

// TestLoadFileWithBaseDirRejectsNonLocalHost verifies that fileURLPath's
// non-local-host error is propagated correctly when FileBaseDir is configured.
func TestLoadFileWithBaseDirRejectsNonLocalHost(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_, err := NewLoader(WithFileBaseDir(dir)).Load(context.Background(), brainapi.SourceRef{URI: "file://remote.example/tmp/a.txt"}, nil)
	if !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("kind=%q err=%v", brainapi.KindOf(err), err)
	}
}

// TestFileURLPathBranches directly covers the two fileURLPath branches that are
// only reachable when a FileBaseDir is configured (loadFile exits early otherwise).
func TestFileURLPathBranches(t *testing.T) {
	t.Parallel()
	// Non-local host.
	_, err := fileURLPath(&url.URL{Host: "remote.example", Path: "/tmp/f.txt"})
	if !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Errorf("non-local host: kind=%q err=%v", brainapi.KindOf(err), err)
	}
	// Empty path.
	_, err = fileURLPath(&url.URL{Host: "localhost", Path: ""})
	if !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Errorf("empty path: kind=%q err=%v", brainapi.KindOf(err), err)
	}
}

// TestLoadFileRejectsInaccessibleBaseDir covers the filepath.EvalSymlinks error
// branch for l.FileBaseDir when the directory does not exist on disk.  Phase 1
// passes (URI is lexically inside the non-existent base) then phase 2 fails.
func TestLoadFileRejectsInaccessibleBaseDir(t *testing.T) {
	t.Parallel()
	baseDir := filepath.Join(t.TempDir(), "nonexistent-sub")
	// URI must be lexically inside baseDir so that phase-1 passes and we reach EvalSymlinks.
	uri := "file://" + filepath.Join(baseDir, "file.txt")
	_, err := NewLoader(WithFileBaseDir(baseDir)).Load(context.Background(), brainapi.SourceRef{URI: uri}, nil)
	if !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("kind=%q err=%v", brainapi.KindOf(err), err)
	}
}

// TestLoadFileRejectsSymlinkEscapeOutsideBaseDir proves that a symlink placed
// INSIDE FileBaseDir that points to a file OUTSIDE it is blocked by the
// phase-2 symlink-resolved containment check.
func TestLoadFileRejectsSymlinkEscapeOutsideBaseDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Write the secret file in a separate temp dir (outside dir).
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("outside content"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// Create a symlink INSIDE dir that points to the secret file OUTSIDE dir.
	link := filepath.Join(dir, "escape.txt")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlinks unavailable on this platform: %v", err)
	}
	_, err := NewLoader(WithFileBaseDir(dir)).Load(context.Background(), brainapi.SourceRef{URI: "file://" + link}, nil)
	if !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("symlink escape not caught: kind=%q err=%v", brainapi.KindOf(err), err)
	}
}

// ── FIX 3: non-regular file rejection ────────────────────────────────────────

// TestLoadFileRejectsNonRegularFile verifies that a FIFO (named pipe) is
// rejected before os.Open is called, preventing an indefinite hang.
// Skipped on platforms where the mkfifo command is unavailable.
func TestLoadFileRejectsNonRegularFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fifoPath := filepath.Join(dir, "test.fifo")
	if err := exec.Command("mkfifo", fifoPath).Run(); err != nil {
		t.Skipf("mkfifo unavailable on this platform: %v", err)
	}
	_, err := NewLoader(WithFileBaseDir(dir)).Load(context.Background(), brainapi.SourceRef{URI: "file://" + fifoPath}, nil)
	if !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("kind=%q err=%v", brainapi.KindOf(err), err)
	}
}

// TestLoadFileStatErrorAfterEvalSymlinks covers the osStat error branch that
// fires after EvalSymlinks succeeds by replacing osStat with a stub.
// This test is not parallel because it temporarily modifies a package-level var.
func TestLoadFileStatErrorAfterEvalSymlinks(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(p, []byte("content"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	orig := osStat
	defer func() { osStat = orig }()
	osStat = func(string) (os.FileInfo, error) {
		return nil, errors.New("simulated stat error")
	}
	_, err := NewLoader(WithFileBaseDir(dir)).Load(context.Background(), brainapi.SourceRef{URI: "file://" + p}, nil)
	if !brainapi.IsKind(err, brainapi.KindNotFound) {
		t.Fatalf("kind=%q err=%v", brainapi.KindOf(err), err)
	}
}

// TestLoadFileOpenErrorAfterStatSucceeds covers the osOpenFile error branch that
// fires after a successful osStat by replacing osOpenFile with a stub.
// This test is not parallel because it temporarily modifies a package-level var.
func TestLoadFileOpenErrorAfterStatSucceeds(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(p, []byte("content"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	orig := osOpenFile
	defer func() { osOpenFile = orig }()
	osOpenFile = func(string, int, os.FileMode) (*os.File, error) {
		return nil, errors.New("simulated open error")
	}
	_, err := NewLoader(WithFileBaseDir(dir)).Load(context.Background(), brainapi.SourceRef{URI: "file://" + p}, nil)
	if !brainapi.IsKind(err, brainapi.KindNotFound) {
		t.Fatalf("kind=%q err=%v", brainapi.KindOf(err), err)
	}
}

// ── FIX 3: withOptionalTimeout caps parent deadline when ours is tighter ─────

// TestWithOptionalTimeoutCapsTighterThanParent verifies the new behaviour:
// when our timeout is shorter than an existing parent deadline the returned
// context is replaced with the tighter deadline.
func TestWithOptionalTimeoutCapsTighterThanParent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	// Parent has 1-hour deadline; our timeout is 1 second → we should cap it.
	longCtx, cancelLong := context.WithDeadline(ctx, time.Now().Add(time.Hour))
	defer cancelLong()
	capped, cancelCapped := withOptionalTimeout(longCtx, time.Second)
	defer cancelCapped()
	if capped == longCtx {
		t.Fatal("expected a new capped context, got the original long-deadline context")
	}
	d, ok := capped.Deadline()
	if !ok {
		t.Fatal("capped context must have a deadline")
	}
	if remaining := time.Until(d); remaining > 2*time.Second || remaining < 0 {
		t.Fatalf("capped deadline remaining = %v, want ~1s", remaining)
	}
}

// ── Existing tests (updated where needed) ────────────────────────────────────

func TestLoaderLoadsFileAndEnrichesMetadata(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(path, []byte("alpha beta"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// WithFileBaseDir is required; file:// is denied without it.
	loaded, err := NewLoader(WithFileBaseDir(dir)).Load(context.Background(), brainapi.SourceRef{URI: "file://" + path}, map[string]string{"keep": "yes"})
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
	// A missing file within the allowed directory must return KindNotFound.
	dir := t.TempDir()
	_, err := NewLoader(WithFileBaseDir(dir)).Load(context.Background(), brainapi.SourceRef{URI: "file://" + filepath.Join(dir, "definitely-missing.txt")}, nil)
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
	// Inject a plain client: the SSRF-safe default blocks loopback test servers.
	_, err := testHTTPLoader(WithMaxBytes(3)).Load(context.Background(), brainapi.SourceRef{URI: server.URL + "/doc.txt"}, nil)
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
	// The default client must NOT be the global http.DefaultClient (SSRF risk).
	if loader.HTTPClient == nil {
		t.Fatal("default HTTPClient must not be nil")
	}
	if loader.HTTPClient == http.DefaultClient {
		t.Fatal("default HTTPClient must not be http.DefaultClient (SSRF risk)")
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
	// Inject a plain client: SSRF-safe default blocks loopback test servers.
	loaded, err := testHTTPLoader().Load(context.Background(), brainapi.SourceRef{URI: server.URL + "/path-name.txt"}, nil)
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
	_, err := testHTTPLoader().Load(context.Background(), brainapi.SourceRef{URI: server.URL}, nil)
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
	_, err := testHTTPLoader().Load(context.Background(), brainapi.SourceRef{URI: server.URL}, nil)
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
	loaded, err := testHTTPLoader().Load(context.Background(), brainapi.SourceRef{URI: server.URL + "/server.txt", Name: "given", MimeType: "application/custom"}, nil)
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
	loaded, err := NewLoader(WithFileBaseDir(dir)).Load(context.Background(), brainapi.SourceRef{URI: "file://localhost" + path}, nil)
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
	loaded, err := NewLoader(WithFileBaseDir(dir)).Load(context.Background(), brainapi.SourceRef{URI: "file://" + path, Name: "given", MimeType: "application/custom"}, nil)
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

	// Zero timeout: context must be returned unchanged.
	ctxNoTimeout, cancelNoTimeout := withOptionalTimeout(ctx, 0)
	defer cancelNoTimeout()
	if ctxNoTimeout != ctx {
		t.Fatalf("zero timeout replaced context")
	}

	// Parent deadline tighter than our timeout: parent must be kept unchanged.
	tightCtx, cancelTight := context.WithDeadline(ctx, time.Now().Add(time.Millisecond))
	defer cancelTight()
	ctxKept, cancelKept := withOptionalTimeout(tightCtx, time.Hour)
	defer cancelKept()
	if ctxKept != tightCtx {
		t.Fatal("tighter parent deadline should not be replaced by a longer timeout")
	}

	// No deadline: timeout must be applied.
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
	_, err := NewLoader(WithFileBaseDir(dir)).Load(context.Background(), brainapi.SourceRef{URI: "file://" + path}, nil)
	if !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("kind=%q err=%v", brainapi.KindOf(err), err)
	}
}

func TestLoadHTTPRejectsInvalidRequestURI(t *testing.T) {
	t.Parallel()
	_, err := NewLoader().loadHTTP(context.Background(), LoadedSource{Source: brainapi.SourceRef{URI: "http://%zz"}})
	if !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("kind=%q err=%v", brainapi.KindOf(err), err)
	}
}
