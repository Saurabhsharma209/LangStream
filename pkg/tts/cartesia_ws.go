package tts

// This file implements just enough of RFC 6455 (the WebSocket protocol)
// to speak Cartesia's streaming TTS API: a client-side handshake plus
// masked frame writes / frame reads with fragmentation, ping/pong, and
// close handling. It is intentionally minimal (no compression
// extensions, no server-push subprotocol negotiation) rather than a
// general-purpose WebSocket library, since pkg/tts has no external
// dependencies today (see go.mod) and this package must not introduce
// one just to make one outbound connection type.

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// WebSocket opcodes (RFC 6455 section 5.2).
const (
	wsOpContinuation byte = 0x0
	wsOpText         byte = 0x1
	wsOpBinary       byte = 0x2
	wsOpClose        byte = 0x8
	wsOpPing         byte = 0x9
	wsOpPong         byte = 0xA
)

// wsGUID is the fixed magic string used to compute Sec-WebSocket-Accept
// from the client's Sec-WebSocket-Key, per RFC 6455 section 1.3.
const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// maxWSFramePayload bounds a single frame's declared payload length so a
// misbehaving or malicious server can't force an unbounded allocation.
// Cartesia audio chunks are small (sub-second of PCM); 16MiB is generous
// headroom.
const maxWSFramePayload = 16 * 1024 * 1024

// wsHandshakeStatusError records the HTTP status code a WebSocket
// handshake was rejected with, so a caller (SynthesizeStream's retry
// logic, via isRetryableStatusCode) can classify it as a transient
// vendor-side condition (429/5xx) worth retrying or a permanent client
// error (other 4xx: bad auth, bad request, ...) that a retry cannot fix,
// without parsing dialWS's formatted error string.
type wsHandshakeStatusError struct {
	StatusCode int
}

func (e *wsHandshakeStatusError) Error() string {
	return fmt.Sprintf("websocket handshake rejected: status %d", e.StatusCode)
}

// dialWS opens a client WebSocket connection to rawURL (scheme "ws" or
// "wss"), sending the given extra headers (e.g. auth) during the
// handshake. It returns the underlying connection and a *bufio.Reader
// that must be used for all subsequent reads, since the handshake may
// have already buffered bytes belonging to the first frame.
func dialWS(ctx context.Context, rawURL string, header http.Header) (net.Conn, *bufio.Reader, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, nil, fmt.Errorf("tts/cartesia: invalid websocket url %q: %w", rawURL, err)
	}

	var useTLS bool
	switch u.Scheme {
	case "ws":
		useTLS = false
	case "wss":
		useTLS = true
	default:
		return nil, nil, fmt.Errorf("tts/cartesia: unsupported websocket scheme %q", u.Scheme)
	}

	host := u.Host
	if _, _, err := net.SplitHostPort(host); err != nil {
		if useTLS {
			host = net.JoinHostPort(host, "443")
		} else {
			host = net.JoinHostPort(host, "80")
		}
	}

	var dialer net.Dialer
	rawConn, err := dialer.DialContext(ctx, "tcp", host)
	if err != nil {
		return nil, nil, fmt.Errorf("tts/cartesia: dialing %s: %w", host, err)
	}

	var conn net.Conn = rawConn
	if useTLS {
		tlsConn := tls.Client(rawConn, &tls.Config{ServerName: u.Hostname()})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			rawConn.Close()
			return nil, nil, fmt.Errorf("tts/cartesia: tls handshake with %s: %w", host, err)
		}
		conn = tlsConn
	}

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-done:
		}
	}()

	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("tts/cartesia: generating websocket key: %w", err)
	}
	key := base64.StdEncoding.EncodeToString(keyBytes)

	reqURI := u.RequestURI()
	if reqURI == "" {
		reqURI = "/"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "GET %s HTTP/1.1\r\n", reqURI)
	fmt.Fprintf(&b, "Host: %s\r\n", u.Host)
	b.WriteString("Upgrade: websocket\r\n")
	b.WriteString("Connection: Upgrade\r\n")
	fmt.Fprintf(&b, "Sec-WebSocket-Key: %s\r\n", key)
	b.WriteString("Sec-WebSocket-Version: 13\r\n")
	for name, values := range header {
		for _, v := range values {
			fmt.Fprintf(&b, "%s: %s\r\n", name, v)
		}
	}
	b.WriteString("\r\n")

	if _, err := io.WriteString(conn, b.String()); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("tts/cartesia: sending websocket handshake: %w", err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("tts/cartesia: reading websocket handshake response: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		conn.Close()
		return nil, nil, fmt.Errorf("tts/cartesia: websocket handshake rejected: status %d: %s: %w",
			resp.StatusCode, strings.TrimSpace(string(body)), &wsHandshakeStatusError{StatusCode: resp.StatusCode})
	}
	if !strings.EqualFold(resp.Header.Get("Upgrade"), "websocket") {
		conn.Close()
		return nil, nil, fmt.Errorf("tts/cartesia: websocket handshake rejected: unexpected Upgrade header %q", resp.Header.Get("Upgrade"))
	}
	if got, want := resp.Header.Get("Sec-WebSocket-Accept"), computeAcceptKey(key); got != want {
		conn.Close()
		return nil, nil, fmt.Errorf("tts/cartesia: websocket handshake rejected: invalid Sec-WebSocket-Accept")
	}

	return conn, br, nil
}

