// search.go - optional web_search / web_fetch tools backed by:
//
//   - web_search has TWO interchangeable backends, either of which is
//     enough to turn the tool on - no self-hosted service is required
//     anymore:
//
//     1. Ollama's own hosted Web Search API (https://ollama.com/api/web_search) -
//     just an API key, set via OLA_OLLAMA_SEARCH_API_KEY/OLLAMA_API_KEY
//     or --ollama-search-key. No container, no separate service to run
//     or maintain - this is the recommended default for anyone who
//     doesn't already run a SearXNG instance.
//     2. a local SearXNG instance (its native JSON API) for anyone who
//     already self-hosts one and prefers that: set
//     OLA_SEARXNG_API_BASE / --searxng-url to enable it.
//
//     If both are configured, SearXNG wins (preserves prior behavior for
//     existing self-hosted setups) - see searchConfig.searchBackendLabel.
//
//   - a single, dependency-free "direct" mode for web_fetch: plain
//     http.Get + HTML-to-text extraction, done entirely within ola itself.
//     Unlike web_search, this needs no external service or configuration at
//     all, so it is turned on automatically for every session - the only
//     way to turn it off is --no-web-search, which also disables
//     web_search. Direct mode cannot execute JavaScript; a page that is
//     essentially an empty shell without it (a client-side-rendered SPA)
//     will come back as an explicit "no text found" error rather than
//     silently returning nothing useful.
//
// Design note: ola talks to SearXNG, to Ollama's Web Search API, and to
// fetch targets over plain net/http only - no embedded browser, no
// external scrape service, no Node.js driver process. ola remains a single
// native Go binary with no runtime dependency beyond an HTTP client.
//
// Both web_search and web_fetch accept a *list* of queries/URLs and fan
// them out concurrently (bounded by OLA_SEARCH_CONCURRENCY /
// OLA_FETCH_CONCURRENCY) so a model asking about several things at once
// doesn't pay for them serially.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
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
	// defaultFetchConcurrency bounds how many URLs web_fetch's single
	// (direct-mode) implementation will GET at once. Plain HTTP GETs are
	// cheap, so this can be raised per-run with --fetch-concurrency if
	// needed; the shared default is kept modest mainly so a model asking
	// about a long list of URLs at once doesn't hammer a single site.
	defaultFetchConcurrency = 4
	defaultSearchTimeoutSec = 20
	defaultFetchTimeoutSec  = 30

	// maxWebResultOutput caps how much text a single search/fetch result
	// contributes to the model's context, same rationale as
	// maxRunCommandOutput in coding.go: one verbose page or bloated result
	// set must not blow the context budget by itself.
	maxWebResultOutput = 6000

	// maxFetchDownloadBytes caps how much of a response body direct-mode
	// fetch will read before giving up, independent of the eventual
	// truncation to maxWebResultOutput - a multi-hundred-MB response must
	// not be downloaded in full just to throw most of it away afterwards.
	maxFetchDownloadBytes = 6 << 20 // 6MB
)

// searchConfig holds resolved settings for the web_search/web_fetch tools.
// searchEnabled()/fetchEnabled() gate whether each tool is actually offered
// to the model at all - mirroring how run_command is only offered when a
// build/test toolchain was actually detected: a tool that can only ever
// error out just confuses a local model into calling it anyway.
//
// web_search stays opt-in (either SearXNGBase or OllamaAPIKey must be
// configured), but web_fetch needs no external service, so FetchEnabled
// defaults to true and is only ever false when the whole feature was
// explicitly disabled (--no-web-search).
type searchConfig struct {
	SearXNGBase  string
	OllamaAPIKey string // Ollama Web Search API (https://ollama.com/api/web_search) - needs no self-hosted service, just an API key
	OllamaBase   string // base URL for the Ollama Web Search API, default defaultOllamaSearchBase (overridable for testing/self-hosted mirrors)

	FetchEnabled      bool // web_fetch (direct mode, plain HTTP): on by default
	MaxResults        int
	SearchConcurrency int
	FetchConcurrency  int
	SearchTimeout     time.Duration
	FetchTimeout      time.Duration
}

