package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func testHub() (*Hub, Snapshot) {
	h := NewHub()
	s := Snapshot{Scope: Scope{Session: "s", Project: "p", Task: "t", Version: 1}, Running: true, Evidence: "task evidence"}
	h.Publish(s)
	return h, s
}

func TestHTTPActionPath(t *testing.T) {
	h, s := testHub()
	handler, err := NewHandler(h, testToken, []string{"http://127.0.0.1:9999"})
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path string, body []byte) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://127.0.0.1:9999"+path, bytes.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+testToken)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	a := Action{ID: "steer-1", Scope: s.Scope, Kind: "steer", Text: "only inspect"}
	body, _ := json.Marshal(a)
	if w := request("POST", "/v1/actions", body); w.Code != 202 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	calls := 0
	h.Drain(func() Snapshot { return s }, func(got Action) error {
		calls++
		if got != a {
			t.Fatalf("action %+v", got)
		}
		return nil
	})
	if calls != 1 {
		t.Fatalf("calls=%d", calls)
	}
	if w := request("POST", "/v1/actions", body); w.Code != 409 {
		t.Fatalf("replay %d", w.Code)
	}
	if w := request("GET", "/v1/actions/steer-1", nil); w.Code != 200 || !strings.Contains(w.Body.String(), `"applied"`) {
		t.Fatalf("result %d %s", w.Code, w.Body)
	}
	if w := request("GET", "/v1/status", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "task evidence") {
		t.Fatalf("snapshot %d %s", w.Code, w.Body)
	}
}

func TestHTTPGuards(t *testing.T) {
	h, _ := testHub()
	handler, err := NewHandler(h, testToken, []string{"http://127.0.0.1:9999", "https://phone.example"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, host, auth, origin, site, path, method, content, body string
		want                                                        int
	}{
		{name: "no-auth", want: 401},
		{name: "wrong-auth", auth: "Bearer wrong", want: 401},
		{name: "query-auth", path: "/v1/status?token=secret", auth: "Bearer " + testToken, want: 403},
		{name: "host", host: "evil.example", auth: "Bearer " + testToken, want: 403},
		{name: "origin", origin: "https://evil.example", auth: "Bearer " + testToken, want: 403},
		{name: "host-origin-mismatch", origin: "https://phone.example", auth: "Bearer " + testToken, want: 403},
		{name: "fetch-site", site: "same-site", auth: "Bearer " + testToken, want: 403},
		{name: "preflight", method: "OPTIONS", auth: "Bearer " + testToken, want: 404},
		{name: "wrong-content", method: "POST", path: "/v1/actions", body: `{}`, auth: "Bearer " + testToken, want: 415},
		{name: "oversize", method: "POST", path: "/v1/actions", content: "application/json", body: strings.Repeat(" ", 16385), auth: "Bearer " + testToken, want: 400},
		{name: "trailing", method: "POST", path: "/v1/actions", content: "application/json", body: `{} {}`, auth: "Bearer " + testToken, want: 400},
		{name: "unknown", method: "POST", path: "/v1/actions", content: "application/json", body: `{"shell":"whoami"}`, auth: "Bearer " + testToken, want: 400},
		{name: "same-origin", origin: "http://127.0.0.1:9999", auth: "Bearer " + testToken, want: 200},
		{name: "tunnel", host: "phone.example", origin: "https://phone.example", auth: "Bearer " + testToken, want: 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			method, path := tc.method, tc.path
			if method == "" {
				method = "GET"
			}
			if path == "" {
				path = "/v1/status"
			}
			r := httptest.NewRequest(method, "http://127.0.0.1:9999"+path, strings.NewReader(tc.body))
			if tc.host != "" {
				r.Host = tc.host
			}
			if tc.auth != "" {
				r.Header.Set("Authorization", tc.auth)
			}
			r.Header.Set("Origin", tc.origin)
			r.Header.Set("Sec-Fetch-Site", tc.site)
			r.Header.Set("Content-Type", tc.content)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("code=%d want=%d body=%s", w.Code, tc.want, w.Body)
			}
			if w.Header().Get("Access-Control-Allow-Origin") != "" {
				t.Fatal("CORS enabled")
			}
		})
	}
}

func TestBindAndLifecycle(t *testing.T) {
	for _, cfg := range []Config{
		{Address: "0.0.0.0:0"}, {Address: "[::]:0"}, {Address: "8.8.8.8:0", AllowLAN: true},
		{Address: "192.168.1.2:0"}, {Address: "192.168.1.2:0", AllowLAN: true},
		{PublicOrigin: "http://phone.example"}, {PublicOrigin: "https://phone.example/path"},
	} {
		if server, err := Start(cfg, NewHub()); err == nil {
			server.Stop(context.Background())
			t.Fatalf("unsafe config accepted: %+v", cfg)
		}
	}
	h, _ := testHub()
	path := filepath.Join(t.TempDir(), "credential")
	server, err := Start(Config{TokenFile: path}, h)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Stop(context.Background()) })
	token, err := os.ReadFile(path)
	if err != nil || len(strings.TrimSpace(string(token))) != 64 {
		t.Fatal("credential not created")
	}
	request, _ := http.NewRequest("GET", server.URL+"/v1/status", nil)
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	client := &http.Client{Timeout: time.Second}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("status=%d", response.StatusCode)
	}
	if duplicate, err := Start(Config{TokenFile: path}, NewHub()); err == nil {
		duplicate.Stop(context.Background())
		t.Fatal("credential overwritten")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := server.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("credential not removed")
	}
	if _, err := h.Submit(Action{}); err != ErrUnavailable {
		t.Fatal("stopped hub still accepting")
	}
}
