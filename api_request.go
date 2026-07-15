// api_request.go - the "api_request" tool: a general-purpose HTTP client
// the model can use to call APIs, following the same "only offer what the
// user actually opted into" principle as web_search/web_fetch/scp_copy
// (see main.go's package doc comment and integrations.go). Fully opt-in:
// unless OLA_API_ENDPOINTS/--api-endpoints is set OR --api-allow-direct-url
// is explicitly turned on, this tool does not exist for the session at
// all and has zero effect.
//
// api_request is meaningfully more dangerous than web_fetch, so it gets
// its own, stricter guardrails rather than reusing web_fetch's shape
// as-is:
//
//   - web_fetch is GET-only and always public-web-only. api_request can
//     send POST/PUT/PATCH/DELETE with an arbitrary body, so mutating
//     methods are gated behind a second, separate opt-in
//     (--api-allow-mutating/OLA_API_ALLOW_MUTATING) - a session that only
//     wants read access to some internal API never accidentally exposes
//     a DELETE.
//
//   - Two independent ways to pick a target, same split as
//     scp_copy/web_fetch's own history in this codebase:
//
//     1. "endpoint" mode (preferred, always available once any endpoint
//        is configured): the model picks a pre-approved alias from
//        OLA_API_ENDPOINTS - it never supplies a host, port, or scheme
//        itself, the same "operator pre-approves the destination" shape
//        scp_copy uses for remote_alias. This is the only way to reach a
//        private/internal host (e.g. Moo's own Ollama/Open WebUI/SearXNG
//        stack on Docker Swarm) - see resolveAPIRequestConfig. Optional
//        per-alias credentials (OLA_API_ENDPOINT_<ALIAS>_AUTH_HEADER/
//        _AUTH_VALUE) are injected by ola itself and are never visible to
//        or settable by the model, so a prompt-injected instruction from
//        some earlier fetched web page can never exfiltrate them.
//
//     2. "url" mode (opt-in via --api-allow-direct-url/
//        OLA_API_ALLOW_DIRECT_URL, off by default): the model supplies a
//        full URL directly, same as web_fetch. Reuses web_fetch's own SSRF
//        guard (validateFetchURL in main.go) verbatim, so a direct-mode
//        api_request call is never less safe than web_fetch is today -
//        private/reserved IPs and obviously-local hostnames are rejected
//        exactly the same way.
//
//   - Header allowlist: the model can add arbitrary headers EXCEPT a
//     small reserved set (Host, Content-Length, Transfer-Encoding,
//     Connection, Authorization) - see isReservedRequestHeader. Blocking
//     Authorization specifically means "call this API with a bearer
//     token" always goes through the endpoint-alias + AUTH_HEADER config
//     path above, never through a token the model typed inline, which
//     would otherwise end up sitting in the tool_call preview printed to
//     the terminal and -o log file.
//
//   - Request/response size caps (MaxRequestBytes/MaxResponseBytes,
//     independent of each other) and a per-call timeout, same rationale
//     as maxFetchDownloadBytes/FetchTimeout in integrations.go.
//
//   - A non-2xx HTTP response is NOT treated as a Go error - unlike
//     web_fetch's doDirectFetch, which does error on non-200. Many real
//     APIs put the actually-useful information (validation errors,
//     structured problem-details bodies) in a 4xx/5xx response, and
//     hiding that from the model would make the tool less useful for
//     exactly the calls most worth showing it. Only genuine transport
//     failures (DNS, connection refused, TLS, timeout) become a Go error.
//     See formatAPIResponse.
//
// Available in both "ask" and "coding" (see extraTools in main.go and
// codingToolset/dispatchCodingToolCall's extra closure) - the same set of
// subcommands web_search/web_fetch are offered in, since api_request is a
// general capability rather than an ask-only convenience like scp_copy.
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ─────────────────────────────────────────────────────────────────
// Config
// ─────────────────────────────────────────────────────────────────

const (
	defaultAPIRequestTimeoutSec       = 30
	defaultAPIRequestMaxBodyBytes     = 2 << 20 // 2MB - request body the model builds itself (json/form/multipart/text/binary)
	defaultAPIRequestMaxResponseBytes = 4 << 20 // 4MB - raw download cap, independent of the further text-truncation below

	// maxAPIResultOutput caps how much of a text/json/xml response body is
	// actually shown to the model, same budget/rationale as
	// maxWebResultOutput (integrations.go) - one verbose API response must
	// not blow the context budget by itself.
	maxAPIResultOutput = 6000
)

