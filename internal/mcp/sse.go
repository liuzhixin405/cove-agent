package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// httpPostTimeout bounds a POST whose answer is only an acknowledgement (the
// SSE transport's responses arrive on the stream, not on the POST).
var httpPostTimeout = 30 * time.Second

// sseEndpointTimeout bounds the wait for the server's "endpoint" event.
var sseEndpointTimeout = 30 * time.Second

// sseTransport implements the HTTP+SSE transport (spec 2024-11-05): the client
// GETs the configured URL as an event stream, the server's first event
// ("endpoint") names the URL to POST messages to, and every server message -
// responses included - arrives on the stream.
//
// It used to POST to "<url>/sse" for a JSON {"sessionId"} and then GET
// "<url>/message": a protocol no real server speaks, so "type": "sse" could
// not connect to anything.
type sseTransport struct {
	sseURL   string
	endpoint string // absolute POST URL from the endpoint event
	client   *http.Client
	msgChan  chan json.RawMessage
	ctx      context.Context
	cancel   context.CancelFunc
}

func NewSSETransport(sseURL string) (*sseTransport, error) {
	ctx, cancel := context.WithCancel(context.Background())
	t := &sseTransport{
		sseURL:  sseURL,
		client:  &http.Client{Timeout: httpPostTimeout},
		msgChan: make(chan json.RawMessage, 64),
		ctx:     ctx,
		cancel:  cancel,
	}

	req, err := http.NewRequestWithContext(ctx, "GET", sseURL, nil)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("sse connect: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	// No timeout on the long-lived stream itself, but the response headers
	// must arrive: a server that accepts the connection and never answers
	// used to hang LoadFromConfig (and so startup) and /mcp connect for good.
	resp, err := (&http.Client{Transport: headerBoundedTransport(sseEndpointTimeout)}).Do(req)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("sse connect: %w", err)
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		_ = resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("sse connect: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	endpointCh := make(chan string, 1)
	streamDone := make(chan struct{})
	go func() {
		defer close(streamDone)
		t.listenSSE(resp.Body, endpointCh)
	}()

	timer := time.NewTimer(sseEndpointTimeout)
	defer timer.Stop()
	var raw string
	select {
	case raw = <-endpointCh:
	case <-streamDone:
		cancel()
		return nil, fmt.Errorf("sse: stream closed before the server sent its endpoint event")
	case <-timer.C:
		cancel()
		return nil, fmt.Errorf("sse: no endpoint event within %v (is %s an SSE endpoint?)", sseEndpointTimeout, sseURL)
	}

	endpoint, err := resolveSSEEndpoint(sseURL, raw)
	if err != nil {
		cancel()
		return nil, err
	}
	t.endpoint = endpoint
	return t, nil
}

// resolveSSEEndpoint resolves the endpoint event against the SSE URL and
// refuses one on another origin: the event is server-controlled, and following
// it elsewhere would send every later request - arguments included - to a
// host the user never configured.
func resolveSSEEndpoint(sseURL, raw string) (string, error) {
	base, err := url.Parse(sseURL)
	if err != nil {
		return "", fmt.Errorf("sse: bad url %q: %w", sseURL, err)
	}
	ref, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("sse: bad endpoint %q: %w", raw, err)
	}
	abs := base.ResolveReference(ref)
	if abs.Scheme != base.Scheme || abs.Host != base.Host {
		return "", fmt.Errorf("sse: endpoint %q is on a different origin than %s; refusing to follow it", raw, sseURL)
	}
	return abs.String(), nil
}

// listenSSE reads the stream: the first endpoint event is handed to the
// constructor, message events to Receive. When the stream ends the transport
// is shut down, so Receive reports EOF and the client knows it is dead.
func (t *sseTransport) listenSSE(body io.ReadCloser, endpointCh chan<- string) {
	defer t.cancel()
	defer func() { _ = body.Close() }()
	sentEndpoint := false
	_ = readSSE(body, func(ev sseEvent) bool {
		if ev.Type == "endpoint" {
			if !sentEndpoint {
				sentEndpoint = true
				endpointCh <- ev.Data
			}
			return true
		}
		if !isMessageEvent(ev) {
			return true
		}
		// Block until the message is consumed (backpressure) rather than
		// dropping it; dropping a JSON-RPC response would hang the caller
		// until its timeout. Unblock on transport shutdown.
		select {
		case t.msgChan <- json.RawMessage(ev.Data):
			return true
		case <-t.ctx.Done():
			return false
		}
	})
}

func (t *sseTransport) Send(ctx context.Context, msg any) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", t.endpoint, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	// A non-2xx here (expired/invalid session, server error) means the message
	// was not delivered. Surface it instead of returning nil, otherwise the
	// caller blocks waiting for a response that will never arrive.
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("sse send: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func (t *sseTransport) Receive(ctx context.Context) (json.RawMessage, error) {
	select {
	case msg := <-t.msgChan:
		return msg, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-t.ctx.Done():
		return nil, io.EOF
	}
}

func (t *sseTransport) Close() error {
	t.cancel()
	return nil
}

// headerBoundedTransport is the default transport with a bound on how long a
// server may take to send response headers; the body (an event stream) stays
// unbounded.
func headerBoundedTransport(timeout time.Duration) http.RoundTripper {
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		t := base.Clone()
		t.ResponseHeaderTimeout = timeout
		return t
	}
	return http.DefaultTransport
}
