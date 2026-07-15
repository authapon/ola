// api_request_test.go - tests for the api_request tool (api_request.go).
// Kept in its own file rather than folded into unit_test.go/
// integration_test.go, mirroring how platform_linux.go/platform_other.go
// get their own files for a self-contained feature.

package main

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testAPIRequestConfig builds an apiRequestConfig with a single "test"
// endpoint pointed at srv, mirroring testSCPConfig's shape in unit_test.go.
func testAPIRequestConfig(srv *httptest.Server) apiRequestConfig {
	return apiRequestConfig{
		Endpoints:        map[string]apiEndpoint{"test": {Alias: "test", BaseURL: srv.URL}},
		EndpointOrder:    []string{"test"},
		Timeout:          defaultAPIRequestTimeoutSec * 1e9,
		MaxRequestBytes:  defaultAPIRequestMaxBodyBytes,
		MaxResponseBytes: defaultAPIRequestMaxResponseBytes,
	}
}

func TestToolAPIRequestDisabledWithEmptyConfig(t *testing.T) {
	if _, err := toolAPIRequest(map[string]interface{}{"endpoint": "test", "path": "/x"}, apiRequestConfig{}); err == nil {
		t.Fatal("expected an error when api_request is not configured")
	}
}

func TestToolAPIRequestRejectsUnknownEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	cfg := testAPIRequestConfig(srv)

	_, err := toolAPIRequest(map[string]interface{}{"endpoint": "not-configured", "path": "/x"}, cfg)
	if err == nil {
		t.Fatal("expected an unknown endpoint alias to be rejected")
	}
	if !strings.Contains(err.Error(), "test") {
		t.Fatalf("expected the error to list the allowed endpoint(s), got: %v", err)
	}
}

func TestToolAPIRequestRejectsBothEndpointAndURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	cfg := testAPIRequestConfig(srv)
	cfg.AllowDirectURL = true

	args := map[string]interface{}{"endpoint": "test", "path": "/x", "url": "https://example.com"}
	if _, err := toolAPIRequest(args, cfg); err == nil {
		t.Fatal("expected specifying both endpoint and url to be rejected")
	}
}

func TestToolAPIRequestRequiresEndpointOrURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	cfg := testAPIRequestConfig(srv)

	if _, err := toolAPIRequest(map[string]interface{}{}, cfg); err == nil {
		t.Fatal("expected omitting both endpoint and url to be rejected")
	}
}

func TestToolAPIRequestEndpointModeGETWithQuery(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.Query().Get("q")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	cfg := testAPIRequestConfig(srv)

	args := map[string]interface{}{
		"endpoint": "test", "path": "/api/tags",
		"query": map[string]interface{}{"q": "hello"},
	}
	result, err := toolAPIRequest(args, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotPath != "/api/tags" {
		t.Fatalf("expected path /api/tags, got %q", gotPath)
	}
	if gotQuery != "hello" {
		t.Fatalf("expected query q=hello, got %q", gotQuery)
	}
	if !strings.Contains(result, "HTTP 200") || !strings.Contains(result, `"ok":true`) {
		t.Fatalf("expected result to include status and body, got: %s", result)
	}
}

func TestToolAPIRequestDefaultMethodIsGET(t *testing.T) {
	var gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
	}))
	defer srv.Close()
	cfg := testAPIRequestConfig(srv)

	if _, err := toolAPIRequest(map[string]interface{}{"endpoint": "test", "path": "/"}, cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotMethod != http.MethodGet {
		t.Fatalf("expected default method GET, got %q", gotMethod)
	}
}

func TestToolAPIRequestRejectsMutatingMethodByDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("server should never be hit - method should be rejected before any request is sent")
	}))
	defer srv.Close()
	cfg := testAPIRequestConfig(srv) // AllowMutating defaults to false

	args := map[string]interface{}{"endpoint": "test", "path": "/x", "method": "POST"}
	if _, err := toolAPIRequest(args, cfg); err == nil {
		t.Fatal("expected POST to be rejected when AllowMutating is false")
	}
}