// apiEndpoint is one operator-configured, pre-approved API target. The
// model only ever selects one of these by Alias - BaseURL, AuthHeader, and
// AuthValue are never visible to or settable by the model, mirroring how
// scpHost's user/host/port/remote-root are never model-controlled either.
type apiEndpoint struct {
	Alias      string
	BaseURL    string
	AuthHeader string // e.g. "Authorization" or "X-API-Key"; empty = no injected auth for this endpoint
	AuthValue  string // never logged, never echoed back into any tool_call preview
}

// apiRequestConfig is the resolved result of OLA_API_ENDPOINTS/
// --api-endpoints plus the direct-URL/mutating-method opt-ins and the
// shared timeout/size caps. enabled() gates whether api_request is
// offered to the model at all, mirroring searchConfig.searchEnabled()/
// scpConfig.enabled() elsewhere in this codebase.
type apiRequestConfig struct {
	Endpoints     map[string]apiEndpoint
	EndpointOrder []string // preserves config order for stable-ish error listings before sorting

	AllowDirectURL bool // opt-in: model may also pass a raw "url" instead of "endpoint"+"path"
	AllowMutating  bool // opt-in: POST/PUT/PATCH/DELETE; GET/HEAD/OPTIONS always allowed once enabled() is true

	Timeout          time.Duration
	MaxRequestBytes  int64
	MaxResponseBytes int64
}

func (c apiRequestConfig) enabled() bool {
	return len(c.Endpoints) > 0 || c.AllowDirectURL
}

// endpointList renders the allowed alias names for error messages, sorted
// so the message is stable/testable rather than depending on map
// iteration order - same approach as scpConfig.aliasList.
func (c apiRequestConfig) endpointList() string {
	if len(c.EndpointOrder) == 0 {
		return "(ไม่มี - ยังไม่ได้ตั้งค่า OLA_API_ENDPOINTS/--api-endpoints)"
	}
	names := append([]string{}, c.EndpointOrder...)
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// resolveAPIRequestConfig applies flag > env > default precedence, the
// same convention used throughout ola (resolveSearchConfig, resolveSCPConfig).
// A bad individual OLA_API_ENDPOINTS entry is collected as a warning (that
// one alias is skipped), not fatal - same non-fatal shape resolveSCPConfig
// uses for OLA_SCP_HOSTS.
func resolveAPIRequestConfig(endpointsFlag string, allowDirectFlag, allowMutatingFlag bool, timeoutSecFlag int, maxReqBytesFlag, maxRespBytesFlag int64) (apiRequestConfig, []string) {
	var warnings []string

	raw := endpointsFlag
	if raw == "" {
		raw = os.Getenv("OLA_API_ENDPOINTS")
	}
	endpoints := map[string]apiEndpoint{}
	var order []string
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		ep, err := parseAPIEndpointEntry(entry)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("OLA_API_ENDPOINTS: ข้าม entry %q (%v)", entry, err))
			continue
		}
		if _, dup := endpoints[ep.Alias]; dup {
			warnings = append(warnings, fmt.Sprintf("OLA_API_ENDPOINTS: alias %q ซ้ำ - ใช้ตัวแรกที่เจอ", ep.Alias))
			continue
		}
		ep.AuthHeader, ep.AuthValue = resolveAPIEndpointAuth(ep.Alias)
		endpoints[ep.Alias] = ep
		order = append(order, ep.Alias)
	}

	allowDirect := allowDirectFlag || envBool("OLA_API_ALLOW_DIRECT_URL")
	allowMutating := allowMutatingFlag || envBool("OLA_API_ALLOW_MUTATING")

	timeoutSec := timeoutSecFlag
	if timeoutSec <= 0 {
		timeoutSec = envInt("OLA_API_REQUEST_TIMEOUT_SEC", defaultAPIRequestTimeoutSec)
	}
	maxReq := maxReqBytesFlag
	if maxReq <= 0 {
		maxReq = int64(envInt("OLA_API_REQUEST_MAX_BODY_BYTES", defaultAPIRequestMaxBodyBytes))
	}
	maxResp := maxRespBytesFlag
	if maxResp <= 0 {
		maxResp = int64(envInt("OLA_API_REQUEST_MAX_RESPONSE_BYTES", defaultAPIRequestMaxResponseBytes))
	}

	return apiRequestConfig{
		Endpoints:        endpoints,
		EndpointOrder:    order,
		AllowDirectURL:   allowDirect,
		AllowMutating:    allowMutating,
		Timeout:          time.Duration(timeoutSec) * time.Second,
		MaxRequestBytes:  maxReq,
		MaxResponseBytes: maxResp,
	}, warnings
}

