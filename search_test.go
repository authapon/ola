package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestHtmlToTextStripsScriptStyleTagsAndExtractsTitle(t *testing.T) {
	page := `<!DOCTYPE html>
<html><head><title>My Page &amp; Friends</title>
<style>body { color: red; }</style>
<script>alert('hi'); var x = "<not a real tag>";</script>
</head>
<body>
<nav>Home | About</nav>
<h1>Welcome</h1>
<p>This is a <b>paragraph</b> with <i>inline</i> tags.</p>
<!-- a comment that should vanish -->
<p>Second paragraph here.</p>
</body></html>`

	title, text := htmlToText(page)
	if title != "My Page & Friends" {
		t.Fatalf("expected title to be extracted and entity-unescaped, got %q", title)
	}
	if strings.Contains(text, "alert(") || strings.Contains(text, "color: red") {
		t.Fatalf("expected script/style contents to be stripped entirely, got: %s", text)
	}
	if strings.Contains(text, "a comment that should vanish") {
		t.Fatalf("expected HTML comments to be stripped, got: %s", text)
	}
	if strings.Contains(text, "<") || strings.Contains(text, ">") {
		t.Fatalf("expected all tags to be stripped, got: %s", text)
	}
	if !strings.Contains(text, "Welcome") || !strings.Contains(text, "paragraph") || !strings.Contains(text, "Second paragraph") {
		t.Fatalf("expected visible text to survive extraction, got: %s", text)
	}
}

// TestDoDirectFetchRunsInParallel confirms doDirectFetch is safe and fast
// to call concurrently (goroutine-safe, no shared mutable state) against a
// mock HTML server - mirroring the worker-pool pattern toolWebFetch itself
// uses (see TestToolWebFetchRunsURLsInParallel for the end-to-end version
// through toolWebFetch/fetchOneDirect). It calls doDirectFetch directly
// (not fetchOneDirect/toolWebFetch) because the mock server necessarily
// lives on loopback, which the production SSRF guard in fetchOneDirect
// correctly rejects as a fetch *target* - see
// TestToolWebFetchDirectModeRejectsPrivateURLs for that guard's own test.
func TestDoDirectFetchRunsInParallel(t *testing.T) {
	var hits int32
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		time.Sleep(150 * time.Millisecond)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, "<html><head><title>Page %s</title></head><body><p>content for %s</p></body></html>", r.URL.Path, r.URL.Path)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	paths := []string{"/a", "/b", "/c", "/d"}
	results := make([]string, len(paths))
	var wg sync.WaitGroup

	start := time.Now()
	for i, p := range paths {
		wg.Add(1)
		go func(idx int, path string) {
			defer wg.Done()
			r, err := doDirectFetch(client, srv.URL+path)
			if err != nil {
				t.Errorf("doDirectFetch(%s) failed: %v", path, err)
				return
			}
			results[idx] = r
		}(i, p)
	}
	wg.Wait()
	elapsed := time.Since(start)

	if atomic.LoadInt32(&hits) != 4 {
		t.Fatalf("expected 4 direct HTTP hits, got %d", hits)
	}
	if elapsed > 400*time.Millisecond {
		t.Fatalf("expected concurrent execution (~150ms), took %s - looks serial", elapsed)
	}
	for i, p := range paths {
		if !strings.Contains(results[i], "content for "+p) {
			t.Fatalf("expected extracted text for %q, got: %s", p, results[i])
		}
	}
}

func TestDoDirectFetchNonHTMLPassthrough(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/data.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"hello":"world"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	result, err := doDirectFetch(client, srv.URL+"/data.json")
	if err != nil {
		t.Fatalf("expected JSON content-type to pass through, got err: %v", err)
	}
	if !strings.Contains(result, `"hello":"world"`) {
		t.Fatalf("expected raw JSON body to be returned as-is, got: %s", result)
	}
}