func (c searchConfig) searchEnabled() bool { return c.SearXNGBase != "" || c.OllamaAPIKey != "" }
func (c searchConfig) fetchEnabled() bool  { return c.FetchEnabled }

// searchBackendLabel describes, for status lines (dry-run/-o log
// header/help text) and error messages, which backend web_search will
// actually use this session. When both SearXNG and an Ollama Web Search
// API key are configured, SearXNG wins - this keeps prior behavior
// unchanged for anyone who already had OLA_SEARXNG_API_BASE set before
// this backend existed.
func (c searchConfig) searchBackendLabel() string {
	switch {
	case c.SearXNGBase != "":
		return fmt.Sprintf("SearXNG (%s)", c.SearXNGBase)
	case c.OllamaAPIKey != "":
		return fmt.Sprintf("Ollama Web Search API (%s)", c.OllamaBase)
	default:
		return "disabled"
	}
}

// resolveSearchConfig applies flag > env > default precedence for
// web_search's SearXNG backend and both tools' shared timeout/concurrency
// knobs. web_fetch itself has nothing to configure - it is a single
// zero-config direct-HTTP mode that is always on. disable forces
// everything off regardless of env/flags (wired to --no-web-search),
// turning off web_search AND web_fetch together.
func resolveSearchConfig(searxngURL string, maxResults, searchConcurrency, fetchConcurrency, searchTimeoutSec, fetchTimeoutSec int, disable bool) searchConfig {
	if disable {
		return searchConfig{}
	}
	base := searxngURL
	if base == "" {
		base = os.Getenv("OLA_SEARXNG_API_BASE")
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
		FetchEnabled:      true,
		MaxResults:        maxResults,
		SearchConcurrency: searchConcurrency,
		FetchConcurrency:  fetchConcurrency,
		SearchTimeout:     time.Duration(searchTimeoutSec) * time.Second,
		FetchTimeout:      time.Duration(fetchTimeoutSec) * time.Second,
	}
}

// defaultOllamaSearchBase is Ollama's hosted Web Search API host. Kept
// overridable (OLA_OLLAMA_SEARCH_API_BASE) purely for testing against a
// mock server - there is no supported self-hosted mirror of this endpoint.
const defaultOllamaSearchBase = "https://ollama.com"

