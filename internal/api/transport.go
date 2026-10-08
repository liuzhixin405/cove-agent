package api

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// endpoint is the HTTP side of a provider request: one POST with its key,
// the connection phase of a stream, key-pool bookkeeping and the status
// classification. The providers keep only their wire formats. The two
// implementations used to carry their own copies of this, and the copies
// had drifted: a client timeout was retried by one and not the other, and
// one read SSE lines of any length.
type endpoint struct {
	url    string
	client *http.Client
	key    func() string
	pool   *KeyPool
	// auth sets the provider's authentication and version headers.
	auth func(h http.Header, key string)
}

// maxResponseBytes bounds a non-streamed response body.
const maxResponseBytes = 10 * 1024 * 1024

// errCreateRequest marks a request that could not be built, so post tells
// it apart from a transport failure (both can be a *url.Error).
var errCreateRequest = errors.New("create request")

// newRequest builds the POST of data with the provider's headers.
func (ep endpoint) newRequest(ctx context.Context, data []byte, key string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", ep.url, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errCreateRequest, err)
	}
	req.Header.Set("Content-Type", "application/json")
	ep.auth(req.Header, key)
	return req, nil
}

// send makes one request with the pool's next key and records the outcome
// in the pool (rate-limited / dead / ok), so multi-key rotation fails over.
//
// A 401/403 is the key's fault while another key in the pool is still
// alive, so the request goes out again at once with the next key. It used to
// come back as the reply: the dead key was marked, but the caller saw a
// non-retryable auth error, so one revoked key in a three-key pool failed the
// turn and the fallback chain blacklisted the provider for the session. Only
// when no live key remains is the auth error returned.
func (ep endpoint) send(ctx context.Context, data []byte) (*http.Response, error) {
	attempts := max(1, ep.pool.size())
	for i := 1; ; i++ {
		key := ep.key()
		req, err := ep.newRequest(ctx, data, key)
		if err != nil {
			return nil, err
		}
		resp, err := ep.client.Do(req)
		if err != nil {
			return nil, err
		}
		ep.pool.MarkOutcome(key, resp.StatusCode, RetryAfterFor(resp.StatusCode, resp.Header))
		if isAuthStatus(resp.StatusCode) && i < attempts && ep.pool.hasLiveKey() {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 16<<10))
			_ = resp.Body.Close()
			continue
		}
		return resp, nil
	}
}

func isAuthStatus(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden
}

// response is a non-streamed reply: its body (bounded), status and headers
// (the rate-limit headers are read from them).
type response struct {
	Body   []byte
	Status int
	Header http.Header
}

// post sends one request and returns a response that is not a server error
// or a rate limit; those, and transport failures
// before the response, are a *RetryableError for retryWithBackoff. A client
// timeout is not retried: the request went out and the model generated for
// the whole timeout, so a retry repeats that wait for most likely the same
// outcome.
func (ep endpoint) post(ctx context.Context, data []byte) (response, error) {
	resp, err := ep.send(ctx, data)
	if err != nil {
		if errors.Is(err, errCreateRequest) {
			return response{}, err
		}
		if isClientTimeout(err) {
			return response{}, fmt.Errorf("http: %w", err)
		}
		return response{}, &RetryableError{Msg: fmt.Sprintf("http: %v", err)}
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return response{}, fmt.Errorf("read response body: %w", err)
	}
	if len(raw) > maxResponseBytes {
		return response{}, fmt.Errorf("response body exceeds %d bytes", maxResponseBytes)
	}
	// 529 is Anthropic's "overloaded"; it falls under >= 500.
	if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
		wait := RetryAfterFor(resp.StatusCode, resp.Header)
		if wait == 0 && resp.StatusCode == http.StatusTooManyRequests {
			wait = retryAfterFromBody(string(raw))
		}
		return response{}, &RetryableError{Msg: truncate(string(raw), 500), Status: resp.StatusCode, RetryAfter: wait}
	}
	return response{Body: raw, Status: resp.StatusCode, Header: resp.Header}, nil
}

// openStream establishes a streamed request, retrying the connection phase
// only: once the body streams, deltas have reached the handler and a retry
// would duplicate output. The caller checks the status and closes the body.
func (ep endpoint) openStream(ctx context.Context, data []byte) (*http.Response, error) {
	return retryConnectHTTP(
		ctx,
		defaultRetry,
		// send marks every attempt's key, as post does. Only the final
		// response used to be: each retry takes the next key and the 429s in
		// between were swallowed, so a rate-limited key was never cooled down
		// and kept being handed out.
		func(callCtx context.Context) (*http.Response, error) { return ep.send(callCtx, data) },
		func(statusCode int) bool { return statusCode >= 500 || statusCode == http.StatusTooManyRequests },
	)
}

// maxSSELineBytes bounds one SSE line: a whole file's content can arrive as
// a single tool-call argument delta, but a line without end is not a stream.
const maxSSELineBytes = 10 * 1024 * 1024

// readSSELine is reader.ReadString('\n') with the line bounded by
// maxSSELineBytes: the last line of a stream comes back with io.EOF, like
// ReadString.
func readSSELine(r *bufio.Reader) (string, error) {
	var line []byte
	for {
		chunk, err := r.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > maxSSELineBytes {
			return "", fmt.Errorf("SSE line longer than %d bytes", maxSSELineBytes)
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return string(line), err
	}
}
