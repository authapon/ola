// cdp.go - a minimal, dependency-free Chrome DevTools Protocol (CDP) client,
// used as a third web_fetch mode: instead of running JavaScript via an
// external scrape service (shim mode) or not running it at all (direct
// mode), ola can talk straight to a Chromium/Chrome instance's built-in
// remote-debugging HTTP+WebSocket interface - the SAME interface Playwright
// itself talks to under the hood - with no Playwright, no Node.js driver,
// and no wrapper service required on your side.
//
// Point --fetch-cdp-url / OLA_FETCH_CDP_BASE at the browser's remote
// debugging HTTP port (e.g. "http://localhost:9222", Chrome's default
// DevTools port; a Playwright/Chromium container that already has this
// port published needs no other changes). The browser needs to have been
// launched with something along these lines:
//
//	chromium --headless --remote-debugging-address=0.0.0.0 \
//	         --remote-debugging-port=9222 --remote-allow-origins=*
//
// --remote-allow-origins=* matters: modern Chrome/Chromium versions refuse
// the DevTools WebSocket upgrade from anything that isn't loopback unless
// this flag is set, as a DNS-rebinding protection. This is far and away the
// most common reason CDP mode fails to connect - if fetchOneCDP reports the
// handshake was rejected, check this flag first.
//
// This file implements just enough of RFC 6455 (WebSocket framing and
// client handshake) and CDP (Page/Runtime/Target domains) to: open a new
// tab, navigate it, wait for the page to finish loading, pull out the
// rendered title and visible text via Runtime.evaluate (document.title /
// document.body.innerText - actual rendered, JS-executed values, not a
// regex over raw HTML), and close the tab again. It is deliberately not a
// general-purpose WebSocket or CDP library, only the small slice of both
// protocols web_fetch needs - no permessage-deflate, no concurrent in-flight
// CDP commands, no browser-launching (it only ever connects to a browser
// that's already running somewhere).
package main

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ─────────────────────────────────────────────────────────────────
// Minimal RFC 6455 WebSocket client
// ─────────────────────────────────────────────────────────────────

const (
	wsOpContinuation = 0x0
	wsOpText         = 0x1
	wsOpBinary       = 0x2
	wsOpClose        = 0x8
	wsOpPing         = 0x9
	wsOpPong         = 0xA

	wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
)

type wsConn struct {
	conn net.Conn
	br   *bufio.Reader
}

