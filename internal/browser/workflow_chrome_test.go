//go:build chromedp

package browser

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func requireWorkflowChrome(t *testing.T) string {
	t.Helper()
	path := workflowChromePath("")
	if path == "" {
		t.Skip("Chrome/Chromium not installed; actual DOM workflow unverified")
	}
	return path
}

func TestWorkflowChromeActionsScreenshots(t *testing.T) {
	chrome := requireWorkflowChrome(t)
	fixture := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		if request.URL.Path != "/form" {
			fmt.Fprint(writer, `<html><body><a id="open" href="/form">Open form</a></body></html>`)
			return
		}
		fmt.Fprint(writer, `<html><head><meta name="viewport" content="width=device-width,initial-scale=1"><style>body{margin:0;background:#fff;font:20px Georgia;color:#222}header{padding:24px;background:#18745c;color:white}main{padding:24px;max-width:680px}input,button{font:inherit;padding:12px;margin:8px 0;max-width:90%}button{background:#bd334d;color:white;border:0}#result{padding:20px;background:#eef5fa;border-left:4px solid #257499}</style></head><body><header>Browser workflow fixture</header><main><h1>Order lookup</h1><label for="name">Reference</label><br><input id="name"><br><button id="submit" onclick="document.querySelector('#result').textContent='Confirmed '+document.querySelector('#name').value">Confirm</button><p id="result">Pending</p></main></body></html>`)
	}))
	defer fixture.Close()
	workflow := Workflow{URL: fixture.URL, TimeoutMS: 60000, StepTimeoutMS: 3000, Fixtures: map[string]string{"reference": "fixture-42"}, Steps: []WorkflowStep{
		{Action: "navigate", URL: fixture.URL + "/start"},
		{Action: "click", Selector: "#open"},
		{Action: "assert_url", URL: fixture.URL + "/form"},
		{Action: "fill", Selector: "#name", Fixture: "reference"},
		{Action: "click", Selector: "#submit"},
		{Action: "assert_text", Selector: "#result", Text: "Confirmed fixture-42"},
		{Action: "assert_visible", Selector: "#result"},
	}}
	browser := New(Config{AllowLocalhost: true, Timeout: 60 * time.Second})
	options := RunOptions{ChromePath: chrome, EvidenceDir: t.TempDir()}
	if directory := os.Getenv("COVE_BROWSER_TEST_EVIDENCE_DIR"); directory != "" {
		options.EvidenceDir = directory
	}
	report, err := browser.Run(context.Background(), workflow, options)
	if err != nil || report.Status != StatusPass || report.Assertions != 6 || len(report.Steps) != 14 || len(report.Artifacts) != 2 {
		t.Fatalf("actual browser workflow: %+v %v", report, err)
	}
	for _, artifact := range report.Artifacts {
		data, err := os.ReadFile(filepath.Join(report.EvidenceDir, artifact.Name))
		if err != nil {
			t.Fatal(err)
		}
		image, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		width, height := 1280, 800
		if artifact.Viewport == "mobile" {
			width, height = 390, 844
		}
		if image.Bounds().Dx() != width || image.Bounds().Dy() != height {
			t.Fatalf("viewport dimensions: %v", image.Bounds())
		}
		colors := map[uint64]bool{}
		for vertical := 0; vertical < height; vertical += 3 {
			for horizontal := 0; horizontal < width; horizontal += 3 {
				red, green, blue, alpha := image.At(horizontal, vertical).RGBA()
				colors[uint64(red)<<48|uint64(green)<<32|uint64(blue)<<16|uint64(alpha)] = true
			}
		}
		if len(colors) < 20 {
			t.Fatalf("blank screenshot: %d colors", len(colors))
		}
		hash := sha256.Sum256(data)
		if artifact.SHA256 != hex.EncodeToString(hash[:]) || artifact.Bytes != len(data) {
			t.Fatal("artifact hash/size mismatch")
		}
		t.Logf("pixel proof: %s %dx%d %d sampled colors %d bytes sha256=%s", artifact.Name, width, height, len(colors), len(data), artifact.SHA256)
	}
	data, err := os.ReadFile(filepath.Join(report.EvidenceDir, "report.json"))
	if err != nil || strings.Contains(string(data), "fixture-42") || strings.Contains(string(data), fixture.URL) {
		t.Fatalf("report leaks fixtures/URLs: %s %v", data, err)
	}
	t.Logf("persisted evidence: %s", report.EvidenceDir)
	workflow.Steps[5].Text = "Wrong expected value"
	workflow.StepTimeoutMS = 300
	report, err = browser.Run(context.Background(), workflow, RunOptions{ChromePath: chrome, EvidenceDir: t.TempDir()})
	if !errors.Is(err, ErrAssertionFailed) || report.Status != StatusFail || report.Reason != "assertion_failed" {
		t.Fatalf("wrong DOM assertion must fail: %+v %v", report, err)
	}
	t.Logf("negative proof: wrong expected DOM text -> status=%s reason=%s error=%v assertions=%d screenshots=%d", report.Status, report.Reason, err, report.Assertions, len(report.Artifacts))
}

