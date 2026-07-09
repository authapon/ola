// search.go - optional web_search / web_fetch tools backed by:
//   - a local SearXNG instance for web_search (its native JSON API)
//   - a local Playwright-backed HTTP scrape service for web_fetch
//
// Design note: ola talks to BOTH over plain net/http only, exactly like it
// already talks to Ollama itself. It deliberately does NOT embed a
// Playwright/Puppeteer Go binding - doing so would pull in a Node.js driver
// process and downloaded browser binaries at runtime, breaking the
// single-native-Go-binary design this project otherwise holds to (see the
// package doc comment in main.go). Instead, OLA_FETCH_API_BASE is expected
// to point at a small local HTTP service - your own wrapper around
// Playwright, or a Browserless-compatible instance - that accepts:
//
//	POST {base}/scrape   {"url": "https://..."}
//	  -> {"ok": true, "title": "...", "markdown": "...", "text": "..."}
//
// Both tools accept a *list* of queries/URLs and fan them out concurrently
// (bounded by OLA_SEARCH_CONCURRENCY / OLA_FETCH_CONCURRENCY) so a model
// asking about several things at once doesn't pay for them serially.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ─────────────────────────────────────────────────────────────────
// Tunables + config resolution (flag > env > default, same precedence
// used throughout the rest of ola - see host/model/ctx in cmdAsk/cmdCoding)
// ─────────────────────────────────────────────────────────────────

const (
	defaultSearchMaxResults  = 5
	defaultSearchConcurrency = 3
	defaultFetchConcurrency  = 2 // browser tabs are far heavier than HTTP requests
	defaultSearchTimeoutSec  = 20
	defaultFetchTimeoutSec   = 30

	// maxWebResultOutput caps how much text a single search/fetch result
	// contributes to the model's context, same rationale as
	// maxRunCommandOutput in coding.go: one verbose page or bloated result
	// set must not blow the context budget by itself.
	maxWebResultOutput = 6000
)

// searchConfig holds resolved settings for the web_search/web_fetch tools.
// searchEnabled()/fetchEnabled() gate whether each tool is actually offered
// to the model at all - mirroring how run_command is only offered when a
// build/test toolchain was actually detected: a tool that can only ever
// error out just confuses a local model into calling it anyway.
type searchConfig struct {
	SearXNGBase       string
	FetchBase         string
	MaxResults        int
	SearchConcurrency int
	FetchConcurrency  int
	SearchTimeout     time.Duration
	FetchTimeout      time.Duration
}

func (c searchConfig) searchEnabled() bool { return c.SearXNGBase != "" }
func (c searchConfig) fetchEnabled() bool  { return c.FetchBase != "" }

// resolveSearchConfig applies flag > env > default precedence. Pass 0/""
// for any flag value the user didn't explicitly set. disable forces both
// tools off regardless of env/flags (wired to --no-web-search).
func resolveSearchConfig(searxngURL, fetchURL string, maxResults, searchConcurrency, fetchConcurrency, searchTimeoutSec, fetchTimeoutSec int, disable bool) searchConfig {
	if disable {
		return searchConfig{}
	}
	base := searxngURL
	if base == "" {
		base = os.Getenv("OLA_SEARXNG_API_BASE")
	}
	fetch := fetchURL
	if fetch == "" {
		fetch = os.Getenv("OLA_FETCH_API_BASE")
	}
	if maxResults <= 0 {
		maxResults = envInt("OLA_SEARCH_MAX_RESULTS", defaultSearchMaxResults)
	}
	if searchConcurrency <= 0 {
		searchConcurrency = envInt("OLA_SEARCH_CONCURRENCY", defaultSearchConcurrency)
	}
	if fetchConcurrency <= 0 {
		fetchConcurrency = envInt("OLA_FETCH_CONCURRENCY", defaultFetchConcurrency)
	}
	if searchTimeoutSec <= 0 {
		searchTimeoutSec = envInt("OLA_SEARCH_TIMEOUT_SEC", defaultSearchTimeoutSec)
	}
	if fetchTimeoutSec <= 0 {
		fetchTimeoutSec = envInt("OLA_FETCH_TIMEOUT_SEC", defaultFetchTimeoutSec)
	}
	return searchConfig{
		SearXNGBase:       strings.TrimRight(base, "/"),
		FetchBase:         strings.TrimRight(fetch, "/"),
		MaxResults:        maxResults,
		SearchConcurrency: searchConcurrency,
		FetchConcurrency:  fetchConcurrency,
		SearchTimeout:     time.Duration(searchTimeoutSec) * time.Second,
		FetchTimeout:      time.Duration(fetchTimeoutSec) * time.Second,
	}
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// ─────────────────────────────────────────────────────────────────
// Tool schemas
// ─────────────────────────────────────────────────────────────────

var webSearchTool = ollamaTool{
	Type: "function",
	Function: ollamaToolFunction{
		Name: "web_search",
		Description: "ค้นหาเว็บผ่าน local SearXNG instance รองรับหลายคำค้นพร้อมกันในเรียกเดียว " +
			"(รันแบบขนานให้อัตโนมัติ ไม่ต้องเรียกทีละคำ) ผลลัพธ์แต่ละคำค้นจะถูก truncate ถ้ายาวเกินไป",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"queries": map[string]interface{}{
					"type":        "array",
					"items":       map[string]interface{}{"type": "string"},
					"description": "รายการคำค้น อย่างน้อย 1 รายการ ระบุหลายคำค้นพร้อมกันได้เพื่อค้นแบบขนาน",
				},
				"max_results": map[string]interface{}{
					"type":        "integer",
					"description": fmt.Sprintf("จำนวนผลลัพธ์สูงสุดต่อคำค้น (default: %d)", defaultSearchMaxResults),
				},
			},
			"required": []string{"queries"},
		},
	},
}