// computeAcceptKey derives the expected Sec-WebSocket-Accept header value
// for a given Sec-WebSocket-Key, per RFC 6455 section 1.3.
func computeAcceptKey(key string) string {
	h := sha1.New()
	io.WriteString(h, key)
	io.WriteString(h, wsGUID)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// writeWSFrame writes a single, unfragmented, masked client frame (masking
// is mandatory for client-to-server frames per RFC 6455 section 5.1).
func writeWSFrame(w io.Writer, opcode byte, payload []byte) error {
	n := len(payload)
	header := make([]byte, 0, 14)
	header = append(header, 0x80|opcode) // FIN=1, RSV=0

	const maskBit = 0x80
	switch {
	case n <= 125:
		header = append(header, maskBit|byte(n))
	case n <= 65535:
		header = append(header, maskBit|126)
		var l [2]byte
		binary.BigEndian.PutUint16(l[:], uint16(n))
		header = append(header, l[:]...)
	default:
		header = append(header, maskBit|127)
		var l [8]byte
		binary.BigEndian.PutUint64(l[:], uint64(n))
		header = append(header, l[:]...)
	}

	var maskKey [4]byte
	if _, err := rand.Read(maskKey[:]); err != nil {
		return fmt.Errorf("tts/cartesia: generating frame mask: %w", err)
	}
	header = append(header, maskKey[:]...)

	if _, err := w.Write(header); err != nil {
		return fmt.Errorf("tts/cartesia: writing frame header: %w", err)
	}
	if n == 0 {
		return nil
	}
	masked := make([]byte, n)
	for i, bt := range payload {
		masked[i] = bt ^ maskKey[i%4]
	}
	if _, err := w.Write(masked); err != nil {
		return fmt.Errorf("tts/cartesia: writing frame payload: %w", err)
	}
	return nil
}

// readWSFrame reads a single WebSocket frame from br, unmasking the
// payload if the frame's MASK bit is set (server frames normally are not
// masked, but we tolerate it either way).
func readWSFrame(br *bufio.Reader) (fin bool, opcode byte, payload []byte, err error) {
	var head [2]byte
	if _, err = io.ReadFull(br, head[:]); err != nil {
		return false, 0, nil, err
	}
	fin = head[0]&0x80 != 0
	opcode = head[0] & 0x0F
	masked := head[1]&0x80 != 0
	length := int64(head[1] & 0x7F)

	switch length {
	case 126:
		var ext [2]byte
		if _, err = io.ReadFull(br, ext[:]); err != nil {
			return false, 0, nil, err
		}
		length = int64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err = io.ReadFull(br, ext[:]); err != nil {
			return false, 0, nil, err
		}
		length = int64(binary.BigEndian.Uint64(ext[:]))
	}
	if length < 0 || length > maxWSFramePayload {
		return false, 0, nil, fmt.Errorf("tts/cartesia: websocket frame payload too large (%d bytes)", length)
	}

	var maskKey [4]byte
	if masked {
		if _, err = io.ReadFull(br, maskKey[:]); err != nil {
			return false, 0, nil, err
		}
	}

	payload = make([]byte, length)
	if length > 0 {
		if _, err = io.ReadFull(br, payload); err != nil {
			return false, 0, nil, err
		}
	}
	if masked {
		for i := range payload {
			payload[i] ^= maskKey[i%4]
		}
	}
	return fin, opcode, payload, nil
}

// wsErrClosed is returned by readWSMessage once the peer has sent (or we
// have sent) a Close frame and the connection should be torn down.
var wsErrClosed = fmt.Errorf("tts/cartesia: websocket connection closed")

// readWSMessage reads one complete (possibly fragmented) text/binary
// message from br, transparently answering pings with pongs and ignoring
// pongs. On a Close frame it echoes a Close frame back (best-effort) and
// returns wsErrClosed.
func readWSMessage(conn net.Conn, br *bufio.Reader) (opcode byte, payload []byte, err error) {
	var assembled []byte
	var msgOpcode byte
	started := false

	for {
		fin, op, data, ferr := readWSFrame(br)
		if ferr != nil {
			return 0, nil, ferr
		}

		switch op {
		case wsOpPing:
			if werr := writeWSFrame(conn, wsOpPong, data); werr != nil {
				return 0, nil, werr
			}
			continue
		case wsOpPong:
			continue
		case wsOpClose:
			_ = writeWSFrame(conn, wsOpClose, nil)
			return wsOpClose, data, wsErrClosed
		case wsOpContinuation:
			if !started {
				return 0, nil, fmt.Errorf("tts/cartesia: unexpected continuation frame")
			}
			assembled = append(assembled, data...)
			if fin {
				return msgOpcode, assembled, nil
			}
		case wsOpText, wsOpBinary:
			if fin && !started {
				return op, data, nil
			}
			started = true
			msgOpcode = op
			assembled = append(assembled, data...)
			if fin {
				return msgOpcode, assembled, nil
			}
		default:
			return 0, nil, fmt.Errorf("tts/cartesia: unsupported websocket opcode %#x", op)
		}
	}
}