// parseAPIEndpointEntry parses one "alias=https://base.url" entry from
// OLA_API_ENDPOINTS/--api-endpoints - deliberately simpler than
// parseSCPHostEntry's "alias=user@host[:port]/root" shape, since an API
// endpoint is just a base URL, not a user/host/port/root tuple. Only ONE
// "=" is expected (between alias and the URL); base URLs don't contain a
// bare "=" in practice, and unlike parseSCPHostEntry there's no second
// delimiter to worry about.
func parseAPIEndpointEntry(entry string) (apiEndpoint, error) {
	const usage = `รูปแบบต้องเป็น "alias=https://base.url"`

	eqIdx := strings.Index(entry, "=")
	if eqIdx <= 0 {
		return apiEndpoint{}, fmt.Errorf("%s", usage)
	}
	alias := strings.TrimSpace(entry[:eqIdx])
	base := strings.TrimSpace(entry[eqIdx+1:])
	if alias == "" || base == "" {
		return apiEndpoint{}, fmt.Errorf("alias/base URL ต้องไม่ว่างเปล่า")
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return apiEndpoint{}, fmt.Errorf("base URL ต้องเป็น http/https ที่ถูกต้อง (ได้ %q)", base)
	}
	// Deliberately NO SSRF guard here, unlike validateFetchURL: an
	// endpoint's base URL is trusted operator configuration, not
	// model-controlled input - the whole point of endpoint mode is to let
	// a private/internal host (e.g. http://localhost:11434) be reached
	// safely, which the model itself could never do via direct-URL mode.
	return apiEndpoint{Alias: alias, BaseURL: strings.TrimRight(base, "/")}, nil
}

// resolveAPIEndpointAuth reads an optional per-alias credential the model
// never sees: OLA_API_ENDPOINT_<ALIAS>_AUTH_HEADER names the header (e.g.
// "Authorization", "X-API-Key") and OLA_API_ENDPOINT_<ALIAS>_AUTH_VALUE is
// its value. Both are read fresh from the environment (not from
// --api-endpoints, which only ever carries base URLs) so a secret value
// never has to appear in a shell history alongside the endpoint list, and
// is never part of anything printed/logged for this alias's config.
func resolveAPIEndpointAuth(alias string) (header, value string) {
	key := envKeyFromAlias(alias)
	header = strings.TrimSpace(os.Getenv("OLA_API_ENDPOINT_" + key + "_AUTH_HEADER"))
	if header == "" {
		return "", ""
	}
	return header, os.Getenv("OLA_API_ENDPOINT_" + key + "_AUTH_VALUE")
}

// envKeyFromAlias upper-cases alias and replaces every non [A-Z0-9] rune
// with "_", turning an alias like "open-webui" into the env var fragment
// "OPEN_WEBUI" (so OLA_API_ENDPOINT_OPEN_WEBUI_AUTH_HEADER is what you'd
// set for it) - env var names can't contain "-".
func envKeyFromAlias(alias string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(alias) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	return b.String()
}

