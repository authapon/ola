package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ─────────────────────────────────────────────────────────────────
// WebSocket framing correctness
// ─────────────────────────────────────────────────────────────────

// TestComputeWSAcceptMatchesRFC6455Vector checks our Sec-WebSocket-Accept
// computation against the official worked example from RFC 6455 §1.3, so
// the handshake math itself is verified independent of any server we talk
// to.
func TestComputeWSAcceptMatchesRFC6455Vector(t *testing.T) {
	got := computeWSAccept("dGhlIHNhbXBsZSBub25jZQ==")
	want := "s3pPLMBiTxaQ9kYGzzhZRbK+xOo="
	if got != want {
		t.Fatalf("computeWSAccept mismatch: got %q, want %q (RFC 6455 §1.3 test vector)", got, want)
	}
}

// writeUnmaskedFrame writes a single unfragmented server->client frame.
// Test-only: server-to-client WebSocket frames must NOT be masked (unlike
// client frames, see wsConn.writeFrame in cdp.go, which is client-only) -
// this is the mirror-image helper needed to play "server" in these tests.
func writeUnmaskedFrame(conn net.Conn, opcode byte, payload []byte) error {
	var header []byte
	header = append(header, 0x80|opcode)
	length := len(payload)
	switch {
	case length <= 125:
		header = append(header, byte(length))
	case length <= 65535:
		lenBuf := make([]byte, 2)
		binary.BigEndian.PutUint16(lenBuf, uint16(length))
		header = append(header, 126)
		header = append(header, lenBuf...)
	default:
		lenBuf := make([]byte, 8)
		binary.BigEndian.PutUint64(lenBuf, uint64(length))
		header = append(header, 127)
		header = append(header, lenBuf...)
	}
	if _, err := conn.Write(header); err != nil {
		return err
	}
	_, err := conn.Write(payload)
	return err
}

// TestWebSocketFrameRoundTripSmallPayload proves a client-written masked
// text frame is correctly framed and can be read back (with correct
// unmasking) by something playing the server role - readFrame is generic
// over masked/unmasked frames, so this also validates the exact code path
// the fake CDP server below reuses to read our client's commands.
func TestWebSocketFrameRoundTripSmallPayload(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	client := &wsConn{conn: clientConn, br: bufio.NewReader(clientConn)}

	type result struct {
		opcode  int
		payload []byte
		err     error
	}
	done := make(chan result, 1)
	go func() {
		server := &wsConn{conn: serverConn, br: bufio.NewReader(serverConn)}
		op, payload, err := server.readMessage(time.Now().Add(2 * time.Second))
		done <- result{op, payload, err}
	}()

	if err := client.writeText([]byte("hello cdp")); err != nil {
		t.Fatalf("writeText failed: %v", err)
	}
	r := <-done
	if r.err != nil {
		t.Fatalf("server-side readMessage failed: %v", r.err)
	}
	if r.opcode != wsOpText {
		t.Fatalf("expected text opcode 0x%x, got 0x%x", wsOpText, r.opcode)
	}
	if string(r.payload) != "hello cdp" {
		t.Fatalf("payload mismatch: got %q, want %q", r.payload, "hello cdp")
	}
}

// TestWebSocketFrameRoundTripLargePayload forces the 64-bit extended
// payload-length path (>65535 bytes) on both the write and read side.
func TestWebSocketFrameRoundTripLargePayload(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	client := &wsConn{conn: clientConn, br: bufio.NewReader(clientConn)}
	large := strings.Repeat("x", 70000)

	type result struct {
		payload []byte
		err     error
	}
	done := make(chan result, 1)
	go func() {
		server := &wsConn{conn: serverConn, br: bufio.NewReader(serverConn)}
		_, payload, err := server.readMessage(time.Now().Add(3 * time.Second))
		done <- result{payload, err}
	}()

	if err := client.writeText([]byte(large)); err != nil {
		t.Fatalf("writeText failed: %v", err)
	}
	r := <-done
	if r.err != nil {
		t.Fatalf("server-side readMessage failed: %v", r.err)
	}
	if string(r.payload) != large {
		t.Fatalf("large payload round-trip mismatch: got %d bytes, want %d", len(r.payload), len(large))
	}
}

