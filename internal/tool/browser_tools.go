package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/liuzhixin405/cove-agent/internal/browser"
	"github.com/liuzhixin405/cove-agent/internal/safepath"
)

// BrowserTool drives a (optionally headless-Chrome) browser to fetch
// JavaScript-rendered content or capture screenshots. When the binary is built
// without the "chromedp" tag, the navigate/read actions transparently fall back
// to HTTP fetch and screenshot reports that headless mode is unavailable.
type BrowserTool struct {
	baseTool
	br *browser.Browser
}

func NewBrowserTool() Tool {
	return &BrowserTool{
		baseTool: baseTool{def: Def{
			Name:        "browser",
			Description: "Drive a headless browser. action=navigate renders a page (executing JavaScript) and returns text/markdown/html; action=screenshot saves a PNG and returns native image content within the preview limits. Falls back to HTTP fetch when headless Chrome is unavailable.",
			InputSchema: json.RawMessage(`{
				"type":"object",
				"properties":{
					"action":{"type":"string","enum":["navigate","screenshot"],"description":"navigate to read rendered content, or screenshot to capture a PNG"},
					"url":{"type":"string","description":"The URL to open"},
					"format":{"type":"string","enum":["text","markdown","html"],"description":"navigate output format (default text)"},
					"output":{"type":"string","description":"screenshot file path (default browser-screenshot.png)"}
				},
				"required":["action","url"]
			}`),
			// Not read-only: action=screenshot writes a PNG into the
			// workspace. Declared read-only, the tool went to read-only
			// sub-agents (which run read-only tools with no permission gate)
			// and through plan mode, either of which could then overwrite a
			// project file. Def has no per-action read-only report, so the
			// whole tool is gated like write; navigate stays Allowed in
			// CheckPermissions.
			IsReadOnly: false, IsConcurrencySafe: false, UserFacingName: "Browser",
		}},
		br: browser.New(browser.DefaultConfig()),
	}
}

func (t *BrowserTool) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	action, _ := input["action"].(string)
	action = strings.ToLower(strings.TrimSpace(action))
	rawURL, _ := input["url"].(string)
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return Result{Data: "Error: url required", IsError: true}, nil
	}

	switch action {
	case "", "navigate", "open", "read":
		format, _ := input["format"].(string)
		format = strings.ToLower(strings.TrimSpace(format))
		if format == "" {
			format = "text"
		}
		if format != "text" && format != "markdown" && format != "html" {
			return Result{Data: "Error: format must be one of text, markdown, html", IsError: true}, nil
		}
		var (
			res *browser.FetchResult
			err error
		)
		if t.br.ChromeAvailable() {
			res, err = t.br.FetchRendered(ctx, rawURL, format)
		} else {
			// One request in the requested format. This used to fetch as
			// markdown first and then fetch again for text/html, doubling the
			// latency and any side effect of the GET.
			switch format {
			case "markdown":
				res, err = t.br.FetchMarkdown(ctx, rawURL)
			case "html":
				res, err = t.br.FetchHTML(ctx, rawURL)
			default:
				res, err = t.br.FetchHeadless(ctx, rawURL)
			}
		}
		if err != nil {
			return Result{Data: "Error: " + err.Error(), IsError: true}, nil
		}
		mode := "headless-chrome"
		if !t.br.ChromeAvailable() {
			mode = "http"
		}
		status := "unknown"
		if res.StatusCode > 0 {
			status = strconv.Itoa(res.StatusCode)
		}
		header := fmt.Sprintf("URL: %s\nStatus: %s\nFormat: %s\nMode: %s\n\n", res.URL, status, res.Format, mode)
		return Result{Data: header + res.Content}, nil

	case "screenshot", "capture":
		out, err := screenshotPath(input, tctx.Cwd)
		if err != nil {
			return Result{Data: "Error: " + err.Error(), IsError: true}, nil
		}
		if !t.br.ChromeAvailable() {
			return Result{Data: "Error: " + browser.ErrChromeUnavailable.Error(), IsError: true}, nil
		}
		png, err := t.br.Screenshot(ctx, rawURL)
		if err != nil {
			return Result{Data: "Error: " + err.Error(), IsError: true}, nil
		}
		if err := saveScreenshot(out, png); err != nil {
			return Result{Data: "Error writing screenshot: " + err.Error(), IsError: true}, nil
		}
		return savedScreenshotResult(out, png), nil

	default:
		return Result{Data: "Error: unknown action " + action + " (use navigate or screenshot)", IsError: true}, nil
	}
}