var webFetchTool = ollamaTool{
	Type: "function",
	Function: ollamaToolFunction{
		Name: "web_fetch",
		Description: "โหลดเนื้อหาหน้าเว็บที่ render แล้ว (ผ่าน headless browser ในเครื่อง) จาก URL " +
			"รองรับหลาย URL พร้อมกันในเรียกเดียว (รันแบบขนานให้อัตโนมัติ) เนื้อหาที่ยาวเกินไปจะถูก truncate " +
			"เฉพาะ http/https URL สาธารณะเท่านั้น",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"urls": map[string]interface{}{
					"type":        "array",
					"items":       map[string]interface{}{"type": "string"},
					"description": "รายการ URL (http/https เท่านั้น) อย่างน้อย 1 รายการ",
				},
			},
			"required": []string{"urls"},
		},
	},
}

// ─────────────────────────────────────────────────────────────────
// web_search: SearXNG's native JSON API (GET /search?q=...&format=json).
// Requires "formats: [html, json]" enabled under search: in the instance's
// settings.yml, or the request comes back 403.
// ─────────────────────────────────────────────────────────────────

type searxngResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Content string `json:"content"`
}

type searxngResponse struct {
	Results []searxngResult `json:"results"`
}

func searchOne(client *http.Client, base, query string, maxResults int) (string, error) {
	u := base + "/search?q=" + url.QueryEscape(query) + "&format=json"
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("เรียก SearXNG ไม่สำเร็จ: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20)) // 2MB safety cap
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("SearXNG ตอบ HTTP %d (ตรวจสอบว่าเปิด 'formats: json' ใน settings.yml แล้วหรือยัง): %s",
			resp.StatusCode, truncateText(string(body), 300))
	}
	var parsed searxngResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("แปลง JSON จาก SearXNG ไม่ได้: %w", err)
	}
	if len(parsed.Results) == 0 {
		return "(ไม่พบผลลัพธ์)", nil
	}
	if maxResults <= 0 {
		maxResults = defaultSearchMaxResults
	}
	var b strings.Builder
	for i, r := range parsed.Results {
		if i >= maxResults {
			break
		}
		fmt.Fprintf(&b, "%d. %s\n   %s\n   %s\n\n", i+1, r.Title, r.URL, truncateText(r.Content, 300))
	}
	return truncateText(b.String(), maxWebResultOutput), nil
}

func toolWebSearch(args map[string]interface{}, cfg searchConfig) (string, error) {
	if !cfg.searchEnabled() {
		return "", fmt.Errorf("web_search ไม่ได้ถูกตั้งค่า (ต้องตั้ง OLA_SEARXNG_API_BASE หรือ --searxng-url)")
	}
	queries := stringsFromArg(args["queries"])
	if len(queries) == 0 {
		return "", fmt.Errorf("ต้องระบุ queries อย่างน้อย 1 รายการ (non-empty string)")
	}
	maxResults := cfg.MaxResults
	if mr, ok := args["max_results"].(float64); ok && mr > 0 {
		maxResults = int(mr)
	}

	client := &http.Client{Timeout: cfg.SearchTimeout}
	concurrency := cfg.SearchConcurrency
	if concurrency < 1 {
		concurrency = 1
	}
	results := make([]string, len(queries))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for i, q := range queries {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int, query string) {
			defer wg.Done()
			defer func() { <-sem }()
			r, err := searchOne(client, cfg.SearXNGBase, query, maxResults)
			if err != nil {
				results[idx] = fmt.Sprintf("=== query: %q ===\nERROR: %v", query, err)
				return
			}
			results[idx] = fmt.Sprintf("=== query: %q ===\n%s", query, r)
		}(i, q)
	}
	wg.Wait()
	return strings.Join(results, "\n\n"), nil
}

// ─────────────────────────────────────────────────────────────────
// web_fetch: talks to a local Playwright-backed HTTP scrape shim - see the
// package doc comment above for the expected contract.
// ─────────────────────────────────────────────────────────────────

type fetchShimResponse struct {
	OK       bool   `json:"ok"`
	Title    string `json:"title"`
	Markdown string `json:"markdown"`
	Text     string `json:"text"`
	Content  string `json:"content"` // fallback field name some shims use
	Message  string `json:"message"`
}