// TestWebSocketPingIsAnsweredWithPong confirms readMessage transparently
// answers a ping with a pong and keeps waiting for the next real message,
// rather than surfacing the ping as if it were the message the caller
// asked for.
func TestWebSocketPingIsAnsweredWithPong(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	client := &wsConn{conn: clientConn, br: bufio.NewReader(clientConn)}

	go func() {
		_ = writeUnmaskedFrame(serverConn, wsOpPing, []byte("ping-payload"))
		server := &wsConn{conn: serverConn, br: bufio.NewReader(serverConn)}
		// Consume exactly the client's pong reply. Deliberately calling the
		// low-level readFrame (not readMessage) here: readMessage is
		// designed to transparently absorb control frames while it keeps
		// waiting for an actual text/binary message, so using it just to
		// "read one pong" would block until the deadline instead of
		// returning - that behavior is correct for real CDP usage, just
		// not what this line wants.
		_ = server.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, _, _, _ = server.readFrame()
		_ = writeUnmaskedFrame(serverConn, wsOpText, []byte("real message"))
	}()

	op, payload, err := client.readMessage(time.Now().Add(2 * time.Second))
	if err != nil {
		t.Fatalf("readMessage failed: %v", err)
	}
	if op != wsOpText || string(payload) != "real message" {
		t.Fatalf("expected the ping to be absorbed and the real message returned, got opcode 0x%x payload %q", op, payload)
	}
}

// ─────────────────────────────────────────────────────────────────
// Fake CDP server: emulates just enough of Chrome's HTTP+WebSocket remote
// debugging interface (/json/new, /json/close, and a WS endpoint speaking
// Page.enable/Page.navigate/Runtime.evaluate) to drive fetchOneCDP through
// a complete, realistic round trip without needing an actual browser.
// ─────────────────────────────────────────────────────────────────

type fakeCDPServer struct {
	srv   *httptest.Server
	title string
	text  string
	// httpStatus/omitWSURL let individual tests simulate failure modes at
	// the /json/new step without needing a second server type.
	newTargetStatus int
	omitWSURL       bool
}