func TestToolAPIRequestRejectsUnsupportedMethod(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("server should never be hit")
	}))
	defer srv.Close()
	cfg := testAPIRequestConfig(srv)
	cfg.AllowMutating = true

	args := map[string]interface{}{"endpoint": "test", "path": "/x", "method": "TRACE"}
	if _, err := toolAPIRequest(args, cfg); err == nil {
		t.Fatal("expected an unsupported method (TRACE) to be rejected even with AllowMutating on")
	}
}

func TestToolAPIRequestAllowsMutatingWhenEnabled(t *testing.T) {
	var gotMethod, gotBody, gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotContentType = r.Header.Get("Content-Type")
		buf := make([]byte, 1024)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	cfg := testAPIRequestConfig(srv)
	cfg.AllowMutating = true

	args := map[string]interface{}{
		"endpoint": "test", "path": "/create", "method": "POST",
		"body_type": "json", "body": map[string]interface{}{"name": "moo"},
	}
	result, err := toolAPIRequest(args, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("expected POST, got %q", gotMethod)
	}
	if gotContentType != "application/json" {
		t.Fatalf("expected application/json content-type, got %q", gotContentType)
	}
	if !strings.Contains(gotBody, `"name":"moo"`) {
		t.Fatalf("expected JSON body to contain name=moo, got: %s", gotBody)
	}
	if !strings.Contains(result, "HTTP 201") {
		t.Fatalf("expected result to report HTTP 201, got: %s", result)
	}
}

