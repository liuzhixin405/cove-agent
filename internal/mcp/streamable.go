package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/liuzhixin405/cove-agent/internal/log"
)

// streamableHTTPTransport implements the MCP Streamable HTTP transport
// (spec 2025-03-26). Every client message is a POST to the one endpoint, and
// the server answers each request on that POST's own response - either as a
// plain application/json body or as an SSE stream. An optional GET stream
// carries server-initiated messages.
type streamableHTTPTransport struct {
	baseURL string
	client  *http.Client
	msgChan chan json.RawMessage
	ctx     context.Context
	cancel  context.CancelFunc
	mu      sync.Mutex
	stream  io.ReadCloser // active GET stream body

	// sessionID is issued by the server on the initialize response and must
	// accompany every later request; without it session-based servers answer
	// 400 to everything after initialize.
	sessionID string
	// streamSession is the session the GET stream was last attempted for, so
	// it is retried once a session exists but not on every POST.
	streamSession string
}

// NewStreamableHTTPTransport connects to an MCP server using the
// Streamable HTTP transport.
//
// The GET stream is opened best-effort. It is optional in the spec, and the
// SDK servers refuse it before initialize (400: no session) or altogether
// (405 in stateless mode); failing the connection on that made cove unable to
// talk to them at all. A dead or wrong URL still fails, at the initialize POST.
func NewStreamableHTTPTransport(endpoint string) (*streamableHTTPTransport, error) {
	ctx, cancel := context.WithCancel(context.Background())
	t := &streamableHTTPTransport{
		baseURL: strings.TrimRight(endpoint, "/"),
		// No overall Timeout: it also covers reading the body, so a tool call
		// that ran longer than it had its response cut off mid-stream. Every
		// request carries the caller's context instead (the handshake deadline,
		// the turn's cancellation).
		client:  &http.Client{Transport: headerBoundedTransport(sseEndpointTimeout)},
		msgChan: make(chan json.RawMessage, 64),
		ctx:     ctx,
		cancel:  cancel,
	}
	if err := t.openStream(); err != nil {
		log.Debugf("streamablehttp: no GET stream (%v); continuing without one", err)
	}
	return t, nil
}

// openStream establishes a long-lived GET connection for server→client messages.
func (t *streamableHTTPTransport) openStream() error {
	req, err := http.NewRequestWithContext(t.ctx, "GET", t.baseURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	t.mu.Lock()
	sid := t.sessionID
	t.streamSession = sid
	t.mu.Unlock()
	if sid != "" {
		req.Header.Set("Mcp-Session-Id", sid)
	}

	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		_ = resp.Body.Close()
		return fmt.Errorf("streamablehttp: server returned %d", resp.StatusCode)
	}

	t.mu.Lock()
	if t.stream != nil {
		_ = t.stream.Close()
	}
	t.stream = resp.Body
	t.mu.Unlock()

	go t.readStream(resp.Body)
	return nil
}

// readStream continuously reads SSE events from the GET stream.
func (t *streamableHTTPTransport) readStream(body io.ReadCloser) {
	defer func() {
		t.mu.Lock()
		if t.stream == body {
			t.stream = nil
		}
		t.mu.Unlock()
		_ = body.Close()
	}()
	err := readSSE(body, t.deliverEvent)
	// Our own shutdown cancels the stream; that is not worth a warning, and a
	// WARN is shown to the user on every /mcp disconnect and at exit.
	if err != nil && !errors.Is(err, io.EOF) && t.ctx.Err() == nil {
		log.Warnf("streamablehttp: read error: %v", err)
	}
}

// deliverEvent hands one SSE message event to Receive. It blocks until the
// message is consumed (backpressure) rather than dropping it - a dropped
// JSON-RPC response hangs its caller - and gives up only on shutdown.
func (t *streamableHTTPTransport) deliverEvent(ev sseEvent) bool {
	if !isMessageEvent(ev) {
		return true
	}
	return t.deliver(json.RawMessage(ev.Data))
}

func (t *streamableHTTPTransport) deliver(msg json.RawMessage) bool {
	select {
	case t.msgChan <- msg:
		return true
	case <-t.ctx.Done():
		return false
	}
}

// Send posts a JSON-RPC message to the server and routes whatever the server
// answers on that response back to Receive.
func (t *streamableHTTPTransport) Send(ctx context.Context, msg any) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	// The request must also end when the transport is closed, or a reader of a
	// long SSE response outlives the transport.
	reqCtx, cancelReq := context.WithCancel(ctx)
	stop := context.AfterFunc(t.ctx, cancelReq)
	release := func() { stop(); cancelReq() }

	req, err := http.NewRequestWithContext(reqCtx, "POST", t.baseURL, bytes.NewReader(data))
	if err != nil {
		release()
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	t.mu.Lock()
	sid := t.sessionID
	t.mu.Unlock()
	if sid != "" {
		req.Header.Set("Mcp-Session-Id", sid)
	}

	resp, err := t.client.Do(req)
	if err != nil {
		release()
		return err
	}

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		release()
		return fmt.Errorf("streamablehttp: POST %d: %s", resp.StatusCode, string(body))
	}

	if newSID := resp.Header.Get("Mcp-Session-Id"); newSID != "" {
		t.mu.Lock()
		t.sessionID = newSID
		retry := t.stream == nil && t.streamSession != newSID
		t.mu.Unlock()
		if retry {
			go func() {
				if err := t.openStream(); err != nil {
					log.Debugf("streamablehttp: no GET stream (%v); continuing without one", err)
				}
			}()
		}
	}

	ct := resp.Header.Get("Content-Type")
	switch {
	case strings.Contains(ct, "text/event-stream"):
		// The reader goroutine owns the body. Closing it here (a deferred
		// close) raced the reads and truncated the streamed response.
		go func() {
			defer release()
			defer func() { _ = resp.Body.Close() }()
			_ = readSSE(resp.Body, t.deliverEvent)
		}()
		return nil
	case strings.Contains(ct, "application/json"):
		// The body IS the response. It used to be closed unread, so against a
		// server answering in JSON mode initialize never completed.
		defer release()
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("streamablehttp: read response: %w", err)
		}
		t.deliverBody(body)
		return nil
	default:
		// 202 Accepted for notifications and responses: nothing to read.
		_ = resp.Body.Close()
		release()
		return nil
	}
}

// deliverBody routes a JSON response body: one message, or a JSON-RPC batch.
func (t *streamableHTTPTransport) deliverBody(body []byte) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return
	}
	if body[0] == '[' {
		var batch []json.RawMessage
		if err := json.Unmarshal(body, &batch); err == nil {
			for _, m := range batch {
				if !t.deliver(m) {
					return
				}
			}
			return
		}
	}
	t.deliver(json.RawMessage(body))
}

// Receive returns the next server→client message.
func (t *streamableHTTPTransport) Receive(ctx context.Context) (json.RawMessage, error) {
	select {
	case msg := <-t.msgChan:
		return msg, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-t.ctx.Done():
		return nil, io.EOF
	}
}

// Close shuts down the transport.
func (t *streamableHTTPTransport) Close() error {
	t.cancel()
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stream != nil {
		_ = t.stream.Close()
		t.stream = nil
	}
	return nil
}
