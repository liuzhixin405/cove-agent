package tool

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestIsPrivateURL covers the SSRF gate. The DNS-failure case (an unresolvable
// host must fail closed) is tested in internal/safeurl with a stubbed
// resolver: resolving a real name here failed on machines whose DNS answers
// every name (fake-IP proxies resolve nonexistent.invalid. to 198.18.0.0/15).
func TestIsPrivateURL(t *testing.T) {
	cases := map[string]bool{
		"http://127.0.0.1":         true,
		"http://localhost":         true,
		"http://169.254.169.254/x": true, // cloud metadata
		"http://10.0.0.5":          true,
		"http://192.168.1.1":       true,
		"https://8.8.8.8":          false, // public literal IP
	}
	for u, want := range cases {
		if got := isPrivateURL(u); got != want {
			t.Errorf("isPrivateURL(%q) = %v, want %v", u, got, want)
		}
	}
}

// TestSafeHTTPClient_BlocksPrivateDial locks in H-1/H-11: the hardened client must
// refuse to connect to a private address even when reached directly. httptest
// servers listen on 127.0.0.1, so a successful GET would mean the dial-time IP
// guard failed (this is also the mechanism that defeats DNS rebinding on redirect).
func TestSafeHTTPClient_BlocksPrivateDial(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("secret"))
	}))
	defer srv.Close()

	client := newSafeHTTPClient(5 * time.Second)
	resp, err := client.Get(srv.URL) // srv.URL is http://127.0.0.1:PORT
	if err == nil {
		resp.Body.Close()
		t.Fatalf("safe client reached private address %s; SSRF dial guard failed", srv.URL)
	}
}

func TestFetchedBodyRefusesBinary(t *testing.T) {
	pdf := append([]byte("%PDF-1.7\n"), make([]byte, 64)...)
	if _, err := fetchedText(pdf, "application/pdf", "markdown"); err == nil {
		t.Fatal("binary body was returned as text")
	}
	got, err := fetchedText([]byte("<html><body><h1>Title</h1><p>hi</p></body></html>"), "text/html; charset=utf-8", "markdown")
	if err != nil || !strings.Contains(got, "# Title") {
		t.Fatalf("html body = %q, %v", got, err)
	}
}

func TestFetchedBodyHTMLParserBoundaries(t *testing.T) {
	tests := []struct {
		name, format, input, want string
	}{
		{"quoted attribute", "text", `<p title="a > b">visible</p>`, "visible"},
		{"unclosed script", "text", `<p>visible</p><script>secret <p>hidden</p>`, "visible"},
		{"unquoted link", "markdown", `<p><a href=/docs?a=1&amp;b=2>Docs</a></p>`, "[Docs](/docs?a=1&b=2)"},
		{"code whitespace", "markdown", "<pre>\tfirst  line\n\n\n  last\n</pre>", "```\n\tfirst  line\n\n\n  last\n```"},
		{"raw HTML", "html", `<p title="a > b">visible</p>`, `<p title="a > b">visible</p>`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := fetchedText([]byte(test.input), "text/html; charset=utf-8", test.format)
			if err != nil || got != test.want {
				t.Fatalf("fetchedText() = %q, %v; want %q", got, err, test.want)
			}
		})
	}
}

// CheckPermissions judged the raw input, and safeurl prepends https:// only
// when "://" appears nowhere, so "example.com/?next=http://127.0.0.1/" was
// parsed with scheme "example.com/?next=http" and denied as private although
// the fetch itself normalizes it to https://example.com/... . The check must
// see the URL that will be fetched. The mirror case, a private host with a
// public URL in its query, must still be denied.
func TestWebFetchPermissionChecksTheFetchedURL(t *testing.T) {
	wf := NewWebFetchTool()
	cases := map[string]PermissionResult{
		"8.8.8.8/?next=http://127.0.0.1/":       Allow,
		"https://8.8.8.8/?next=http://10.0.0.5": Allow,
		"127.0.0.1/?next=https://8.8.8.8/":      Deny,
		"http://127.0.0.1/":                     Deny,
		"":                                      Deny,
	}
	for u, want := range cases {
		if d := wf.CheckPermissions(Input{"url": u}, Context{}); d.Decision != want {
			t.Errorf("url %q: decision = %v (%s), want %v", u, d.Decision, d.Reason, want)
		}
	}
}