// TestDoDirectFetchErrorsOnJSOnlyPages confirms a page whose body is
// essentially an empty shell (typical of a client-side-rendered SPA with no
// server-rendered content) produces a helpful, honest error saying
// web_fetch cannot execute JavaScript, rather than silently returning
// nothing useful or pointing at a scrape mode that no longer exists.
func TestDoDirectFetchErrorsOnJSOnlyPages(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><head><script src="/app.js"></script></head><body><div id="root"></div></body></html>`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	_, err := doDirectFetch(client, srv.URL)
	if err == nil {
		t.Fatal("expected an error for a page with no text content after stripping HTML")
	}
	if !strings.Contains(err.Error(), "JavaScript") {
		t.Fatalf("expected a hint about JS-rendered content, got: %v", err)
	}
}

func TestToolWebFetchDirectModeRejectsPrivateURLs(t *testing.T) {
	cfg := resolveSearchConfig("", 0, 0, 0, 0, 0, false)
	result, err := toolWebFetch(map[string]interface{}{"urls": []interface{}{"http://127.0.0.1:9/admin"}}, cfg)
	if err != nil {
		t.Fatalf("expected batch call to succeed with an ERROR slot, got err: %v", err)
	}
	if !strings.Contains(result, "ERROR") {
		t.Fatalf("expected the SSRF guard to reject a private-IP URL, got: %s", result)
	}
}

func TestResolveSearchConfigFetchEnabledByDefault(t *testing.T) {
	// web_fetch needs no configuration at all - it must be enabled the
	// instant a session isn't explicitly disabled with --no-web-search.
	cfg := resolveSearchConfig("", 0, 0, 0, 0, 0, false)
	if !cfg.fetchEnabled() {
		t.Fatal("expected fetchEnabled() true by default with no flags/env set at all")
	}
}

func TestResolveSearchConfigFlagOverridesEnv(t *testing.T) {
	os.Setenv("OLA_SEARXNG_API_BASE", "http://env-searxng:8080")
	os.Setenv("OLA_SEARCH_MAX_RESULTS", "9")
	defer os.Unsetenv("OLA_SEARXNG_API_BASE")
	defer os.Unsetenv("OLA_SEARCH_MAX_RESULTS")

	cfg := resolveSearchConfig("http://flag-searxng:9000", 0, 0, 0, 0, 0, false)
	if cfg.SearXNGBase != "http://flag-searxng:9000" {
		t.Fatalf("expected flag to win over env, got %q", cfg.SearXNGBase)
	}
	if cfg.MaxResults != 9 {
		t.Fatalf("expected env fallback for max results, got %d", cfg.MaxResults)
	}
	if !cfg.searchEnabled() {
		t.Fatal("expected searchEnabled() true when SearXNGBase is set")
	}
	if !cfg.fetchEnabled() {
		t.Fatal("expected fetchEnabled() true regardless of SearXNG configuration (web_fetch is always-on)")
	}
}

func TestResolveSearchConfigDisableWins(t *testing.T) {
	os.Setenv("OLA_SEARXNG_API_BASE", "http://env-searxng:8080")
	defer os.Unsetenv("OLA_SEARXNG_API_BASE")

	cfg := resolveSearchConfig("", 0, 0, 0, 0, 0, true /* --no-web-search */)
	if cfg.searchEnabled() || cfg.fetchEnabled() {
		t.Fatalf("expected --no-web-search to force both tools off, got %+v", cfg)
	}
}

func TestResolveSearchConfigDefaults(t *testing.T) {
	cfg := resolveSearchConfig("http://x:1", 0, 0, 0, 0, 0, false)
	if cfg.MaxResults != defaultSearchMaxResults {
		t.Fatalf("expected default max results %d, got %d", defaultSearchMaxResults, cfg.MaxResults)
	}
	if cfg.SearchConcurrency != defaultSearchConcurrency {
		t.Fatalf("expected default search concurrency %d, got %d", defaultSearchConcurrency, cfg.SearchConcurrency)
	}
	if cfg.FetchConcurrency != defaultFetchConcurrency {
		t.Fatalf("expected default fetch concurrency %d, got %d", defaultFetchConcurrency, cfg.FetchConcurrency)
	}
}

// TestToolWebSearchRunsQueriesInParallel spins up a SearXNG-shaped mock that
// sleeps 150ms per request, fires 4 queries with concurrency=4, and asserts
// the whole batch finishes in well under 4*150ms - proving the fan-out is
// actually concurrent, not a serial loop with a concurrency knob that does
// nothing.
func TestToolWebSearchRunsQueriesInParallel(t *testing.T) {
	var hits int32
	mux := http.NewServeMux()
	mux.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		time.Sleep(150 * time.Millisecond)
		q := r.URL.Query().Get("q")
		resp := searxngResponse{Results: []searxngResult{
			{Title: "result for " + q, URL: "https://example.com/" + q, Content: "some content about " + q},
		}}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := resolveSearchConfig(srv.URL, 0, 4, 0, 5, 0, false)
	args := map[string]interface{}{
		"queries": []interface{}{"golang", "ollama", "searxng", "ripgrep"},
	}

	start := time.Now()
	result, err := toolWebSearch(args, cfg)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("toolWebSearch returned error: %v", err)
	}
	if atomic.LoadInt32(&hits) != 4 {
		t.Fatalf("expected 4 upstream hits, got %d", hits)
	}
	if elapsed > 400*time.Millisecond {
		t.Fatalf("expected concurrent fan-out (~150ms), took %s - looks serial", elapsed)
	}
	for _, q := range []string{"golang", "ollama", "searxng", "ripgrep"} {
		if !strings.Contains(result, q) {
			t.Fatalf("expected result to mention query %q, got: %s", q, result)
		}
	}
}

func TestToolWebSearchDisabledReturnsError(t *testing.T) {
	cfg := resolveSearchConfig("", 0, 0, 0, 0, 0, false)
	_, err := toolWebSearch(map[string]interface{}{"queries": []interface{}{"x"}}, cfg)
	if err == nil {
		t.Fatal("expected error when web_search is not configured")
	}
}

func TestToolWebSearchPartialFailureStillReturnsGoodResults(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		if q == "bad" {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("boom"))
			return
		}
		resp := searxngResponse{Results: []searxngResult{{Title: "ok", URL: "https://example.com", Content: "fine"}}}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := resolveSearchConfig(srv.URL, 0, 2, 0, 5, 0, false)
	result, err := toolWebSearch(map[string]interface{}{"queries": []interface{}{"good", "bad"}}, cfg)
	if err != nil {
		t.Fatalf("expected batch call itself to succeed even with one bad query, got err: %v", err)
	}
	if !strings.Contains(result, "ERROR") {
		t.Fatalf("expected the failing query's slot to carry an ERROR marker, got: %s", result)
	}
	if !strings.Contains(result, "ok") {
		t.Fatalf("expected the succeeding query's result to still be present, got: %s", result)
	}
}

// TestToolWebFetchRunsURLsInParallel mirrors the search concurrency test,
// but against a plain direct-mode HTML server (web_fetch's only mode).
//
// toolWebFetch's SSRF guard (validateFetchURL) rejects the target URL if it
// resolves to a loopback/private address, which a plain httptest.Server
// URL always does - so, unlike the old shim-mode version of this test
// (which only ever talked to a *local scrape service*, never the target
// URL itself), we can't just point straight at srv.URL. Instead: use URLs
// on the reserved, guaranteed-NXDOMAIN ".invalid" TLD (RFC 2606) - a failed
// DNS lookup makes validateFetchURL let the URL through (see its "DNS
// hiccup/offline" comment) - and swap in a RoundTripper that redirects the
// actual dial to the local test server regardless of the requested host,
// so the fetch still really happens end-to-end.
func TestToolWebFetchRunsURLsInParallel(t *testing.T) {
	var hits int32
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		time.Sleep(150 * time.Millisecond)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, "<html><body><p>content for %s</p></body></html>", r.URL.Path)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	origTransport := http.DefaultTransport
	http.DefaultTransport = &redirectAllTransport{target: srv.Listener.Addr().String()}
	defer func() { http.DefaultTransport = origTransport }()

	cfg := resolveSearchConfig("", 0, 0, 4, 0, 5, false)
	urls := []interface{}{
		"http://a.example.invalid/a", "http://b.example.invalid/b",
		"http://c.example.invalid/c", "http://d.example.invalid/d",
	}

	start := time.Now()
	result, err := toolWebFetch(map[string]interface{}{"urls": urls}, cfg)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("toolWebFetch returned error: %v", err)
	}
	if atomic.LoadInt32(&hits) != 4 {
		t.Fatalf("expected 4 upstream hits, got %d - result was: %s", hits, result)
	}
	if elapsed > 400*time.Millisecond {
		t.Fatalf("expected concurrent fan-out (~150ms), took %s - looks serial", elapsed)
	}
	for _, u := range []string{"/a", "/b", "/c", "/d"} {
		if !strings.Contains(result, "content for "+u) {
			t.Fatalf("expected result to mention content for %q, got: %s", u, result)
		}
	}
}

// redirectAllTransport is a test-only http.RoundTripper that dials target
// no matter what host/port the request was addressed to - used above to
// let a fetch to a fake, never-resolving hostname actually land on a local
// httptest.Server.
type redirectAllTransport struct {
	target string
	base   http.Transport
}

func (rt *redirectAllTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Host = rt.target
	return rt.base.RoundTrip(req)
}

func TestToolWebFetchRejectsPrivateAndLocalURLs(t *testing.T) {
	cfg := resolveSearchConfig("", 0, 0, 0, 0, 0, false)
	cases := []string{
		"http://localhost:11434/api/tags",
		"http://127.0.0.1:8080/admin",
		"http://169.254.169.254/latest/meta-data/",
		"ftp://example.com/file",
	}
	for _, u := range cases {
		result, err := toolWebFetch(map[string]interface{}{"urls": []interface{}{u}}, cfg)
		if err != nil {
			t.Fatalf("toolWebFetch batch call itself should not hard-fail for %q, got err: %v", u, err)
		}
		if !strings.Contains(result, "ERROR") {
			t.Fatalf("expected %q to be rejected by the SSRF guard, got: %s", u, result)
		}
	}
}

func TestValidateFetchURLAllowsPublicHTTPS(t *testing.T) {
	if err := validateFetchURL("https://example.com/some/page"); err != nil {
		t.Fatalf("expected a plain public https URL to be allowed, got: %v", err)
	}
}

func TestToolWebFetchDisabledReturnsError(t *testing.T) {
	cfg := resolveSearchConfig("", 0, 0, 0, 0, 0, true /* --no-web-search */)
	_, err := toolWebFetch(map[string]interface{}{"urls": []interface{}{"https://example.com"}}, cfg)
	if err == nil {
		t.Fatal("expected error when web_fetch has been explicitly disabled via --no-web-search")
	}
}

func TestTruncateText(t *testing.T) {
	short := "hello"
	if truncateText(short, 100) != short {
		t.Fatal("expected short text to pass through unchanged")
	}
	long := strings.Repeat("x", 100)
	got := truncateText(long, 10)
	if len(got) <= 10 {
		t.Fatal("expected truncation marker to be appended, making output longer than the limit")
	}
	if !strings.HasPrefix(got, strings.Repeat("x", 10)) {
		t.Fatalf("expected truncated output to start with the first 10 chars, got: %s", got)
	}
}

func TestWebSearchToolNotOfferedWhenDisabled_sanity(t *testing.T) {
	// Sanity check for the schema constants used by main.go/coding.go when
	// deciding whether to append these tools to a request's tool list.
	if webSearchTool.Function.Name != "web_search" {
		t.Fatalf("unexpected web_search tool name: %s", webSearchTool.Function.Name)
	}
	if webFetchTool.Function.Name != "web_fetch" {
		t.Fatalf("unexpected web_fetch tool name: %s", webFetchTool.Function.Name)
	}
}

// ─────────────────────────────────────────────────────────────────
// Ollama Web Search API backend (no self-hosted SearXNG required) +
// backend precedence + the terminal/log "found N results, here's every
// title+link" summary that rides on top of whichever backend actually ran.
// ─────────────────────────────────────────────────────────────────

func TestResolveOllamaSearchConfigFlagOverridesEnv(t *testing.T) {
	os.Setenv("OLA_OLLAMA_SEARCH_API_KEY", "env-key")
	os.Setenv("OLLAMA_API_KEY", "generic-env-key")
	os.Setenv("OLA_OLLAMA_SEARCH_API_BASE", "http://mock-ollama:1234")
	defer os.Unsetenv("OLA_OLLAMA_SEARCH_API_KEY")
	defer os.Unsetenv("OLLAMA_API_KEY")
	defer os.Unsetenv("OLA_OLLAMA_SEARCH_API_BASE")

	apiKey, base := resolveOllamaSearchConfig("flag-key")
	if apiKey != "flag-key" {
		t.Fatalf("expected --ollama-search-key flag to win over both env vars, got %q", apiKey)
	}
	if base != "http://mock-ollama:1234" {
		t.Fatalf("expected OLA_OLLAMA_SEARCH_API_BASE to override the default base, got %q", base)
	}
}

func TestResolveOllamaSearchConfigFallsBackToGenericOllamaAPIKeyEnv(t *testing.T) {
	os.Unsetenv("OLA_OLLAMA_SEARCH_API_KEY")
	os.Setenv("OLLAMA_API_KEY", "generic-env-key")
	defer os.Unsetenv("OLLAMA_API_KEY")

	// No flag, no ola-specific env var - must fall back to the standard
	// OLLAMA_API_KEY name that Ollama's own CLI/Python/JS libraries use, so
	// a machine already configured for `ollama.web_search` needs no
	// ola-specific setup at all.
	apiKey, base := resolveOllamaSearchConfig("")
	if apiKey != "generic-env-key" {
		t.Fatalf("expected fallback to $OLLAMA_API_KEY, got %q", apiKey)
	}
	if base != defaultOllamaSearchBase {
		t.Fatalf("expected default base %q when OLA_OLLAMA_SEARCH_API_BASE is unset, got %q", defaultOllamaSearchBase, base)
	}
}

func TestSearchConfigSearchEnabledViaOllamaKeyAlone(t *testing.T) {
	cfg := resolveSearchConfig("", 0, 0, 0, 0, 0, false)
	cfg.OllamaAPIKey, cfg.OllamaBase = resolveOllamaSearchConfig("some-key")
	if !cfg.searchEnabled() {
		t.Fatal("expected searchEnabled() true when only OllamaAPIKey is set (no SearXNG at all)")
	}
}

func TestSearchBackendLabel(t *testing.T) {
	disabled := searchConfig{}
	if got := disabled.searchBackendLabel(); got != "disabled" {
		t.Fatalf("expected %q for an all-zero config, got %q", "disabled", got)
	}
	ollamaOnly := searchConfig{OllamaAPIKey: "k", OllamaBase: "https://ollama.com"}
	if got := ollamaOnly.searchBackendLabel(); !strings.Contains(got, "Ollama") {
		t.Fatalf("expected label to mention Ollama, got %q", got)
	}
	both := searchConfig{SearXNGBase: "http://searxng:8080", OllamaAPIKey: "k", OllamaBase: "https://ollama.com"}
	if got := both.searchBackendLabel(); !strings.Contains(got, "SearXNG") {
		t.Fatalf("expected SearXNG to win the label when both backends are configured, got %q", got)
	}
}

// TestToolWebSearchOllamaBackendRunsQueriesInParallel mirrors
// TestToolWebSearchRunsQueriesInParallel but against a mock shaped like
// Ollama's hosted Web Search API (POST /api/web_search, Bearer auth,
// {"results":[{"title","url","content"}]}) instead of SearXNG - confirming
// the new backend is wired into toolWebSearch's concurrent fan-out
// identically to the original one.
func TestToolWebSearchOllamaBackendRunsQueriesInParallel(t *testing.T) {
	var hits int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/web_search", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-ollama-key" {
			t.Errorf("expected Bearer auth with the configured key, got %q", got)
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		time.Sleep(150 * time.Millisecond)
		resp := ollamaSearchResponse{Results: []ollamaSearchResult{
			{Title: "result for " + body["query"], URL: "https://example.com/" + body["query"], Content: "some content about " + body["query"]},
		}}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// No SearXNG configured at all - only the Ollama backend.
	cfg := resolveSearchConfig("", 0, 4, 0, 5, 0, false)
	cfg.OllamaAPIKey = "test-ollama-key"
	cfg.OllamaBase = srv.URL

	args := map[string]interface{}{
		"queries": []interface{}{"golang", "ollama", "searxng", "ripgrep"},
	}

	start := time.Now()
	result, err := toolWebSearch(args, cfg)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("toolWebSearch returned error: %v", err)
	}
	if atomic.LoadInt32(&hits) != 4 {
		t.Fatalf("expected 4 upstream hits, got %d", hits)
	}
	if elapsed > 400*time.Millisecond {
		t.Fatalf("expected concurrent fan-out (~150ms), took %s - looks serial", elapsed)
	}
	for _, q := range []string{"golang", "ollama", "searxng", "ripgrep"} {
		if !strings.Contains(result, q) {
			t.Fatalf("expected result to mention query %q, got: %s", q, result)
		}
	}
}

// TestToolWebSearchOllamaBackendRejectsBadKey confirms an HTTP
// 401/403 from Ollama's Web Search API (bad/missing key) surfaces as a
// clear, actionable error mentioning the relevant env vars/flag, not a
// generic JSON-parse failure.
func TestToolWebSearchOllamaBackendRejectsBadKey(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/web_search", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid api key"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := resolveSearchConfig("", 0, 0, 0, 5, 0, false)
	cfg.OllamaAPIKey = "bad-key"
	cfg.OllamaBase = srv.URL

	result, err := toolWebSearch(map[string]interface{}{"queries": []interface{}{"x"}}, cfg)
	if err != nil {
		t.Fatalf("expected the batch call itself to succeed with an ERROR slot, got err: %v", err)
	}
	if !strings.Contains(result, "ERROR") || !strings.Contains(result, "API key") {
		t.Fatalf("expected a clear API-key error mentioning 'API key', got: %s", result)
	}
}

// TestToolWebSearchSearXNGWinsWhenBothBackendsConfigured confirms the
// documented precedence rule: if a session has both OLA_SEARXNG_API_BASE
// and an Ollama Web Search API key configured, SearXNG is the one actually
// used (only its mock server receives hits) - preserving prior behavior
// for anyone who already had SearXNG configured before this backend
// existed.
func TestToolWebSearchSearXNGWinsWhenBothBackendsConfigured(t *testing.T) {
	var searxngHits, ollamaHits int32
	searxngMux := http.NewServeMux()
	searxngMux.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&searxngHits, 1)
		resp := searxngResponse{Results: []searxngResult{{Title: "from searxng", URL: "https://searxng.example.com", Content: "c"}}}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	searxngSrv := httptest.NewServer(searxngMux)
	defer searxngSrv.Close()

	ollamaMux := http.NewServeMux()
	ollamaMux.HandleFunc("/api/web_search", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&ollamaHits, 1)
		resp := ollamaSearchResponse{Results: []ollamaSearchResult{{Title: "from ollama", URL: "https://ollama.example.com", Content: "c"}}}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	ollamaSrv := httptest.NewServer(ollamaMux)
	defer ollamaSrv.Close()

	cfg := resolveSearchConfig(searxngSrv.URL, 0, 0, 0, 5, 0, false)
	cfg.OllamaAPIKey = "some-key"
	cfg.OllamaBase = ollamaSrv.URL

	result, err := toolWebSearch(map[string]interface{}{"queries": []interface{}{"x"}}, cfg)
	if err != nil {
		t.Fatalf("toolWebSearch returned error: %v", err)
	}
	if atomic.LoadInt32(&searxngHits) != 1 {
		t.Fatalf("expected SearXNG to be hit exactly once, got %d", searxngHits)
	}
	if atomic.LoadInt32(&ollamaHits) != 0 {
		t.Fatalf("expected Ollama Web Search API to NOT be hit when SearXNG is also configured, got %d hits", ollamaHits)
	}
	if !strings.Contains(result, "from searxng") || strings.Contains(result, "from ollama") {
		t.Fatalf("expected result to come from SearXNG only, got: %s", result)
	}
}

// TestToolWebSearchPublishesStructuredItemsForTerminalSummary confirms
// toolWebSearch stashes the same title/url data (per query, including
// per-query errors) that dispatchToolCall's terminal/log summary reads via
// popLastWebSearchItems - and that popping clears it, so a session that
// runs web_search twice never shows stale results from the first call.
func TestToolWebSearchPublishesStructuredItemsForTerminalSummary(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		if q == "bad" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		resp := searxngResponse{Results: []searxngResult{
			{Title: "Title for " + q, URL: "https://example.com/" + q, Content: "content"},
		}}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := resolveSearchConfig(srv.URL, 0, 2, 0, 5, 0, false)

	if _, err := toolWebSearch(map[string]interface{}{"queries": []interface{}{"good", "bad"}}, cfg); err != nil {
		t.Fatalf("toolWebSearch returned error: %v", err)
	}

	items := popLastWebSearchItems()
	if len(items) != 2 {
		t.Fatalf("expected 2 published query-item groups, got %d", len(items))
	}
	var sawGood, sawBad bool
	for _, qi := range items {
		switch qi.Query {
		case "good":
			sawGood = true
			if qi.Err != nil {
				t.Fatalf("expected no error for 'good' query, got: %v", qi.Err)
			}
			if len(qi.Items) != 1 || qi.Items[0].Title != "Title for good" || qi.Items[0].URL != "https://example.com/good" {
				t.Fatalf("unexpected items for 'good' query: %+v", qi.Items)
			}
		case "bad":
			sawBad = true
			if qi.Err == nil {
				t.Fatal("expected an error to be published for the 'bad' query")
			}
		}
	}
	if !sawGood || !sawBad {
		t.Fatalf("expected both 'good' and 'bad' queries to be represented, got: %+v", items)
	}

	// Popping clears the side-channel.
	if again := popLastWebSearchItems(); again != nil {
		t.Fatalf("expected popLastWebSearchItems to clear after popping once, got: %+v", again)
	}
}

// TestDispatchToolCallWebSearchLogsSummary drives web_search through the
// real dispatchToolCall path (the same one "ask" and "coding" both use) and
// confirms the -o log file gets a "[web_search_summary]" line reporting the
// total result count across all queries, plus every single result's
// title+link grouped by query - independent of, and un-truncated compared
// to, the generic 300-char [tool_result] preview dispatchToolCall already
// logs for every tool.
func TestDispatchToolCallWebSearchLogsSummary(t *testing.T) {
	dir := t.TempDir()
	mux := http.NewServeMux()
	mux.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		resp := searxngResponse{Results: []searxngResult{
			{Title: "Result A for " + q, URL: "https://a.example.com/" + q, Content: strings.Repeat("x", 500)},
			{Title: "Result B for " + q, URL: "https://b.example.com/" + q, Content: strings.Repeat("y", 500)},
		}}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := resolveSearchConfig(srv.URL, 0, 0, 0, 5, 0, false)

	outFile, err := os.CreateTemp(dir, "log")
	if err != nil {
		t.Fatal(err)
	}
	defer outFile.Close()

	argsJSON, _ := json.Marshal(map[string]interface{}{"queries": []string{"golang"}})
	tc := toolCall{Function: toolCallFunction{Name: "web_search", Arguments: argsJSON}}
	extra := func(name string, args map[string]interface{}) (string, error, bool) {
		if name == "web_search" {
			r, e := toolWebSearch(args, cfg)
			return r, e, true
		}
		return "", nil, false
	}

	result := dispatchToolCall(tc, "", "", "", outFile, extra)
	if strings.HasPrefix(result, "ERROR:") {
		t.Fatalf("expected web_search to succeed, got: %s", result)
	}

	logged, err := os.ReadFile(outFile.Name())
	if err != nil {
		t.Fatal(err)
	}
	logStr := string(logged)
	if !strings.Contains(logStr, "[web_search_summary] 2 ผลลัพธ์ทั้งหมด จาก 1 คำค้น") {
		t.Fatalf("expected a summary line with the total result count (2) and query count (1), got:\n%s", logStr)
	}
	if !strings.Contains(logStr, "Result A for golang") || !strings.Contains(logStr, "https://a.example.com/golang") {
		t.Fatalf("expected the first result's title+link to appear in full, got:\n%s", logStr)
	}
	if !strings.Contains(logStr, "Result B for golang") || !strings.Contains(logStr, "https://b.example.com/golang") {
		t.Fatalf("expected the second result's title+link to appear in full, got:\n%s", logStr)
	}
}
