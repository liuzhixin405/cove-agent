package api

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A client timeout is not retried by either provider: the model generated
// for the whole timeout, and a retry repeats that wait. The OpenAI-compatible
// one used to retry it, the Anthropic one did not.
func TestClientTimeoutIsNotRetried(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		time.Sleep(300 * time.Millisecond)
	}))
	defer srv.Close()
	for _, name := range []string{"openai-compatible", "anthropic"} {
		hits.Store(0)
		p := NewProvider(ProviderConfig{Name: name, APIKey: "k", BaseURL: srv.URL})
		setClientTimeout(t, p, 50*time.Millisecond)
		if _, err := p.Chat(context.Background(), ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "hi"}}}); err == nil {
			t.Fatalf("%s: no error on a timed-out request", name)
		}
		if n := hits.Load(); n != 1 {
			t.Errorf("%s: a timed-out request was sent %d times", name, n)
		}
	}
}

func setClientTimeout(t *testing.T, p Provider, d time.Duration) {
	t.Helper()
	switch v := p.(type) {
	case *openAICompatProvider:
		v.client = &http.Client{Timeout: d}
	case *anthropicProvider:
		v.client = &http.Client{Timeout: d}
	default:
		t.Fatalf("unexpected provider %T", p)
	}
}

// readSSELine reads like ReadString('\n') (the last line comes with io.EOF)
// and refuses a line over maxSSELineBytes instead of growing without end.
func TestReadSSELine(t *testing.T) {
	r := bufio.NewReaderSize(strings.NewReader("data: a\ndata: b"), 16)
	if l, err := readSSELine(r); l != "data: a\n" || err != nil {
		t.Fatalf("first line = %q, %v", l, err)
	}
	if l, err := readSSELine(r); l != "data: b" || !errors.Is(err, io.EOF) {
		t.Fatalf("last line = %q, %v", l, err)
	}
	long := bufio.NewReaderSize(strings.NewReader("data: "+strings.Repeat("x", maxSSELineBytes)+"\n"), 4096)
	if _, err := readSSELine(long); err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("an over-long line was accepted: %v", err)
	}
}

type bodyReadTransport struct{ body io.ReadCloser }

func (transport bodyReadTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: transport.body}, nil
}

type failingBodyReader struct{ err error }

func (reader failingBodyReader) Read([]byte) (int, error) { return 0, reader.err }

type trackedResponseBody struct {
	io.Reader
	closed bool
}

func (body *trackedResponseBody) Close() error {
	body.closed = true
	return nil
}

func bodyReadEndpoint(body io.ReadCloser) endpoint {
	return endpoint{
		url: "https://response.test", client: &http.Client{Transport: bodyReadTransport{body: body}},
		key: func() string { return "test" }, auth: func(http.Header, string) {},
	}
}

func TestPostPropagatesBodyReadError(t *testing.T) {
	for _, readErr := range []error{io.ErrUnexpectedEOF, context.Canceled, context.DeadlineExceeded} {
		t.Run(readErr.Error(), func(t *testing.T) {
			body := &trackedResponseBody{Reader: io.MultiReader(strings.NewReader(`{"choices":[]}`), failingBodyReader{err: readErr})}
			_, err := bodyReadEndpoint(body).post(context.Background(), []byte(`{}`))
			if !errors.Is(err, readErr) {
				t.Fatalf("post error = %v, want %v", err, readErr)
			}
			if !body.closed {
				t.Fatal("failed response body was not closed")
			}
		})
	}
}

func TestPostResponseSizeBoundary(t *testing.T) {
	for _, size := range []int{maxResponseBytes, maxResponseBytes + 1} {
		body := &trackedResponseBody{Reader: strings.NewReader(strings.Repeat("x", size))}
		response, err := bodyReadEndpoint(body).post(context.Background(), []byte(`{}`))
		if size == maxResponseBytes {
			if err != nil || len(response.Body) != size {
				t.Fatalf("boundary response: bytes=%d err=%v", len(response.Body), err)
			}
		} else if err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("oversized response error = %v", err)
		}
		if !body.closed {
			t.Fatal("response body was not closed")
		}
	}
}