func TestToolAPIRequestNon2xxIsNotAGoError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"bad input"}`))
	}))
	defer srv.Close()
	cfg := testAPIRequestConfig(srv)

	result, err := toolAPIRequest(map[string]interface{}{"endpoint": "test", "path": "/x"}, cfg)
	if err != nil {
		t.Fatalf("expected a 4xx response to NOT be a Go error, got: %v", err)
	}
	if !strings.Contains(result, "HTTP 400") || !strings.Contains(result, "bad input") {
		t.Fatalf("expected result to surface the 400 status and body, got: %s", result)
	}
}

func TestToolAPIRequestFormBody(t *testing.T) {
	var gotBody, gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		buf := make([]byte, 1024)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
	}))
	defer srv.Close()
	cfg := testAPIRequestConfig(srv)
	cfg.AllowMutating = true

	args := map[string]interface{}{
		"endpoint": "test", "path": "/x", "method": "POST",
		"body_type": "form", "body": map[string]interface{}{"a": "1", "b": "two"},
	}
	if _, err := toolAPIRequest(args, cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotContentType != "application/x-www-form-urlencoded" {
		t.Fatalf("expected form content-type, got %q", gotContentType)
	}
	if !strings.Contains(gotBody, "a=1") || !strings.Contains(gotBody, "b=two") {
		t.Fatalf("expected form-encoded body, got: %s", gotBody)
	}
}

func TestToolAPIRequestTextBody(t *testing.T) {
	var gotBody, gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		buf := make([]byte, 1024)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
	}))
	defer srv.Close()
	cfg := testAPIRequestConfig(srv)
	cfg.AllowMutating = true

	args := map[string]interface{}{
		"endpoint": "test", "path": "/x", "method": "PUT",
		"body_type": "text", "body": "hello plain text",
	}
	if _, err := toolAPIRequest(args, cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(gotContentType, "text/plain") {
		t.Fatalf("expected text/plain content-type, got %q", gotContentType)
	}
	if gotBody != "hello plain text" {
		t.Fatalf("expected raw text body, got: %s", gotBody)
	}
}

func TestToolAPIRequestBinaryBody(t *testing.T) {
	var gotBody []byte
	var gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		buf := make([]byte, 1024)
		n, _ := r.Body.Read(buf)
		gotBody = buf[:n]
	}))
	defer srv.Close()
	cfg := testAPIRequestConfig(srv)
	cfg.AllowMutating = true

	raw := []byte{0x00, 0x01, 0xFF, 0x10}
	args := map[string]interface{}{
		"endpoint": "test", "path": "/x", "method": "POST",
		"body_type": "binary", "body": base64.StdEncoding.EncodeToString(raw),
	}
	if _, err := toolAPIRequest(args, cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotContentType != "application/octet-stream" {
		t.Fatalf("expected application/octet-stream content-type, got %q", gotContentType)
	}
	if string(gotBody) != string(raw) {
		t.Fatalf("expected raw bytes to round-trip, got: %v want: %v", gotBody, raw)
	}
}

func TestToolAPIRequestMultipartBody(t *testing.T) {
	dir := t.TempDir()
	origWD, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origWD)

	if err := os.WriteFile(filepath.Join(dir, "attach.txt"), []byte("file contents"), 0644); err != nil {
		t.Fatal(err)
	}

	var gotContentType string
	var gotFieldValue, gotFileContent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("server failed to parse multipart form: %v", err)
			return
		}
		gotFieldValue = r.FormValue("note")
		f, _, err := r.FormFile("file0")
		if err != nil {
			t.Errorf("server failed to read file0: %v", err)
			return
		}
		defer f.Close()
		buf := make([]byte, 1024)
		n, _ := f.Read(buf)
		gotFileContent = string(buf[:n])
	}))
	defer srv.Close()
	cfg := testAPIRequestConfig(srv)
	cfg.AllowMutating = true

	args := map[string]interface{}{
		"endpoint": "test", "path": "/upload", "method": "POST",
		"body_type":       "multipart",
		"body":            map[string]interface{}{"note": "hi"},
		"multipart_files": []interface{}{"attach.txt"},
	}
	if _, err := toolAPIRequest(args, cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(gotContentType, "multipart/form-data") {
		t.Fatalf("expected multipart/form-data content-type, got %q", gotContentType)
	}
	if gotFieldValue != "hi" {
		t.Fatalf("expected field note=hi, got %q", gotFieldValue)
	}
	if gotFileContent != "file contents" {
		t.Fatalf("expected attached file contents to round-trip, got %q", gotFileContent)
	}
}

func TestToolAPIRequestMultipartRejectsPathEscape(t *testing.T) {
	dir := t.TempDir()
	origWD, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origWD)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("server should never be hit - path escape should be rejected before sending")
	}))
	defer srv.Close()
	cfg := testAPIRequestConfig(srv)
	cfg.AllowMutating = true

	args := map[string]interface{}{
		"endpoint": "test", "path": "/upload", "method": "POST",
		"body_type":       "multipart",
		"multipart_files": []interface{}{"../../etc/passwd"},
	}
	if _, err := toolAPIRequest(args, cfg); err == nil {
		t.Fatal("expected a multipart_files path escaping the sandbox to be rejected")
	}
}

func TestToolAPIRequestReservedHeadersAreFilteredAndEndpointAuthWins(t *testing.T) {
	var gotAuth, gotHost, gotCustom string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotHost = r.Host
		gotCustom = r.Header.Get("X-Custom")
	}))
	defer srv.Close()
	cfg := testAPIRequestConfig(srv)
	ep := cfg.Endpoints["test"]
	ep.AuthHeader = "Authorization"
	ep.AuthValue = "Bearer secret-token"
	cfg.Endpoints["test"] = ep

	args := map[string]interface{}{
		"endpoint": "test", "path": "/x",
		"headers": map[string]interface{}{
			"Authorization": "Bearer model-supplied-should-be-ignored",
			"Host":          "evil.example",
			"X-Custom":      "hello",
		},
	}
	if _, err := toolAPIRequest(args, cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotAuth != "Bearer secret-token" {
		t.Fatalf("expected endpoint's own auth to win, got Authorization=%q", gotAuth)
	}
	if strings.Contains(gotHost, "evil.example") {
		t.Fatalf("expected model-supplied Host header to be ignored, got Host=%q", gotHost)
	}
	if gotCustom != "hello" {
		t.Fatalf("expected non-reserved custom header to pass through, got %q", gotCustom)
	}
}

func TestToolAPIRequestDirectURLDisabledByDefault(t *testing.T) {
	cfg := apiRequestConfig{
		Endpoints: map[string]apiEndpoint{"test": {Alias: "test", BaseURL: "http://example.invalid"}},
	}
	args := map[string]interface{}{"url": "https://example.com"}
	if _, err := toolAPIRequest(args, cfg); err == nil {
		t.Fatal("expected direct-URL mode to be rejected when AllowDirectURL is false")
	}
}

func TestToolAPIRequestDirectURLRejectsPrivateAndLocalURLs(t *testing.T) {
	cfg := apiRequestConfig{AllowDirectURL: true, Timeout: defaultAPIRequestTimeoutSec * 1e9, MaxResponseBytes: defaultAPIRequestMaxResponseBytes}
	cases := []string{
		"http://localhost:11434/api/tags",
		"http://127.0.0.1:8080/admin",
		"http://169.254.169.254/latest/meta-data/",
		"ftp://example.com/file",
	}
	for _, u := range cases {
		if _, err := toolAPIRequest(map[string]interface{}{"url": u}, cfg); err == nil {
			t.Fatalf("expected %q to be rejected by the SSRF guard", u)
		}
	}
}

// redirectAllTransportAPI mirrors redirectAllTransport in unit_test.go
// (web_fetch's own parallel-fetch test) - a test-only RoundTripper that
// dials target no matter what host/port the request was addressed to, so
// a direct-mode call to a fake public hostname can land on a local
// httptest.Server without tripping the SSRF guard (which only rejects
// obviously-local/private hosts, not made-up public-looking ones).
type redirectAllTransportAPI struct {
	target string
	base   http.Transport
}

func (rt *redirectAllTransportAPI) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Host = rt.target
	return rt.base.RoundTrip(req)
}

func TestToolAPIRequestDirectURLAllowsPublicHTTPS(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	origTransport := http.DefaultTransport
	http.DefaultTransport = &redirectAllTransportAPI{target: srv.Listener.Addr().String()}
	defer func() { http.DefaultTransport = origTransport }()

	cfg := apiRequestConfig{AllowDirectURL: true, Timeout: defaultAPIRequestTimeoutSec * 1e9, MaxResponseBytes: defaultAPIRequestMaxResponseBytes}
	result, err := toolAPIRequest(map[string]interface{}{"url": "http://public.example.invalid/hello"}, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "ok") {
		t.Fatalf("expected response body to come through, got: %s", result)
	}
}

func TestJoinEndpointPathIgnoresHostInPath(t *testing.T) {
	// A path that looks like a full URL (with its own scheme/host) must
	// never redirect the request away from the endpoint's own base host -
	// only its Path/RawQuery should ever be used.
	full, err := joinEndpointPath("https://trusted.example/api", "http://evil.example/steal?x=1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	u, err := url.Parse(full)
	if err != nil {
		t.Fatal(err)
	}
	if u.Host != "trusted.example" {
		t.Fatalf("expected host to stay trusted.example, got %q (full: %s)", u.Host, full)
	}
	if u.Path != "/api/steal" {
		t.Fatalf("expected joined path /api/steal, got %q (full: %s)", u.Path, full)
	}
}

func TestSingleJoiningSlash(t *testing.T) {
	cases := []struct{ a, b, want string }{
		{"/api", "/x", "/api/x"},
		{"/api/", "/x", "/api/x"},
		{"/api", "x", "/api/x"},
		{"/api/", "x", "/api/x"},
		{"/api", "", "/api"},
	}
	for _, c := range cases {
		if got := singleJoiningSlash(c.a, c.b); got != c.want {
			t.Errorf("singleJoiningSlash(%q, %q) = %q, want %q", c.a, c.b, got, c.want)
		}
	}
}

func TestFormatAPIResponseTruncatesLongTextBody(t *testing.T) {
	resp := &http.Response{Header: http.Header{"Content-Type": []string{"text/plain"}}, StatusCode: 200}
	long := strings.Repeat("x", maxAPIResultOutput+500)
	out := formatAPIResponse(resp, []byte(long))
	if len(out) >= len(long) {
		t.Fatalf("expected long text body to be truncated, output was %d bytes (input %d bytes)", len(out), len(long))
	}
}

func TestFormatAPIResponseHidesBinaryBody(t *testing.T) {
	resp := &http.Response{Header: http.Header{"Content-Type": []string{"image/png"}}, StatusCode: 200}
	binary := []byte{0x89, 0x50, 0x4E, 0x47, 0x00, 0x01, 0x02}
	out := formatAPIResponse(resp, binary)
	if strings.Contains(out, string(binary)) {
		t.Fatal("expected binary body to never be echoed back verbatim")
	}
	if !strings.Contains(out, "binary") {
		t.Fatalf("expected a note that the body is binary and hidden, got: %s", out)
	}
}

func TestResolveAPIRequestConfigParsesEndpointsAndAuth(t *testing.T) {
	os.Setenv("OLA_API_ENDPOINT_OLLAMA_AUTH_HEADER", "Authorization")
	os.Setenv("OLA_API_ENDPOINT_OLLAMA_AUTH_VALUE", "Bearer xyz")
	defer os.Unsetenv("OLA_API_ENDPOINT_OLLAMA_AUTH_HEADER")
	defer os.Unsetenv("OLA_API_ENDPOINT_OLLAMA_AUTH_VALUE")

	cfg, warnings := resolveAPIRequestConfig("ollama=http://localhost:11434,bad-entry,openwebui=http://localhost:8080", false, false, 0, 0, 0)
	if len(warnings) != 1 {
		t.Fatalf("expected exactly 1 warning for the malformed entry, got %d: %v", len(warnings), warnings)
	}
	if !cfg.enabled() {
		t.Fatal("expected config with endpoints to be enabled")
	}
	ollama, ok := cfg.Endpoints["ollama"]
	if !ok {
		t.Fatal("expected 'ollama' endpoint to be parsed")
	}
	if ollama.BaseURL != "http://localhost:11434" {
		t.Fatalf("unexpected BaseURL: %q", ollama.BaseURL)
	}
	if ollama.AuthHeader != "Authorization" || ollama.AuthValue != "Bearer xyz" {
		t.Fatalf("expected auth header/value to be picked up from env, got %q/%q", ollama.AuthHeader, ollama.AuthValue)
	}
	if _, ok := cfg.Endpoints["openwebui"]; !ok {
		t.Fatal("expected 'openwebui' endpoint to be parsed despite the earlier bad entry")
	}
}

func TestResolveAPIRequestConfigDisabledWhenNothingSet(t *testing.T) {
	cfg, warnings := resolveAPIRequestConfig("", false, false, 0, 0, 0)
	if len(warnings) != 0 {
		t.Fatalf("expected no warnings, got: %v", warnings)
	}
	if cfg.enabled() {
		t.Fatal("expected config to be disabled when nothing was configured")
	}
}

func TestAPIRequestToolNotOfferedWhenDisabled_sanity(t *testing.T) {
	// Mirrors TestWebSearchToolNotOfferedWhenDisabled_sanity's shape: a
	// quick sanity check that the gating condition itself behaves as
	// expected, independent of the CLI wiring in main.go/cmdCoding.
	var cfg apiRequestConfig
	if cfg.enabled() {
		t.Fatal("expected zero-value apiRequestConfig to be disabled")
	}
}