// resolveOllamaSearchConfig applies flag > env > default precedence for the
// Ollama Web Search API backend, kept as a separate small function (rather
// than folded into resolveSearchConfig's existing 7-arg signature) so every
// existing call site of resolveSearchConfig - main.go, coding.go, and the
// whole existing search_test.go suite - keeps compiling untouched. Callers
// apply this on top of resolveSearchConfig's result, e.g.:
//
//	cfg := resolveSearchConfig(searxngURL, ...)
//	cfg.OllamaAPIKey, cfg.OllamaBase = resolveOllamaSearchConfig(ollamaSearchKeyFlag)
//
// The API key falls back to the plain OLLAMA_API_KEY env var (the name
// Ollama's own CLI/Python/JS libraries already use) so a machine that's
// already set up for `ollama.web_search`/the official examples needs no
// ola-specific configuration at all - OLA_OLLAMA_SEARCH_API_KEY only exists
// for the rare case of wanting a *different* key for ola specifically.
func resolveOllamaSearchConfig(apiKeyFlag string) (apiKey, base string) {
	apiKey = strings.TrimSpace(apiKeyFlag)
	if apiKey == "" {
		apiKey = strings.TrimSpace(os.Getenv("OLA_OLLAMA_SEARCH_API_KEY"))
	}
	if apiKey == "" {
		apiKey = strings.TrimSpace(os.Getenv("OLLAMA_API_KEY"))
	}
	base = strings.TrimRight(os.Getenv("OLA_OLLAMA_SEARCH_API_BASE"), "/")
	if base == "" {
		base = defaultOllamaSearchBase
	}
	return apiKey, base
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
		Description: "ค้นหาเว็บ (ผ่าน Ollama Web Search API หรือ local SearXNG instance แล้วแต่ค่าที่ตั้งไว้สำหรับเซสชันนี้) " +
			"รองรับหลายคำค้นพร้อมกันในเรียกเดียว " +
			"(รันแบบขนานให้อัตโนมัติ ไม่ต้องเรียกทีละคำ) ผลลัพธ์แต่ละคำค้นจะถูก truncate ถ้ายาวเกินไป - " +
			"เรียก tool นี้ทันทีเมื่อคำถามต้องการข้อมูลที่เปลี่ยนแปลงตามเวลาหรืออาจใหม่กว่าความรู้ที่โมเดลมี " +
			"เช่น ข่าวล่าสุด, สถานการณ์/ราคาตลาด ณ ปัจจุบัน, เวอร์ชันล่าสุดของซอฟต์แวร์ - โดยไม่ต้องรอให้ผู้ใช้ " +
			"พิมพ์ขอให้ค้นหาเองก่อน ถ้าคำถามระบุช่วงเวลาสัมพัทธ์ด้วย (เช่น \"ในรอบ 3 วันนี้\") ให้เรียก " +
			"get_current_time ก่อนเพื่อรู้วันที่จริง แล้วค่อยตั้งคำค้นจากวันที่นั้น",
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
		Description: "โหลดเนื้อหาหน้าเว็บจาก URL แล้วดึงเฉพาะข้อความ (ตัด HTML/script/style ออก) กลับมาให้ " +
			"รองรับหลาย URL พร้อมกันในเรียกเดียว (รันแบบขนานให้อัตโนมัติ) เนื้อหาที่ยาวเกินไปจะถูก truncate " +
			"เฉพาะ http/https URL สาธารณะเท่านั้น เป็นการ fetch แบบ HTTP ธรรมดา (plain GET) เสมอ - ไม่รัน " +
			"JavaScript ไม่ว่ากรณีใด ถ้าหน้านั้น render เนื้อหาด้วย JavaScript ล้วนๆ (เช่น SPA ที่ฝั่ง server " +
			"ไม่คืนอะไรมานอกจาก div ว่างๆ) จะได้ error กลับมาแทนที่จะเดาเนื้อหา ให้บอกผู้ใช้ตามตรงว่าเนื้อหานี้ " +
			"ดึงด้วยวิธีนี้ไม่ได้แทนการสมมติเนื้อหาเอง",
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
// web_search: two interchangeable backends behind one shared shape.
//
//   - SearXNG's native JSON API (GET /search?q=...&format=json). Requires
//     "formats: [html, json]" enabled under search: in the instance's
//     settings.yml, or the request comes back 403.
//   - Ollama's hosted Web Search API (POST /api/web_search, Bearer auth).
//     No self-hosted service required - see resolveOllamaSearchConfig.
//
// Both backends are normalized into []webSearchItem so the rest of this
// file (formatting for the model, and dispatchToolCall's terminal summary
// in main.go) doesn't need to know or care which one actually ran.
// ─────────────────────────────────────────────────────────────────

// webSearchItem is the backend-agnostic shape one search result is
// normalized into, regardless of whether it came from SearXNG or Ollama's
// Web Search API - both happen to return the same title/url/content
// fields, just under different transports.
type webSearchItem struct {
	Title   string
	URL     string
	Content string
}

// webSearchQueryItems pairs one query with its structured results (or the
// error that query hit). This exists purely so dispatchToolCall (main.go)
// can print an honest "found N results, here's every title+link" summary
// on the terminal without re-parsing toolWebSearch's already-formatted,
// per-result-truncated, model-facing string - see lastWebSearchItems below.
type webSearchQueryItems struct {
	Query string
	Items []webSearchItem
	Err   error
}

// lastWebSearchItems is a small side-channel: toolWebSearch stashes the
// structured results of the query batch it just ran here, and
// dispatchToolCall (main.go) pops them right after the call returns to
// print the terminal summary. This is deliberately NOT threaded through
// toolWebSearch's return value / the extraTools(name, args) (string,
// error, bool) callback shape, since that shape is shared across
// run_command/web_search/web_fetch/read_skill and changing it would ripple
// into every caller for the benefit of exactly one of the four tools.
// Guarded by a mutex since toolWebSearch's per-query goroutines all
// contribute to the same batch before it's published in one shot.
var (
	lastWebSearchMu    sync.Mutex
	lastWebSearchItems []webSearchQueryItems
)

func setLastWebSearchItems(items []webSearchQueryItems) {
	lastWebSearchMu.Lock()
	lastWebSearchItems = items
	lastWebSearchMu.Unlock()
}

// popLastWebSearchItems returns and clears the most recently completed
// toolWebSearch call's structured results. Safe to call even when
// web_search was never invoked this session (returns nil then).
func popLastWebSearchItems() []webSearchQueryItems {
	lastWebSearchMu.Lock()
	defer lastWebSearchMu.Unlock()
	items := lastWebSearchItems
	lastWebSearchItems = nil
	return items
}

// formatSearchResults renders normalized items into the same numbered
// "title/url/truncated-content" block both backends used to build inline
// before this refactor - kept byte-for-byte equivalent so the model-facing
// contract (and every existing test asserting on that shape) is unchanged
// regardless of which backend actually produced the items.
func formatSearchResults(items []webSearchItem, maxResults int) string {
	if len(items) == 0 {
		return "(ไม่พบผลลัพธ์)"
	}
	if maxResults <= 0 {
		maxResults = defaultSearchMaxResults
	}
	var b strings.Builder
	for i, r := range items {
		if i >= maxResults {
			break
		}
		fmt.Fprintf(&b, "%d. %s\n   %s\n   %s\n\n", i+1, r.Title, r.URL, truncateText(r.Content, 300))
	}
	return truncateText(b.String(), maxWebResultOutput)
}

type searxngResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Content string `json:"content"`
}

