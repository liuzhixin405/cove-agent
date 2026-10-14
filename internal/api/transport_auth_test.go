package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// auth may fail (a signer without credentials). That failure is a request
// that could not be built: no HTTP call, no retry, on both send paths.
func TestEndpointAuthErrorIsNotSentOrRetried(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer server.Close()
	wantErr := errors.New("no aws credentials")
	ep := endpoint{
		url:    server.URL + "/v1/messages",
		client: server.Client(),
		key:    func() string { return "" },
		auth: func(req *http.Request, data []byte, key string) error {
			return wantErr
		},
	}
	if _, err := ep.post(context.Background(), []byte(`{}`)); !errors.Is(err, wantErr) || !errors.Is(err, errCreateRequest) {
		t.Fatalf("post error = %v, want wrapped %v and errCreateRequest", err, wantErr)
	}
	if _, err := ep.openStream(context.Background(), []byte(`{}`)); !errors.Is(err, wantErr) {
		t.Fatalf("openStream error = %v, want %v", err, wantErr)
	}
	if hits.Load() != 0 {
		t.Fatalf("server was reached %d times, want 0", hits.Load())
	}
}

// The auth hook sees the request and the body it was built from.
func TestEndpointAuthSeesRequestAndBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Test-Len") != "2" || r.Header.Get("X-Test-Host") != r.Host {
			t.Errorf("auth did not see the request: len=%q host=%q", r.Header.Get("X-Test-Len"), r.Header.Get("X-Test-Host"))
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	ep := endpoint{
		url:    server.URL + "/v1/messages",
		client: server.Client(),
		key:    func() string { return "k" },
		auth: func(req *http.Request, data []byte, key string) error {
			req.Header.Set("X-Test-Len", string(rune('0'+len(data))))
			req.Header.Set("X-Test-Host", req.URL.Host)
			return nil
		},
	}
	if _, err := ep.post(context.Background(), []byte(`{}`)); err != nil {
		t.Fatalf("post: %v", err)
	}
}