func fetchOne(client *http.Client, base, rawURL string) (string, error) {
	if err := validateFetchURL(rawURL); err != nil {
		return "", err
	}
	payload, err := json.Marshal(map[string]string{"url": rawURL})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequest(http.MethodPost, base+"/scrape", strings.NewReader(string(payload)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("เรียก fetch service ไม่สำเร็จ: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20)) // 4MB safety cap
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch service ตอบ HTTP %d: %s", resp.StatusCode, truncateText(string(body), 300))
	}
	var parsed fetchShimResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		// Not JSON - a minimal shim might just return raw markdown/text
		// directly as the body. Treat the whole thing as content rather
		// than failing outright.
		return truncateText(string(body), maxWebResultOutput), nil
	}
	if !parsed.OK && parsed.Message != "" {
		return "", fmt.Errorf("fetch service รายงานว่าล้มเหลว: %s", parsed.Message)
	}
	text := parsed.Markdown
	if text == "" {
		text = parsed.Text
	}
	if text == "" {
		text = parsed.Content
	}
	if text == "" {
		return "", fmt.Errorf("fetch service ไม่ได้คืนเนื้อหาที่อ่านได้ (คาดหวัง field markdown/text/content)")
	}
	header := ""
	if parsed.Title != "" {
		header = "# " + parsed.Title + "\n\n"
	}
	return truncateText(header+text, maxWebResultOutput), nil
}

func toolWebFetch(args map[string]interface{}, cfg searchConfig) (string, error) {
	if !cfg.fetchEnabled() {
		return "", fmt.Errorf("web_fetch ไม่ได้ถูกตั้งค่า (ต้องตั้ง OLA_FETCH_API_BASE หรือ --fetch-url)")
	}
	urls := stringsFromArg(args["urls"])
	if len(urls) == 0 {
		return "", fmt.Errorf("ต้องระบุ urls อย่างน้อย 1 รายการ (non-empty string)")
	}

	client := &http.Client{Timeout: cfg.FetchTimeout}
	concurrency := cfg.FetchConcurrency
	if concurrency < 1 {
		concurrency = 1
	}
	results := make([]string, len(urls))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for i, u := range urls {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int, target string) {
			defer wg.Done()
			defer func() { <-sem }()
			r, err := fetchOne(client, cfg.FetchBase, target)
			if err != nil {
				results[idx] = fmt.Sprintf("=== url: %s ===\nERROR: %v", target, err)
				return
			}
			results[idx] = fmt.Sprintf("=== url: %s ===\n%s", target, r)
		}(i, u)
	}
	wg.Wait()
	return strings.Join(results, "\n\n"), nil
}

// ─────────────────────────────────────────────────────────────────
// SSRF guard for web_fetch's target URL. This only guards the URL the
// model asks to fetch (fully model-controlled, and the fetched page's own
// content is untrusted per both system prompts' EXTERNAL/UNTRUSTED CONTENT
// section) - it does NOT apply to OLA_FETCH_API_BASE/OLA_SEARXNG_API_BASE
// themselves, which the user configures and are expected to be local.
// ─────────────────────────────────────────────────────────────────

func validateFetchURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("URL ไม่ถูกต้อง: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("รองรับเฉพาะ http/https URL, ได้ scheme %q", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("URL ไม่มี host")
	}
	if isObviouslyLocalHostname(host) {
		return fmt.Errorf("ปฏิเสธ URL ที่ชี้ไปยัง host ภายในเครื่อง: %s", host)
	}
	if ip := net.ParseIP(host); ip != nil {
		if isPrivateOrReservedIP(ip) {
			return fmt.Errorf("ปฏิเสธ URL ที่ชี้ไปยัง private/reserved IP: %s", host)
		}
		return nil
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		// DNS hiccup/offline - let the fetch service itself surface the
		// real error rather than failing the guard on an unrelated cause.
		return nil
	}
	for _, ip := range ips {
		if isPrivateOrReservedIP(ip) {
			return fmt.Errorf("ปฏิเสธ URL ที่ resolve ไปยัง private/reserved IP (%s -> %s) - web_fetch มีไว้สำหรับเว็บสาธารณะเท่านั้น", host, ip)
		}
	}
	return nil
}

func isObviouslyLocalHostname(host string) bool {
	h := strings.ToLower(host)
	return h == "localhost" || strings.HasSuffix(h, ".local") || strings.HasSuffix(h, ".internal")
}

func isPrivateOrReservedIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return true
	}
	// Cloud metadata endpoint (AWS/GCP/Azure instance metadata) - a classic
	// SSRF target, worth blocking explicitly even though it's technically
	// a public-looking unicast address.
	if ip.Equal(net.IPv4(169, 254, 169, 254)) {
		return true
	}
	return false
}

// ─────────────────────────────────────────────────────────────────
// Shared helpers
// ─────────────────────────────────────────────────────────────────

// stringsFromArg converts a JSON-decoded tool argument (an []interface{}
// of strings, as produced by json.Unmarshal into map[string]interface{})
// into a clean []string, dropping empty/non-string entries.
func stringsFromArg(v interface{}) []string {
	raw, _ := v.([]interface{})
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}

func truncateText(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + fmt.Sprintf("\n...(truncated, %d ตัวอักษรทั้งหมด)", len(s))
}