// dialWebSocket performs a minimal RFC 6455 client handshake against rawURL
// (ws:// or wss://) and returns a connection ready for writeText/
// readMessage. No extensions are negotiated - Chrome's DevTools endpoint
// doesn't require any.
func dialWebSocket(rawURL string, timeout time.Duration) (*wsConn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("CDP websocket URL ไม่ถูกต้อง: %w", err)
	}
	if u.Scheme != "ws" && u.Scheme != "wss" {
		return nil, fmt.Errorf("CDP websocket URL ต้องเป็น ws:// หรือ wss://, ได้ %q", u.Scheme)
	}
	host := u.Host
	if !strings.Contains(host, ":") {
		if u.Scheme == "wss" {
			host += ":443"
		} else {
			host += ":80"
		}
	}

	dialer := net.Dialer{Timeout: timeout}
	var conn net.Conn
	if u.Scheme == "wss" {
		conn, err = tls.DialWithDialer(&dialer, "tcp", host, nil)
	} else {
		conn, err = dialer.Dial("tcp", host)
	}
	if err != nil {
		return nil, fmt.Errorf("เชื่อมต่อ CDP websocket ไม่สำเร็จ (%s): %w", host, err)
	}

	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		conn.Close()
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(keyBytes)

	path := u.Path
	if path == "" {
		path = "/"
	}
	if u.RawQuery != "" {
		path += "?" + u.RawQuery
	}

	req := fmt.Sprintf(
		"GET %s HTTP/1.1\r\n"+
			"Host: %s\r\n"+
			"Upgrade: websocket\r\n"+
			"Connection: Upgrade\r\n"+
			"Sec-WebSocket-Key: %s\r\n"+
			"Sec-WebSocket-Version: 13\r\n\r\n",
		path, u.Host, key)

	_ = conn.SetDeadline(time.Now().Add(timeout))
	if _, err := conn.Write([]byte(req)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("ส่ง websocket handshake ไม่สำเร็จ: %w", err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("อ่าน websocket handshake response ไม่สำเร็จ: %w", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		conn.Close()
		return nil, fmt.Errorf(
			"CDP websocket handshake ถูกปฏิเสธ: HTTP %d (เช็คว่าเปิดเบราว์เซอร์ด้วย --remote-allow-origins=* "+
				"และ --remote-debugging-address=0.0.0.0 หรือยัง)", resp.StatusCode)
	}
	if got, want := resp.Header.Get("Sec-WebSocket-Accept"), computeWSAccept(key); got != want {
		conn.Close()
		return nil, fmt.Errorf("websocket handshake ไม่ผ่านการตรวจสอบ Sec-WebSocket-Accept")
	}

	_ = conn.SetDeadline(time.Time{}) // clear the handshake deadline; callers set their own per-op deadlines
	return &wsConn{conn: conn, br: br}, nil
}

func computeWSAccept(key string) string {
	h := sha1.New()
	h.Write([]byte(key + wsGUID))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func (c *wsConn) writeText(payload []byte) error {
	return c.writeFrame(wsOpText, payload)
}

// writeFrame writes one unfragmented, masked frame - client-to-server
// WebSocket frames MUST be masked per RFC 6455 §5.3, or a spec-compliant
// server (Chrome included) will close the connection.
func (c *wsConn) writeFrame(opcode byte, payload []byte) error {
	var header []byte
	header = append(header, 0x80|opcode) // FIN=1, RSV=0, opcode

	length := len(payload)
	const maskBit = byte(0x80)
	switch {
	case length <= 125:
		header = append(header, maskBit|byte(length))
	case length <= 65535:
		lenBuf := make([]byte, 2)
		binary.BigEndian.PutUint16(lenBuf, uint16(length))
		header = append(header, maskBit|126)
		header = append(header, lenBuf...)
	default:
		lenBuf := make([]byte, 8)
		binary.BigEndian.PutUint64(lenBuf, uint64(length))
		header = append(header, maskBit|127)
		header = append(header, lenBuf...)
	}

	maskKey := make([]byte, 4)
	if _, err := rand.Read(maskKey); err != nil {
		return err
	}
	header = append(header, maskKey...)

	masked := make([]byte, length)
	for i, b := range payload {
		masked[i] = b ^ maskKey[i%4]
	}

	if _, err := c.conn.Write(header); err != nil {
		return err
	}
	_, err := c.conn.Write(masked)
	return err
}

// readMessage reads one complete message, transparently reassembling
// fragmented (continuation-frame) messages and answering pings with pongs,
// until a text/binary message is fully received or an error/close occurs.
func (c *wsConn) readMessage(deadline time.Time) (opcode int, payload []byte, err error) {
	var messageOpcode int
	var buf []byte

	for {
		if err = c.conn.SetReadDeadline(deadline); err != nil {
			return 0, nil, err
		}
		var op int
		var fin bool
		var frame []byte
		op, fin, frame, err = c.readFrame()
		if err != nil {
			return 0, nil, err
		}
		switch op {
		case wsOpContinuation:
			buf = append(buf, frame...)
		case wsOpText, wsOpBinary:
			messageOpcode = op
			buf = append(buf, frame...)
		case wsOpClose:
			return 0, nil, io.EOF
		case wsOpPing:
			if werr := c.writeFrame(wsOpPong, frame); werr != nil {
				return 0, nil, werr
			}
			continue
		case wsOpPong:
			continue
		default:
			return 0, nil, fmt.Errorf("unexpected websocket opcode 0x%x", op)
		}
		if fin {
			return messageOpcode, buf, nil
		}
	}
}

// readFrame reads and unmasks (if needed) exactly one WebSocket frame.
func (c *wsConn) readFrame() (opcode int, fin bool, payload []byte, err error) {
	head := make([]byte, 2)
	if _, err = io.ReadFull(c.br, head); err != nil {
		return
	}
	fin = head[0]&0x80 != 0
	opcode = int(head[0] & 0x0F)
	masked := head[1]&0x80 != 0
	length := int64(head[1] & 0x7F)

	switch length {
	case 126:
		ext := make([]byte, 2)
		if _, err = io.ReadFull(c.br, ext); err != nil {
			return
		}
		length = int64(binary.BigEndian.Uint16(ext))
	case 127:
		ext := make([]byte, 8)
		if _, err = io.ReadFull(c.br, ext); err != nil {
			return
		}
		length = int64(binary.BigEndian.Uint64(ext))
	}
	if length < 0 || length > maxFetchDownloadBytes {
		err = fmt.Errorf("websocket frame ใหญ่เกินไป (%d bytes)", length)
		return
	}

	var maskKey []byte
	if masked {
		maskKey = make([]byte, 4)
		if _, err = io.ReadFull(c.br, maskKey); err != nil {
			return
		}
	}

	payload = make([]byte, length)
	if _, err = io.ReadFull(c.br, payload); err != nil {
		return
	}
	if masked {
		for i := range payload {
			payload[i] ^= maskKey[i%4]
		}
	}
	return
}

func (c *wsConn) close() {
	_ = c.writeFrame(wsOpClose, nil)
	_ = c.conn.Close()
}

// ─────────────────────────────────────────────────────────────────
// CDP HTTP endpoints: /json/new and /json/close
// ─────────────────────────────────────────────────────────────────

type cdpTargetInfo struct {
	ID                   string `json:"id"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

// cdpNewTarget asks the browser's HTTP debugging endpoint to open a new
// blank tab and returns its target id and websocket debugger URL.
func cdpNewTarget(client *http.Client, httpBase string) (cdpTargetInfo, error) {
	var info cdpTargetInfo
	req, err := http.NewRequest(http.MethodGet, httpBase+"/json/new?about:blank", nil)
	if err != nil {
		return info, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return info, fmt.Errorf("เปิด tab ใหม่ผ่าน CDP ไม่สำเร็จ (เช็คว่า --fetch-cdp-url ชี้ไปยัง Chrome remote-debugging port ที่รันอยู่จริงหรือไม่): %w", err)
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		return info, readErr
	}
	if resp.StatusCode != http.StatusOK {
		return info, fmt.Errorf("CDP /json/new ตอบ HTTP %d: %s", resp.StatusCode, truncateText(string(body), 300))
	}
	if err := json.Unmarshal(body, &info); err != nil {
		return info, fmt.Errorf("แปลง response ของ CDP /json/new ไม่ได้: %w", err)
	}
	if info.WebSocketDebuggerURL == "" {
		return info, fmt.Errorf("CDP /json/new ไม่ได้คืน webSocketDebuggerUrl มาด้วย")
	}
	return info, nil
}

// cdpCloseTarget is best-effort cleanup: a failure here must never surface
// as a web_fetch error, since by this point the actual fetch has already
// succeeded or failed on its own terms - an orphaned tab is a (minor,
// eventually-recycled) resource leak, not a correctness problem.
func cdpCloseTarget(client *http.Client, httpBase, targetID string) {
	if targetID == "" {
		return
	}
	req, err := http.NewRequest(http.MethodGet, httpBase+"/json/close/"+targetID, nil)
	if err != nil {
		return
	}
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	_ = resp.Body.Close()
}

// ─────────────────────────────────────────────────────────────────
// CDP command/event messaging over the websocket
// ─────────────────────────────────────────────────────────────────

type cdpCommand struct {
	ID     int         `json:"id"`
	Method string      `json:"method"`
	Params interface{} `json:"params,omitempty"`
}

type cdpMessage struct {
	ID     int             `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// cdpSession wraps a websocket connection with a simple synchronous
// request/response helper (call) plus a way to wait for a named event
// (waitForEvent). This is enough for the strictly sequential exchange
// web_fetch needs (enable -> navigate -> wait for load -> evaluate x2) - no
// concurrent in-flight commands are ever needed for a single fetch.
type cdpSession struct {
	ws     *wsConn
	nextID int
}

func (s *cdpSession) call(deadline time.Time, method string, params interface{}) (json.RawMessage, error) {
	s.nextID++
	id := s.nextID
	payload, err := json.Marshal(cdpCommand{ID: id, Method: method, Params: params})
	if err != nil {
		return nil, err
	}
	if err := s.ws.writeText(payload); err != nil {
		return nil, err
	}
	for {
		_, raw, err := s.ws.readMessage(deadline)
		if err != nil {
			return nil, err
		}
		var msg cdpMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue // not a well-formed CDP message frame - ignore and keep reading
		}
		if msg.ID != id {
			continue // an event, or a (shouldn't happen here) reply to an earlier call
		}
		if msg.Error != nil {
			return nil, fmt.Errorf("CDP %s ล้มเหลว: %s", method, msg.Error.Message)
		}
		return msg.Result, nil
	}
}

func (s *cdpSession) waitForEvent(deadline time.Time, method string) error {
	for {
		_, raw, err := s.ws.readMessage(deadline)
		if err != nil {
			return err
		}
		var msg cdpMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue
		}
		if msg.Method == method {
			return nil
		}
	}
}

type cdpEvalResult struct {
	Result struct {
		Value string `json:"value"`
	} `json:"result"`
}

// ─────────────────────────────────────────────────────────────────
// Top-level: fetchOneCDP
// ─────────────────────────────────────────────────────────────────

// fetchOneCDP drives a real Chromium/Chrome tab - over the browser's own
// remote-debugging protocol, the same one Playwright uses internally - to
// load rawURL, wait for it to finish loading, and pull out the rendered
// title and visible text. Unlike direct mode this DOES execute JavaScript,
// since an actual browser is doing the rendering; unlike shim mode it needs
// no external wrapper service, only the browser's own debugging port.
func fetchOneCDP(httpClient *http.Client, httpBase, rawURL string, timeout time.Duration) (string, error) {
	if err := validateFetchURL(rawURL); err != nil {
		return "", err
	}

	target, err := cdpNewTarget(httpClient, httpBase)
	if err != nil {
		return "", err
	}
	defer cdpCloseTarget(httpClient, httpBase, target.ID)

	ws, err := dialWebSocket(target.WebSocketDebuggerURL, timeout)
	if err != nil {
		return "", err
	}
	defer ws.close()

	sess := &cdpSession{ws: ws}
	deadline := time.Now().Add(timeout)

	if _, err := sess.call(deadline, "Page.enable", nil); err != nil {
		return "", fmt.Errorf("CDP Page.enable ล้มเหลว: %w", err)
	}
	if _, err := sess.call(deadline, "Page.navigate", map[string]string{"url": rawURL}); err != nil {
		return "", fmt.Errorf("CDP Page.navigate ล้มเหลว: %w", err)
	}
	if err := sess.waitForEvent(deadline, "Page.loadEventFired"); err != nil {
		return "", fmt.Errorf("หน้าเว็บโหลดไม่เสร็จภายในเวลาที่กำหนด (--fetch-timeout): %w", err)
	}

	titleRaw, err := sess.call(deadline, "Runtime.evaluate", map[string]interface{}{
		"expression": "document.title", "returnByValue": true,
	})
	if err != nil {
		return "", fmt.Errorf("CDP อ่าน document.title ไม่สำเร็จ: %w", err)
	}
	textRaw, err := sess.call(deadline, "Runtime.evaluate", map[string]interface{}{
		"expression":    "document.body ? document.body.innerText : ''",
		"returnByValue": true,
	})
	if err != nil {
		return "", fmt.Errorf("CDP อ่านเนื้อหาหน้าเว็บไม่สำเร็จ: %w", err)
	}

	var titleResult, textResult cdpEvalResult
	_ = json.Unmarshal(titleRaw, &titleResult)
	_ = json.Unmarshal(textRaw, &textResult)

	text := strings.TrimSpace(textResult.Result.Value)
	if text == "" {
		return "", fmt.Errorf("หน้านี้ไม่มีข้อความให้ดึงแม้จะ render ด้วยเบราว์เซอร์จริงแล้ว (อาจเป็นหน้าที่โหลดข้อมูลช้ากว่า load event, ลองเพิ่ม --fetch-timeout)")
	}
	header := ""
	if titleResult.Result.Value != "" {
		header = "# " + titleResult.Result.Value + "\n\n"
	}
	return truncateText(header+text, maxWebResultOutput), nil
}