func TestWorkflowChromeMissingAndCancel(t *testing.T) {
	browser := New(DefaultConfig())
	report, err := browser.Run(context.Background(), testWorkflow(), RunOptions{EvidenceDir: t.TempDir(), ChromePath: filepath.Join(t.TempDir(), "missing-chrome")})
	if !errors.Is(err, ErrChromeUnavailable) || report.Status != StatusUnverified {
		t.Fatalf("missing Chrome: %+v %v", report, err)
	}
	chrome := requireWorkflowChrome(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fixture := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		cancel()
		fmt.Fprint(writer, `<html><body>Cancelled</body></html>`)
	}))
	defer fixture.Close()
	workflow := testWorkflow()
	workflow.URL = fixture.URL
	report, err = New(Config{AllowLocalhost: true}).Run(ctx, workflow, RunOptions{EvidenceDir: t.TempDir(), ChromePath: chrome})
	if !errors.Is(err, context.Canceled) || report.Status != StatusUnverified {
		t.Fatalf("active cancellation: %+v %v", report, err)
	}
}

func TestWorkflowChromeSensitiveAndSubresource(t *testing.T) {
	chrome := requireWorkflowChrome(t)
	for _, scenario := range []struct {
		name, html string
		fill       bool
	}{
		{"password_capture", `<html><body><input type="password" value="do-not-leak"></body></html>`, false},
		{"password_fill", `<html><body><input id="secret" type="password"></body></html>`, true},
		{"closed_shadow_password", `<html><body><div id="host"></div><script>document.querySelector('#host').attachShadow({mode:'closed'}).innerHTML='<input type="password" value="do-not-leak">'</script></body></html>`, false},
		{"iframe_capture", `<html><body><iframe srcdoc='<input type="password" value="do-not-leak">'></iframe></body></html>`, false},
		{"metadata_subresource", `<html><body><img src="http://169.254.169.254/latest/meta-data/"><h1>Fixture</h1></body></html>`, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			fixture := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Type", "text/html")
				fmt.Fprint(writer, scenario.html)
			}))
			defer fixture.Close()
			workflow := testWorkflow()
			workflow.URL = fixture.URL
			if scenario.fill {
				workflow.Fixtures = map[string]string{"secret": "do-not-leak"}
				workflow.Steps = append([]WorkflowStep{{Action: "fill", Selector: "#secret", Fixture: "secret"}}, workflow.Steps...)
			}
			report, err := New(Config{AllowLocalhost: true}).Run(context.Background(), workflow, RunOptions{EvidenceDir: t.TempDir(), ChromePath: chrome})
			if err == nil || report.Status != StatusFail || report.Reason != "safety_policy" || len(report.Artifacts) != 0 {
				t.Fatalf("unsafe workflow: %+v %v", report, err)
			}
			data, err := os.ReadFile(filepath.Join(report.EvidenceDir, "report.json"))
			if err != nil || strings.Contains(string(data), "do-not-leak") {
				t.Fatal("secret leaked in report")
			}
		})
	}
}

func TestWorkflowChromeTimeoutAndProxyGuard(t *testing.T) {
	chrome := requireWorkflowChrome(t)
	fixture := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/redirect" {
			http.Redirect(writer, request, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
			return
		}
		fmt.Fprint(writer, `<html><body>Fixture</body></html>`)
	}))
	defer fixture.Close()
	browser := New(Config{AllowLocalhost: true})
	workflow := testWorkflow()
	workflow.URL = fixture.URL
	workflow.TimeoutMS = 1
	report, err := browser.Run(context.Background(), workflow, RunOptions{EvidenceDir: t.TempDir(), ChromePath: chrome})
	if !errors.Is(err, context.DeadlineExceeded) || report.Status != StatusUnverified {
		t.Fatalf("global timeout: %+v %v", report, err)
	}
	workflow.TimeoutMS = 0
	workflow.URL += "/redirect"
	report, err = browser.Run(context.Background(), workflow, RunOptions{EvidenceDir: t.TempDir(), ChromePath: chrome})
	if err == nil || report.Status != StatusFail || report.Reason != "safety_policy" {
		t.Fatalf("redirect guard: %+v %v", report, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	proxy, err := browser.startWorkflowProxy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.close()
	connection, err := browser.workflowDial(ctx, "tcp", "169.254.169.254:80")
	if connection != nil {
		_ = connection.Close()
	}
	if err == nil {
		t.Fatal("proxy dial reached metadata")
	}
}

func TestWorkflowChromeConnectionFailureCannotPass(t *testing.T) {
	chrome := requireWorkflowChrome(t)
	fixture := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) { fmt.Fprint(writer, "unused") }))
	address := fixture.URL
	fixture.Close()
	workflow := testWorkflow()
	workflow.URL = address
	report, err := New(Config{AllowLocalhost: true}).Run(context.Background(), workflow, RunOptions{EvidenceDir: t.TempDir(), ChromePath: chrome})
	if err == nil || report.Status != StatusUnverified || report.Reason != "network_unavailable" || report.Assertions != 0 || len(report.Artifacts) != 0 {
		t.Fatalf("network error page cannot be a pass: %+v %v", report, err)
	}
}