// envBool reports whether an environment variable is set to a
// conventional "true" value. Unlike envInt (which has a numeric default),
// every api_request boolean opt-in defaults to false/off when unset, so a
// simple presence-style check is all that's needed.
func envBool(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// ─────────────────────────────────────────────────────────────────
// Tool schema
// ─────────────────────────────────────────────────────────────────

var apiRequestTool = ollamaTool{
	Type: "function",
	Function: ollamaToolFunction{
		Name: "api_request",
		Description: "ยิง HTTP request ไปยัง API รองรับ 2 วิธีเลือกปลายทาง: (1) endpoint - ระบุ \"endpoint\" " +
			"เป็นชื่อ alias ที่ผู้ใช้ตั้งค่าไว้ล่วงหน้า (OLA_API_ENDPOINTS) พร้อม \"path\" ต่อท้าย - วิธีนี้เท่านั้นที่เข้าถึง " +
			"host ภายใน/private ได้ และถ้า endpoint นั้นตั้ง credential ไว้ ola จะแนบให้เองโดยที่โมเดลไม่เห็นค่าจริง " +
			"(2) url - ระบุ URL ตรง (เฉพาะเมื่อเปิด --api-allow-direct-url ไว้) รองรับเฉพาะเว็บสาธารณะเหมือน web_fetch " +
			"(ปฏิเสธ private/reserved IP) ระบุ query/headers เพิ่มเติมได้ (ยกเว้น header ที่สงวนไว้ เช่น Authorization - " +
			"ถ้าต้องใช้ auth ให้ตั้งค่าไว้ที่ endpoint แทน) method GET/HEAD/OPTIONS ใช้ได้เสมอ ส่วน POST/PUT/PATCH/DELETE " +
			"ต้องเปิด --api-allow-mutating ไว้ก่อน body รองรับหลายชนิดผ่าน body_type: json (body เป็น object/array), " +
			"form (body เป็น object key:value, ส่งแบบ x-www-form-urlencoded), multipart (body เป็น object field:value " +
			"บวก multipart_files สำหรับไฟล์แนบในเครื่อง), text (body เป็น string ดิบ), binary (body เป็น base64 string), " +
			"none (ไม่มี body) response ที่ไม่ใช่ 2xx จะไม่ถือเป็น error - จะคืน status code และเนื้อหากลับมาให้ตัดสินใจเอง",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"endpoint": map[string]interface{}{
					"type":        "string",
					"description": "ชื่อ alias ของ endpoint ที่ config ไว้ (ระบุอย่างใดอย่างหนึ่งกับ url เท่านั้น)",
				},
				"path": map[string]interface{}{
					"type":        "string",
					"description": "path ต่อท้าย base URL ของ endpoint เช่น \"/api/tags\" (ใช้คู่กับ endpoint เท่านั้น)",
				},
				"url": map[string]interface{}{
					"type":        "string",
					"description": "URL ปลายทางแบบเต็ม http/https (ใช้ได้เฉพาะเมื่อเปิด --api-allow-direct-url - ระบุอย่างใดอย่างหนึ่งกับ endpoint เท่านั้น)",
				},
				"method": map[string]interface{}{
					"type":        "string",
					"enum":        []string{"GET", "HEAD", "OPTIONS", "POST", "PUT", "PATCH", "DELETE"},
					"description": "default: GET",
				},
				"query": map[string]interface{}{
					"type":        "object",
					"description": "query string params เพิ่มเติม (key:value เป็น string หรือ ตัวเลข/บูลีน)",
				},
				"headers": map[string]interface{}{
					"type":        "object",
					"description": "header เพิ่มเติม (key:value เป็น string) - header ที่สงวนไว้ (Host, Authorization, Content-Length, Transfer-Encoding, Connection) จะถูกข้ามเสมอ",
				},
				"body_type": map[string]interface{}{
					"type":        "string",
					"enum":        []string{"json", "form", "multipart", "text", "binary", "none"},
					"description": "default: none",
				},
				"body": map[string]interface{}{
					"description": "เนื้อหา body ตาม body_type: json→object/array ใดๆ, form/multipart→object ของ field:value string, text→string ดิบ, binary→string base64, none→ไม่ต้องใส่",
				},
				"multipart_files": map[string]interface{}{
					"type":        "array",
					"items":       map[string]interface{}{"type": "string"},
					"description": "รายการ path ไฟล์ในเครื่อง (ต้องอยู่ใต้ current directory) ที่จะแนบเป็น multipart file field - ใช้ได้เฉพาะ body_type=multipart",
				},
			},
			"required": []string{},
		},
	},
}

// ─────────────────────────────────────────────────────────────────
// Reserved / blocked request headers
// ─────────────────────────────────────────────────────────────────