func newFakeCDPServer(title, text string) *fakeCDPServer {
	f := &fakeCDPServer{title: title, text: text, newTargetStatus: http.StatusOK}
	mux := http.NewServeMux()
	mux.HandleFunc("/json/new", func(w http.ResponseWriter, r *http.Request) {
		if f.newTargetStatus != http.StatusOK {
			w.WriteHeader(f.newTargetStatus)
			return
		}
		resp := map[string]string{"id": "T1"}
		if !f.omitWSURL {
			resp["webSocketDebuggerUrl"] = "ws://" + r.Host + "/devtools/page/T1"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("/json/close/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/devtools/page/T1", f.serveWS)
	f.srv = httptest.NewServer(mux)
	return f
}

func (f *fakeCDPServer) close()           { f.srv.Close() }
func (f *fakeCDPServer) httpBase() string { return f.srv.URL }

func (f *fakeCDPServer) serveWS(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Sec-WebSocket-Key")
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		return
	}
	defer conn.Close()

	accept := computeWSAccept(key)
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
	if _, err := rw.WriteString(resp); err != nil {
		return
	}
	if err := rw.Flush(); err != nil {
		return
	}

	ws := &wsConn{conn: conn, br: rw.Reader}
	for {
		_, raw, err := ws.readMessage(time.Now().Add(5 * time.Second))
		if err != nil {
			return
		}
		var cmd struct {
			ID     int             `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(raw, &cmd); err != nil {
			continue
		}
		switch cmd.Method {
		case "Page.enable":
			f.sendResult(conn, cmd.ID, map[string]interface{}{})
		case "Page.navigate":
			f.sendResult(conn, cmd.ID, map[string]interface{}{"frameId": "F1"})
			f.sendEvent(conn, "Page.loadEventFired", map[string]interface{}{})
		case "Runtime.evaluate":
			var params struct {
				Expression string `json:"expression"`
			}
			_ = json.Unmarshal(cmd.Params, &params)
			value := f.text
			if strings.Contains(params.Expression, "title") {
				value = f.title
			}
			f.sendResult(conn, cmd.ID, map[string]interface{}{
				"result": map[string]interface{}{"type": "string", "value": value},
			})
		default:
			f.sendResult(conn, cmd.ID, map[string]interface{}{})
		}
	}
}

func (f *fakeCDPServer) sendResult(conn net.Conn, id int, result interface{}) {
	payload, _ := json.Marshal(map[string]interface{}{"id": id, "result": result})
	_ = writeUnmaskedFrame(conn, wsOpText, payload)
}

func (f *fakeCDPServer) sendEvent(conn net.Conn, method string, params interface{}) {
	payload, _ := json.Marshal(map[string]interface{}{"method": method, "params": params})
	_ = writeUnmaskedFrame(conn, wsOpText, payload)
}

// ─────────────────────────────────────────────────────────────────
// fetchOneCDP end-to-end against the fake server
// ─────────────────────────────────────────────────────────────────

func TestFetchOneCDPFullRoundTrip(t *testing.T) {
	srv := newFakeCDPServer("Fake Page Title", "This is the rendered page text, straight from a real browser's DOM.")
	defer srv.close()

	client := &http.Client{Timeout: 5 * time.Second}
	result, err := fetchOneCDP(client, srv.httpBase(), "https://example.com/some-article", 5*time.Second)
	if err != nil {
		t.Fatalf("fetchOneCDP failed: %v", err)
	}
	if !strings.Contains(result, "Fake Page Title") {
		t.Fatalf("expected the title to be included, got: %s", result)
	}
	if !strings.Contains(result, "This is the rendered page text") {
		t.Fatalf("expected the rendered text to be included, got: %s", result)
	}
}

func TestFetchOneCDPRejectsPrivateTargetURL(t *testing.T) {
	srv := newFakeCDPServer("Title", "Text")
	defer srv.close()

	client := &http.Client{Timeout: 5 * time.Second}
	_, err := fetchOneCDP(client, srv.httpBase(), "http://127.0.0.1:8080/internal", 5*time.Second)
	if err == nil {
		t.Fatal("expected the SSRF guard to reject a private-IP fetch target even in CDP mode")
	}
}

func TestFetchOneCDPEmptyTextAfterRenderErrors(t *testing.T) {
	srv := newFakeCDPServer("Title", "") // rendered page has no visible text
	defer srv.close()

	client := &http.Client{Timeout: 5 * time.Second}
	_, err := fetchOneCDP(client, srv.httpBase(), "https://example.com/blank", 5*time.Second)
	if err == nil {
		t.Fatal("expected an error when the rendered page has no visible text")
	}
}

func TestFetchOneCDPNewTargetHTTPError(t *testing.T) {
	srv := newFakeCDPServer("Title", "Text")
	srv.newTargetStatus = http.StatusInternalServerError
	defer srv.close()

	client := &http.Client{Timeout: 5 * time.Second}
	_, err := fetchOneCDP(client, srv.httpBase(), "https://example.com/", 5*time.Second)
	if err == nil {
		t.Fatal("expected an error when /json/new fails")
	}
}

func TestFetchOneCDPNewTargetMissingWebSocketURL(t *testing.T) {
	srv := newFakeCDPServer("Title", "Text")
	srv.omitWSURL = true
	defer srv.close()

	client := &http.Client{Timeout: 5 * time.Second}
	_, err := fetchOneCDP(client, srv.httpBase(), "https://example.com/", 5*time.Second)
	if err == nil {
		t.Fatal("expected an error when /json/new omits webSocketDebuggerUrl")
	}
}

// TestToolWebFetchCDPModeThroughDispatcher confirms the mode-selection
// dispatcher (fetchOne, driven from toolWebFetch) actually reaches CDP mode
// when OLA_FETCH_CDP_BASE/--fetch-cdp-url is the only fetch mode configured.
func TestToolWebFetchCDPModeThroughDispatcher(t *testing.T) {
	srv := newFakeCDPServer("Dispatched Title", "Dispatched body text.")
	defer srv.close()

	cfg := resolveSearchConfig("", "", srv.httpBase(), false, 0, 0, 1, 0, 5, false)
	if !cfg.fetchUsesCDP() {
		t.Fatal("expected fetchUsesCDP() true when only FetchCDPBase is set")
	}

	result, err := toolWebFetch(map[string]interface{}{"urls": []interface{}{"https://example.com/page"}}, cfg)
	if err != nil {
		t.Fatalf("toolWebFetch failed: %v", err)
	}
	if !strings.Contains(result, "Dispatched Title") || !strings.Contains(result, "Dispatched body text") {
		t.Fatalf("expected CDP-mode content in the dispatched result, got: %s", result)
	}
}