func savedScreenshotResult(out string, png []byte) Result {
	summary := fmt.Sprintf("Saved screenshot (%d bytes) to %s", len(png), out)
	result := readImageResult(out, bytes.NewReader(png))
	if result.IsError {
		return Result{Data: summary + "\nImage preview unavailable: " + strings.TrimPrefix(result.Data, "Error: ")}
	}
	result.Data = summary
	return result
}

func (t *BrowserTool) Validate(input Input) string {
	if _, ok := input["url"].(string); !ok {
		return "url is required"
	}
	return ""
}

func (t *BrowserTool) CheckPermissions(input Input, tctx Context) PermissionDecision {
	u, _ := input["url"].(string)
	if isPrivateURL(u) {
		return Denied("access to private/internal URLs is blocked")
	}
	if isScreenshotAction(input) {
		out, err := screenshotPath(input, tctx.Cwd)
		if err != nil {
			return Denied(err.Error())
		}
		// Writes a file to disk; surface for confirmation under default mode.
		return Asked("browser screenshot writes a PNG file: " + out)
	}
	return Allowed("browser navigation is read-only")
}

// isScreenshotAction reports whether the call writes a screenshot. It must
// accept every alias Call does: CheckPermissions used to match only the literal
// "screenshot", so action=capture wrote its file without asking.
func isScreenshotAction(input Input) bool {
	action, _ := input["action"].(string)
	switch strings.ToLower(strings.TrimSpace(action)) {
	case "screenshot", "capture":
		return true
	}
	return false
}

// saveScreenshot writes the PNG the way write does: a complete new file
// renamed into place (replaceFile). It used os.WriteFile, which truncates
// first, so a crash or a full disk left an empty file where an image was.
func saveScreenshot(out string, png []byte) error {
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	return replaceFile(out, png)
}

// screenshotPath resolves the screenshot's output path and refuses anything
// but a .png file inside the workspace, and an existing file that is not a
// PNG. The tool was marked read-only, so a read-only sub-agent ran it with no
// permission gate at all, and the path is picked by the model: without this
// check an absolute path or "../" let such a call overwrite any file the user
// can write. It is no longer read-only, but the checks stay as the second
// line of defense.
func screenshotPath(input Input, cwd string) (string, error) {
	out, _ := input["output"].(string)
	out = strings.TrimSpace(out)
	if out == "" {
		out = "browser-screenshot.png"
	}
	if !strings.EqualFold(filepath.Ext(out), ".png") {
		return "", fmt.Errorf("screenshot output must be a .png file, got %q", out)
	}
	if cwd == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("screenshot output: cannot determine the workspace: %w", err)
		}
		cwd = wd
	}
	abs := out
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(cwd, abs)
	}
	abs = filepath.Clean(abs)
	// "main.go:x.png" ends in .png but is a stream of main.go on NTFS.
	if hasStreamSeparator(abs, runtime.GOOS) {
		return "", fmt.Errorf("screenshot output must not contain ':' (on Windows it names an alternate data stream of another file), got %q", out)
	}
	rel, err := filepath.Rel(filepath.Clean(cwd), abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("screenshot output must be inside the workspace %s, got %q", cwd, out)
	}
	// The text of the path is not enough: os.WriteFile follows links, and a
	// junction in the workspace (mklink /J needs no privilege) pointing
	// elsewhere took "out/x.png" outside it. Resolve links as write does.
	if !safepath.Within(cwd, abs) {
		return "", fmt.Errorf("screenshot output must be inside the workspace %s, got %q (it leads outside through a link)", cwd, out)
	}
	// A link as the file itself would have its target overwritten, or, when
	// dangling, created wherever it points.
	if fi, err := os.Lstat(abs); err == nil && !fi.Mode().IsRegular() {
		return "", fmt.Errorf("screenshot output %q exists and is not a regular file (a link?); choose another name", out)
	}
	// An existing regular file used to be replaced whatever it held: a
	// project asset named .png was gone. As draw_image does, replace only a
	// file that already is a PNG.
	if err := checkReplaceableImage(abs); err != nil {
		return "", fmt.Errorf("screenshot output: %w", err)
	}
	return abs, nil
}

// BrowserChromeAvailable reports whether headless Chrome is compiled in
// (-tags chromedp). Without it the browser tool only falls back to an HTTP
// fetch, which webfetch already does, so the registry leaves it out.
func BrowserChromeAvailable() bool {
	return browser.New(browser.DefaultConfig()).ChromeAvailable()
}