// reservedAPIRequestHeaders lists header names the model is never allowed
// to set directly, regardless of endpoint/direct-URL mode:
//   - Host/Content-Length/Transfer-Encoding/Connection are connection-level
//     concerns net/http manages itself; letting the model set them risks
//     request smuggling / a mismatched body length rather than anything
//     useful.
//   - Authorization is blocked so "call this API with a bearer token"
//     always goes through the endpoint-alias + AUTH_HEADER/AUTH_VALUE
//     config path (see resolveAPIEndpointAuth) instead of a token typed
//     inline by the model - which would otherwise sit in plain text in
//     the tool_call preview printed to the terminal and -o log file (see
//     dispatchToolCall in main.go), and could be exfiltrated to an
//     attacker-chosen direct-mode URL by a prompt-injected instruction.
var reservedAPIRequestHeaders = map[string]bool{
	"host":              true,
	"content-length":    true,
	"transfer-encoding": true,
	"connection":        true,
	"authorization":     true,
}

func isReservedRequestHeader(name string) bool {
	return reservedAPIRequestHeaders[strings.ToLower(strings.TrimSpace(name))]
}

// allowedAPIRequestMethods is the full set api_request ever sends,
// regardless of AllowMutating - anything else (TRACE, CONNECT, a typo)
// is rejected outright rather than passed through to net/http.
var allowedAPIRequestMethods = map[string]bool{
	http.MethodGet:     true,
	http.MethodHead:    true,
	http.MethodOptions: true,
	http.MethodPost:    true,
	http.MethodPut:     true,
	http.MethodPatch:   true,
	http.MethodDelete:  true,
}

func isMutatingMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

// ─────────────────────────────────────────────────────────────────
// Tool implementation
// ─────────────────────────────────────────────────────────────────