type searxngResponse struct {
	Results []searxngResult `json:"results"`
}

func searchOne(client *http.Client, base, query string, maxResults int) ([]webSearchItem, error) {
	u := base + "/search?q=" + url.QueryEscape(query) + "&format=json"
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("เรียก SearXNG ไม่สำเร็จ: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20)) // 2MB safety cap
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("SearXNG ตอบ HTTP %d (ตรวจสอบว่าเปิด 'formats: json' ใน settings.yml แล้วหรือยัง): %s",
			resp.StatusCode, truncateText(string(body), 300))
	}
	var parsed searxngResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("แปลง JSON จาก SearXNG ไม่ได้: %w", err)
	}
	items := make([]webSearchItem, len(parsed.Results))
	for i, r := range parsed.Results {
		items[i] = webSearchItem{Title: r.Title, URL: r.URL, Content: r.Content}
	}
	return items, nil
}

// ollamaSearchResult/ollamaSearchResponse mirror the JSON shape documented
// at https://docs.ollama.com/capabilities/web-search:
// {"results":[{"title":...,"url":...,"content":...}]} - notice this is the
// same three fields as searxngResult, just reached over a different
// transport (POST + Bearer auth vs. a plain GET).
type ollamaSearchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Content string `json:"content"`
}

type ollamaSearchResponse struct {
	Results []ollamaSearchResult `json:"results"`
}

// searchOneOllama calls Ollama's hosted Web Search API for a single query.
// The API itself has no max_results parameter (it returns a fixed set, up
// to 10 by default per Ollama's docs) - trimming to the caller's requested
// maxResults happens client-side in formatSearchResults, same as SearXNG.
func searchOneOllama(client *http.Client, base, apiKey, query string, maxResults int) ([]webSearchItem, error) {
	reqBody, err := json.Marshal(map[string]string{"query": query})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, base+"/api/web_search", bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("เรียก Ollama Web Search API ไม่สำเร็จ: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20)) // 2MB safety cap
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("Ollama Web Search API ปฏิเสธ API key (HTTP %d) - ตรวจสอบ OLA_OLLAMA_SEARCH_API_KEY/OLLAMA_API_KEY/--ollama-search-key: %s",
			resp.StatusCode, truncateText(string(body), 200))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Ollama Web Search API ตอบ HTTP %d: %s", resp.StatusCode, truncateText(string(body), 300))
	}
	var parsed ollamaSearchResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("แปลง JSON จาก Ollama Web Search API ไม่ได้: %w", err)
	}
	items := make([]webSearchItem, len(parsed.Results))
	for i, r := range parsed.Results {
		items[i] = webSearchItem{Title: r.Title, URL: r.URL, Content: r.Content}
	}
	return items, nil
}

