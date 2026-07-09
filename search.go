// search.go - optional web_search / web_fetch tools backed by:
//   - a local SearXNG instance for web_search (its native JSON API)
//   - plain HTTP GET + HTML-to-text extraction for web_fetch, by default
//
// Design note: ola talks to SearXNG over plain net/http only, exactly like
// it already talks to Ollama itself - no embedded browser automation. For
// web_fetch, the *default* mode goes one step further and needs nothing
// external at all: ola does a plain http.Get on the target URL itself and
// strips the HTML down to readable text with the standard library only (no
// headless browser, no Playwright, no extra process). This covers the
// large majority of pages (docs, blog posts, articles, READMEs, API
// references) without any extra infrastructure.
//
// It does NOT execute JavaScript, so client-side-rendered pages (content
// that only appears after JS runs) will come back empty or thin. For that
// minority of cases, web_fetch can optionally be pointed at a local
// Playwright-backed HTTP scrape service instead (OLA_FETCH_API_BASE /
// --fetch-url) - when that's configured it takes precedence over direct
// mode automatically. The service just needs to accept:
//
//	POST {base}/scrape   {"url": "https://..."}
//	  -> {"ok": true, "title": "...", "markdown": "...", "text": "..."}
//
// Either way, ola itself never links against a browser automation library -
// it stays a single native Go binary with no runtime dependency beyond an
// HTTP client.
//
// Both tools accept a *list* of queries/URLs and fan them out concurrently
// (bounded by OLA_SEARCH_CONCURRENCY / OLA_FETCH_CONCURRENCY) so a model
// asking about several things at once doesn't pay for them serially.
package main

import (
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
	// defaultFetchConcurrency covers both fetch modes. Direct mode (plain
	// HTTP GET, the default) is cheap enough that this could go higher, but
	// this same knob also applies when OLA_FETCH_API_BASE points at a
	// Playwright-backed shim, where each concurrent fetch is a real browser
	// tab - keeping the shared default modest is the safer choice; raise it
	// per-run with --fetch-concurrency when only direct mode is in play.
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
type searchConfig struct {
	SearXNGBase       string
	FetchBase         string // shim mode (optional, JS-capable) - takes precedence over FetchDirect when set
	FetchDirect       bool   // direct mode: plain http.Get + HTML-to-text, no external service needed
	MaxResults        int
	SearchConcurrency int
	FetchConcurrency  int
	SearchTimeout     time.Duration
	FetchTimeout      time.Duration
}

func (c searchConfig) searchEnabled() bool { return c.SearXNGBase != "" }
func (c searchConfig) fetchEnabled() bool  { return c.FetchBase != "" || c.FetchDirect }

// fetchUsesShim reports whether web_fetch should use the Playwright-backed
// HTTP shim for this config. Shim mode wins whenever it's configured,
// because it can render JavaScript and direct mode can't - --web-fetch /
// OLA_WEB_FETCH_DIRECT is a "no external service available" fallback, not
// meant to silently override a shim the user deliberately set up.
func (c searchConfig) fetchUsesShim() bool { return c.FetchBase != "" }

// resolveSearchConfig applies flag > env > default precedence. Pass 0/""
// for any flag value the user didn't explicitly set. fetchDirectFlag wires
// --web-fetch (the "turn on direct-mode fetch" opt-in - no external service
// needed). disable forces everything off regardless of env/flags (wired to
// --no-web-search).
func resolveSearchConfig(searxngURL, fetchURL string, fetchDirectFlag bool, maxResults, searchConcurrency, fetchConcurrency, searchTimeoutSec, fetchTimeoutSec int, disable bool) searchConfig {
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
	fetchDirect := fetchDirectFlag || envBool("OLA_WEB_FETCH_DIRECT")
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
		FetchDirect:       fetchDirect,
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

func envBool(key string) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	return v == "1" || v == "true" || v == "yes" || v == "on"
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
		Description: "โหลดเนื้อหาหน้าเว็บจาก URL แล้วดึงเฉพาะข้อความ (ตัด HTML/script/style ออก) กลับมาให้ " +
			"รองรับหลาย URL พร้อมกันในเรียกเดียว (รันแบบขนานให้อัตโนมัติ) เนื้อหาที่ยาวเกินไปจะถูก truncate " +
			"เฉพาะ http/https URL สาธารณะเท่านั้น ปกติเป็นการ fetch แบบ HTTP ธรรมดา (ไม่รัน JavaScript) " +
			"ถ้าหน้าเว็บ render เนื้อหาด้วย JavaScript ล้วนๆ อาจได้ข้อความว่างหรือไม่ครบ",
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
// web_fetch: two modes.
//   - direct (default): plain http.Get + HTML-to-text extraction, no
//     external service required. Cannot execute JavaScript.
//   - shim (optional, set OLA_FETCH_API_BASE/--fetch-url): delegates to a
//     local Playwright-backed HTTP scrape service for JS-rendered pages.
//     See the package doc comment above for the expected contract.
// ─────────────────────────────────────────────────────────────────

// fetchOne dispatches to shim or direct mode per cfg.fetchUsesShim().
func fetchOne(client *http.Client, cfg searchConfig, rawURL string) (string, error) {
	if cfg.fetchUsesShim() {
		return fetchOneShim(client, cfg.FetchBase, rawURL)
	}
	return fetchOneDirect(client, rawURL)
}

// ── direct mode ─────────────────────────────────────────────────

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
// it keeps web_fetch's default mode dependency-free - and is generally
// good enough for a coding assistant skimming docs/articles/READMEs.
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

// fetchOneDirect is the production entry point for direct mode: SSRF policy
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
					"(ตั้ง OLA_FETCH_API_BASE/--fetch-url ให้ชี้ไปยัง Playwright-based scrape service ถ้าต้อง fetch หน้าแบบนี้)")
		}
		header := ""
		if title != "" {
			header = "# " + title + "\n\n"
		}
		return truncateText(header+text, maxWebResultOutput), nil
	case strings.Contains(ct, "text/") || strings.Contains(ct, "json") || strings.Contains(ct, "xml"):
		return truncateText(string(body), maxWebResultOutput), nil
	default:
		return "", fmt.Errorf("Content-Type %q ไม่ใช่ text/html/json - web_fetch (direct mode) รองรับเฉพาะเนื้อหาที่เป็นข้อความ", ct)
	}
}

// ── shim mode ────────────────────────────────────────────────────

type fetchShimResponse struct {
	OK       bool   `json:"ok"`
	Title    string `json:"title"`
	Markdown string `json:"markdown"`
	Text     string `json:"text"`
	Content  string `json:"content"` // fallback field name some shims use
	Message  string `json:"message"`
}

func fetchOneShim(client *http.Client, base, rawURL string) (string, error) {
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
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFetchDownloadBytes))
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
		return "", fmt.Errorf("web_fetch ไม่ได้ถูกตั้งค่า (เปิดโหมด direct ด้วย --web-fetch/OLA_WEB_FETCH_DIRECT=1 " +
			"หรือใช้ shim ด้วย OLA_FETCH_API_BASE/--fetch-url)")
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
			r, err := fetchOne(client, cfg, target)
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