func toolAPIRequest(args map[string]interface{}, cfg apiRequestConfig) (string, error) {
	if !cfg.enabled() {
		return "", fmt.Errorf("api_request ถูกปิดใช้งานสำหรับเซสชันนี้ (ตั้ง OLA_API_ENDPOINTS/--api-endpoints หรือเปิด --api-allow-direct-url/OLA_API_ALLOW_DIRECT_URL ก่อน)")
	}

	method := strings.ToUpper(strings.TrimSpace(stringArg(args, "method")))
	if method == "" {
		method = http.MethodGet
	}
	if !allowedAPIRequestMethods[method] {
		return "", fmt.Errorf("method %q ไม่รองรับ (รองรับเฉพาะ GET/HEAD/OPTIONS/POST/PUT/PATCH/DELETE)", method)
	}
	if isMutatingMethod(method) && !cfg.AllowMutating {
		return "", fmt.Errorf(
			"method %s ต้องเปิด --api-allow-mutating/OLA_API_ALLOW_MUTATING ก่อน "+
				"(ค่า default อนุญาตแค่ GET/HEAD/OPTIONS เพื่อกันเรียก API ที่มีผลข้างเคียงโดยไม่ตั้งใจ)", method)
	}

	target, endpointAlias, err := resolveAPIRequestTarget(args, cfg)
	if err != nil {
		return "", err
	}
	if q := stringMapArg(args["query"]); len(q) > 0 {
		qs := target.Query()
		for k, v := range q {
			qs.Set(k, v)
		}
		target.RawQuery = qs.Encode()
	}

	bodyType := strings.ToLower(strings.TrimSpace(stringArg(args, "body_type")))
	bodyReader, contentType, err := buildAPIRequestBody(bodyType, args["body"], stringsFromArg(args["multipart_files"]), cfg.MaxRequestBytes)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequest(method, target.String(), bodyReader)
	if err != nil {
		return "", fmt.Errorf("สร้าง request ไม่สำเร็จ: %w", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range stringMapArg(args["headers"]) {
		if isReservedRequestHeader(k) {
			continue
		}
		req.Header.Set(k, v)
	}
	// Endpoint auth is applied LAST, after any model-set headers, so a
	// model-supplied header (even if it somehow matched the same name)
	// can never shadow the operator-configured credential.
	if endpointAlias != "" {
		if ep := cfg.Endpoints[endpointAlias]; ep.AuthHeader != "" {
			req.Header.Set(ep.AuthHeader, ep.AuthValue)
		}
	}

	client := &http.Client{Timeout: cfg.Timeout}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("เรียก API ไม่สำเร็จ: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, cfg.MaxResponseBytes))
	if err != nil {
		return "", fmt.Errorf("อ่าน response body ไม่ได้: %w", err)
	}
	return formatAPIResponse(resp, body), nil
}

// resolveAPIRequestTarget picks the destination URL for one call, either
// from a pre-approved endpoint alias (+ optional path) or, only when
// AllowDirectURL is on, a raw model-supplied URL run through the exact
// same SSRF guard web_fetch uses (validateFetchURL, main.go).
func resolveAPIRequestTarget(args map[string]interface{}, cfg apiRequestConfig) (target *url.URL, endpointAlias string, err error) {
	endpointAlias = strings.TrimSpace(stringArg(args, "endpoint"))
	rawURL := strings.TrimSpace(stringArg(args, "url"))

	switch {
	case endpointAlias != "" && rawURL != "":
		return nil, "", fmt.Errorf("ระบุได้แค่ endpoint หรือ url อย่างใดอย่างหนึ่ง ไม่ใช่ทั้งคู่")

	case endpointAlias != "":
		ep, ok := cfg.Endpoints[endpointAlias]
		if !ok {
			return nil, "", fmt.Errorf("ไม่รู้จัก endpoint %q - endpoint ที่อนุญาตไว้: %s", endpointAlias, cfg.endpointList())
		}
		full, err := joinEndpointPath(ep.BaseURL, stringArg(args, "path"))
		if err != nil {
			return nil, "", err
		}
		u, err := url.Parse(full)
		if err != nil {
			return nil, "", fmt.Errorf("ประกอบ URL จาก endpoint %q ไม่สำเร็จ: %w", endpointAlias, err)
		}
		return u, endpointAlias, nil

	case rawURL != "":
		if !cfg.AllowDirectURL {
			return nil, "", fmt.Errorf(
				"การระบุ url ตรงถูกปิดใช้งาน (เปิดด้วย --api-allow-direct-url/OLA_API_ALLOW_DIRECT_URL) - " +
					"ใช้ endpoint ที่ config ไว้แทน (ดู endpoint ที่มีได้จาก error เมื่อระบุ endpoint ผิด)")
		}
		if err := validateFetchURL(rawURL); err != nil {
			return nil, "", err
		}
		u, err := url.Parse(rawURL)
		if err != nil {
			return nil, "", fmt.Errorf("URL ไม่ถูกต้อง: %w", err)
		}
		return u, "", nil

	default:
		return nil, "", fmt.Errorf("ต้องระบุ endpoint (+path ถ้าต้องการ) หรือ url อย่างใดอย่างหนึ่ง")
	}
}

// joinEndpointPath combines an endpoint's trusted BaseURL with a
// model-supplied "path" - deliberately parsing p purely for its
// Path/RawQuery and discarding any Scheme/Host it might contain, so a
// path like "http://evil.example/x" can never redirect the request to a
// different host: net/url parses "evil.example" as p's Host, which this
// function never looks at, leaving only "/x" as the effective path.
func joinEndpointPath(base, p string) (string, error) {
	bu, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("endpoint base URL ไม่ถูกต้อง: %w", err)
	}
	if p == "" {
		return bu.String(), nil
	}
	pu, err := url.Parse(p)
	if err != nil {
		return "", fmt.Errorf("path ไม่ถูกต้อง: %w", err)
	}
	joined := *bu
	joined.Path = singleJoiningSlash(bu.Path, pu.Path)
	if pu.RawQuery != "" {
		joined.RawQuery = pu.RawQuery
	}
	return joined.String(), nil
}

// singleJoiningSlash joins two path segments with exactly one "/" between
// them, the same small helper net/http/httputil's reverse proxy uses for
// the identical "base path + sub path" problem.
func singleJoiningSlash(a, b string) string {
	aSlash := strings.HasSuffix(a, "/")
	bSlash := strings.HasPrefix(b, "/")
	switch {
	case aSlash && bSlash:
		return a + b[1:]
	case !aSlash && !bSlash && b != "":
		return a + "/" + b
	default:
		return a + b
	}
}

// ─────────────────────────────────────────────────────────────────
// Body encoding
// ─────────────────────────────────────────────────────────────────

// buildAPIRequestBody encodes args["body"] (plus, for multipart,
// args["multipart_files"]) according to bodyType, returning a ready
// io.Reader and the Content-Type header that goes with it. Every branch
// enforces maxBytes itself (after encoding, since json/form encoding can
// grow the effective size) rather than relying on the caller to check
// once at the end.
func buildAPIRequestBody(bodyType string, bodyArg interface{}, files []string, maxBytes int64) (io.Reader, string, error) {
	switch bodyType {
	case "", "none":
		return nil, "", nil

	case "json":
		if bodyArg == nil {
			return nil, "", fmt.Errorf("body_type เป็น json ต้องระบุ body")
		}
		data, err := json.Marshal(bodyArg)
		if err != nil {
			return nil, "", fmt.Errorf("แปลง body เป็น JSON ไม่ได้: %w", err)
		}
		if int64(len(data)) > maxBytes {
			return nil, "", fmt.Errorf("body ใหญ่เกินกำหนด (%d bytes, จำกัด %d bytes)", len(data), maxBytes)
		}
		return bytes.NewReader(data), "application/json", nil

	case "form":
		m := stringMapArg(bodyArg)
		if len(m) == 0 {
			return nil, "", fmt.Errorf("body_type เป็น form ต้องระบุ body เป็น object ของ key:value")
		}
		vals := url.Values{}
		for k, v := range m {
			vals.Set(k, v)
		}
		encoded := vals.Encode()
		if int64(len(encoded)) > maxBytes {
			return nil, "", fmt.Errorf("body ใหญ่เกินกำหนด (%d bytes, จำกัด %d bytes)", len(encoded), maxBytes)
		}
		return strings.NewReader(encoded), "application/x-www-form-urlencoded", nil

	case "multipart":
		return buildMultipartRequestBody(bodyArg, files, maxBytes)

	case "text":
		s, ok := bodyArg.(string)
		if !ok || s == "" {
			return nil, "", fmt.Errorf("body_type เป็น text ต้องระบุ body เป็น string")
		}
		if int64(len(s)) > maxBytes {
			return nil, "", fmt.Errorf("body ใหญ่เกินกำหนด (%d bytes, จำกัด %d bytes)", len(s), maxBytes)
		}
		return strings.NewReader(s), "text/plain; charset=utf-8", nil

	case "binary":
		s, ok := bodyArg.(string)
		if !ok || s == "" {
			return nil, "", fmt.Errorf("body_type เป็น binary ต้องระบุ body เป็น base64 string")
		}
		data, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return nil, "", fmt.Errorf("decode base64 ไม่สำเร็จ: %w", err)
		}
		if int64(len(data)) > maxBytes {
			return nil, "", fmt.Errorf("body ใหญ่เกินกำหนด (%d bytes หลัง decode, จำกัด %d bytes)", len(data), maxBytes)
		}
		return bytes.NewReader(data), "application/octet-stream", nil

	default:
		return nil, "", fmt.Errorf("body_type ไม่รู้จัก: %q (ต้องเป็น json/form/multipart/text/binary/none)", bodyType)
	}
}

// buildMultipartRequestBody writes both plain fields (from bodyArg, same
// object shape as "form") and file attachments (from files, each resolved
// through sandboxedPath - the same working-directory confinement
// toolReadFile uses) into one multipart/form-data body. Each attached
// file gets its own numbered field name ("file0", "file1", ...) so
// multiple files never collide on a shared field name.
func buildMultipartRequestBody(bodyArg interface{}, files []string, maxBytes int64) (io.Reader, string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	for k, v := range stringMapArg(bodyArg) {
		if err := mw.WriteField(k, v); err != nil {
			return nil, "", fmt.Errorf("เขียน multipart field %q ไม่ได้: %w", k, err)
		}
	}

	for i, rel := range files {
		full, err := sandboxedPath(rel)
		if err != nil {
			return nil, "", fmt.Errorf("multipart_files: %w", err)
		}
		f, err := os.Open(full)
		if err != nil {
			return nil, "", fmt.Errorf("เปิดไฟล์ %s ไม่ได้: %w", rel, err)
		}
		part, err := mw.CreateFormFile(fmt.Sprintf("file%d", i), filepath.Base(full))
		if err != nil {
			f.Close()
			return nil, "", fmt.Errorf("สร้าง multipart file field สำหรับ %s ไม่ได้: %w", rel, err)
		}
		_, copyErr := io.Copy(part, f)
		f.Close()
		if copyErr != nil {
			return nil, "", fmt.Errorf("อ่านไฟล์ %s ไม่สำเร็จ: %w", rel, copyErr)
		}
		if int64(buf.Len()) > maxBytes {
			return nil, "", fmt.Errorf("multipart body ใหญ่เกินกำหนด (จำกัด %d bytes) หลังแนบ %s", maxBytes, rel)
		}
	}

	if err := mw.Close(); err != nil {
		return nil, "", fmt.Errorf("ปิด multipart writer ไม่สำเร็จ: %w", err)
	}
	if int64(buf.Len()) > maxBytes {
		return nil, "", fmt.Errorf("multipart body ใหญ่เกินกำหนด (%d bytes, จำกัด %d bytes)", buf.Len(), maxBytes)
	}
	return &buf, mw.FormDataContentType(), nil
}

// ─────────────────────────────────────────────────────────────────
// Response formatting
// ─────────────────────────────────────────────────────────────────

// formatAPIResponse renders one HTTP response for the model: status line,
// a small fixed set of response headers worth surfacing by default
// (Content-Type/Content-Length/Location - not the full header set, which
// is long and mostly irrelevant), then the body. A binary/non-text body
// is deliberately NOT included (only its size + content-type are
// reported) so a large binary response can never blow the context budget
// via an accidental base64 dump; a text/json/xml body is truncated to
// maxAPIResultOutput, same as web_fetch's own result truncation.
func formatAPIResponse(resp *http.Response, body []byte) string {
	var b strings.Builder
	fmt.Fprintf(&b, "HTTP %d %s\n", resp.StatusCode, http.StatusText(resp.StatusCode))
	for _, h := range []string{"Content-Type", "Content-Length", "Location"} {
		if v := resp.Header.Get(h); v != "" {
			fmt.Fprintf(&b, "%s: %s\n", h, v)
		}
	}
	b.WriteString("\n")

	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	switch {
	case len(body) == 0:
		b.WriteString("(response body ว่างเปล่า)")
	case strings.Contains(ct, "json") || strings.Contains(ct, "text") || strings.Contains(ct, "xml") || ct == "":
		b.WriteString(truncateText(string(body), maxAPIResultOutput))
	default:
		fmt.Fprintf(&b, "(response body เป็น binary/ไม่ใช่ข้อความ - %d bytes, content-type %q - ไม่แสดงเนื้อหา)", len(body), resp.Header.Get("Content-Type"))
	}
	return b.String()
}

// formatAPIRequestNotification renders a one-line summary of a mutating
// api_request call for the session change log / ntfy.sh notification -
// same rationale and truncateWords/maxNotificationWords budget as
// formatFileChangeNotification (write_file/edit_file) and
// formatSCPNotification (scp_copy) use for their own side-effecting calls.
func formatAPIRequestNotification(args map[string]interface{}) string {
	method := strings.ToUpper(strings.TrimSpace(stringArg(args, "method")))
	target := stringArg(args, "endpoint")
	if target != "" {
		if p := stringArg(args, "path"); p != "" {
			target += p
		}
	} else {
		target = stringArg(args, "url")
	}
	return truncateWords(fmt.Sprintf("[API:%s] %s", method, target), maxNotificationWords)
}

// ─────────────────────────────────────────────────────────────────
// Small arg-decoding helpers shared by this file
// ─────────────────────────────────────────────────────────────────

// stringArg reads a string-typed tool argument, returning "" for a
// missing key or a value of the wrong type rather than panicking - same
// permissive-decode approach stringsFromArg (integrations.go) uses.
func stringArg(args map[string]interface{}, key string) string {
	s, _ := args[key].(string)
	return s
}

// stringMapArg converts a JSON-decoded object argument (map[string]interface{},
// as produced by json.Unmarshal into map[string]interface{}) into a clean
// map[string]string. Non-string scalar values (numbers/bools) are
// coerced via fmt.Sprint for convenience, since local models sometimes
// emit an unquoted number for something like a query param; nested
// objects/arrays are dropped since neither query strings nor form/header
// values have a sane string representation for those.
func stringMapArg(v interface{}) map[string]string {
	raw, _ := v.(map[string]interface{})
	if len(raw) == 0 {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, val := range raw {
		switch t := val.(type) {
		case string:
			out[k] = t
		case float64, bool:
			out[k] = fmt.Sprint(t)
		}
	}
	return out
}