func toolWebSearch(args map[string]interface{}, cfg searchConfig) (string, error) {
	if !cfg.searchEnabled() {
		return "", fmt.Errorf("web_search ไม่ได้ถูกตั้งค่า (ต้องตั้ง OLA_OLLAMA_SEARCH_API_KEY/--ollama-search-key หรือ OLA_SEARXNG_API_BASE/--searxng-url)")
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
	queryItems := make([]webSearchQueryItems, len(queries))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for i, q := range queries {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int, query string) {
			defer wg.Done()
			defer func() { <-sem }()

			// SearXNG wins when both are configured (see
			// searchConfig.searchBackendLabel) - preserves prior behavior
			// for anyone who already had OLA_SEARXNG_API_BASE set.
			var items []webSearchItem
			var err error
			if cfg.SearXNGBase != "" {
				items, err = searchOne(client, cfg.SearXNGBase, query, maxResults)
			} else {
				items, err = searchOneOllama(client, cfg.OllamaBase, cfg.OllamaAPIKey, query, maxResults)
			}

			queryItems[idx] = webSearchQueryItems{Query: query, Items: items, Err: err}
			if err != nil {
				results[idx] = fmt.Sprintf("=== query: %q ===\nERROR: %v", query, err)
				return
			}
			results[idx] = fmt.Sprintf("=== query: %q ===\n%s", query, formatSearchResults(items, maxResults))
		}(i, q)
	}
	wg.Wait()
	setLastWebSearchItems(queryItems)
	return strings.Join(results, "\n\n"), nil
}

// ─────────────────────────────────────────────────────────────────
// web_fetch: a single mode - direct. Plain http.Get + HTML-to-text
// extraction, no external service required, always enabled by default
// (see resolveSearchConfig/searchConfig.fetchEnabled). Cannot execute
// JavaScript; a JS-only page will surface as an explicit error rather than
// silently returning an empty/near-empty result.
// ─────────────────────────────────────────────────────────────────

var (
	reHTMLScript      = regexp.MustCompile(`(?is)<script\b[^>]*>.*?</script>`)
	reHTMLStyle       = regexp.MustCompile(`(?is)<style\b[^>]*>.*?</style>`)
	reHTMLComment     = regexp.MustCompile(`(?s)<!--.*?-->`)
	reHTMLTitle       = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	reHTMLBlockClose  = regexp.MustCompile(`(?i)</\s*(p|div|br|li|h[1-6]|tr|table|section|article|header|footer|ul|ol|blockquote|pre)\s*>`)
	reHTMLTag         = regexp.MustCompile(`(?s)<[^>]+>`)
	reCollapseSpaces  = regexp.MustCompile(`[ \t\f\v]+`)
	reCollapseBlanks  = regexp.MustCompile(`\n{3,}`)
	reUserAgentString = "Mozilla/5.0 (compatible; ola-web-fetch/1.0; +https://github.com/)"
)

// htmlToText strips an HTML document down to a plain-text approximation of
// its visible content, using only the standard library (regexp + html
// entity unescaping - no full HTML parser, no external dependency). This is
// intentionally a rough "poor man's readability", not a proper
// main-content extractor: it will still include nav/footer/boilerplate
// text that a real reader-mode would drop. That trade-off is deliberate -
// it keeps web_fetch dependency-free - and is generally good enough for a
// coding assistant skimming docs/articles/READMEs.
func htmlToText(body string) (title, text string) {
	if m := reHTMLTitle.FindStringSubmatch(body); len(m) > 1 {
		title = strings.TrimSpace(html.UnescapeString(reHTMLTag.ReplaceAllString(m[1], "")))
	}
	s := reHTMLScript.ReplaceAllString(body, " ")
	s = reHTMLStyle.ReplaceAllString(s, " ")
	s = reHTMLComment.ReplaceAllString(s, " ")
	s = reHTMLBlockClose.ReplaceAllString(s, "\n") // turn block boundaries into line breaks first
	s = reHTMLTag.ReplaceAllString(s, " ")         // then drop all remaining tags
	s = html.UnescapeString(s)
	s = reCollapseSpaces.ReplaceAllString(s, " ")

	var lines []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			lines = append(lines, line)
		}
	}
	text = reCollapseBlanks.ReplaceAllString(strings.Join(lines, "\n"), "\n\n")
	return title, text
}

// fetchOneDirect is the production entry point for web_fetch: SSRF policy
// (validateFetchURL) is enforced here, then delegates to doDirectFetch for
// the actual HTTP GET + content extraction. Kept separate so tests can
// exercise doDirectFetch's mechanics (content-type handling, HTML-to-text)
// against a local httptest server without tripping the loopback rejection
// that a *production* fetch target correctly should trip.
func fetchOneDirect(client *http.Client, rawURL string) (string, error) {
	if err := validateFetchURL(rawURL); err != nil {
		return "", err
	}
	return doDirectFetch(client, rawURL)
}

func doDirectFetch(client *http.Client, rawURL string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	// A generic browser-like UA and Accept header: several sites reject or
	// serve a stripped-down response to requests that look like a bare
	// script client, independent of any JS-rendering requirement.
	req.Header.Set("User-Agent", reUserAgentString)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain;q=0.9,application/json;q=0.8,*/*;q=0.5")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch URL ไม่สำเร็จ: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))
		return "", fmt.Errorf("HTTP %d จาก %s: %s", resp.StatusCode, rawURL, truncateText(string(body), 200))
	}

	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFetchDownloadBytes))
	if err != nil {
		return "", fmt.Errorf("อ่าน response body ไม่ได้: %w", err)
	}

	switch {
	case strings.Contains(ct, "html"):
		title, text := htmlToText(string(body))
		if strings.TrimSpace(text) == "" {
			return "", fmt.Errorf(
				"หน้านี้ไม่เหลือข้อความหลังตัด HTML ออก - เป็นไปได้ว่าเนื้อหา render ด้วย JavaScript ล้วนๆ " +
					"web_fetch ไม่รัน JavaScript ไม่มีทางดึงเนื้อหาแบบนี้ได้ ให้แจ้งผู้ใช้ตามตรงแทนการเดา")
		}
		header := ""
		if title != "" {
			header = "# " + title + "\n\n"
		}
		return truncateText(header+text, maxWebResultOutput), nil
	case strings.Contains(ct, "text/") || strings.Contains(ct, "json") || strings.Contains(ct, "xml"):
		return truncateText(string(body), maxWebResultOutput), nil
	default:
		return "", fmt.Errorf("Content-Type %q ไม่ใช่ text/html/json - web_fetch รองรับเฉพาะเนื้อหาที่เป็นข้อความ", ct)
	}
}

func toolWebFetch(args map[string]interface{}, cfg searchConfig) (string, error) {
	if !cfg.fetchEnabled() {
		return "", fmt.Errorf("web_fetch ถูกปิดใช้งานสำหรับเซสชันนี้ (ใช้ --no-web-search เพื่อปิด - เอาออกถ้าต้องการเปิดกลับ)")
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
			r, err := fetchOneDirect(client, target)
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
// section) - it does NOT apply to OLA_SEARXNG_API_BASE itself, which the
// user configures and is expected to be local.
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
		// DNS hiccup/offline - let the fetch itself surface the real error
		// rather than failing the guard on an unrelated cause.
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
