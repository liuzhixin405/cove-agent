// Package browser provides HTTP-based web content retrieval with SSRF protection.
//
// It performs HTTP GET requests with browser-like headers and converts HTML to
// readable text or Markdown. This is a pragmatic alternative to headless Chrome
// for the >90% of web pages that don't require JavaScript rendering.
//
// Full chrome-based rendering can be layered on later via chromedp behind
// a build tag (//go:build chromedp).
package browser

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/liuzhixin405/cove-agent/internal/safeurl"
	"github.com/liuzhixin405/cove-agent/internal/textutil"
	"golang.org/x/net/html"
)

// ErrChromeUnavailable is returned by headless rendering when the binary was
// not built with the "chromedp" tag.
var ErrChromeUnavailable = errors.New("headless browser not available: rebuild with -tags chromedp (requires Chrome installed)")

// Browser fetches web content via HTTP with safety checks.
type Browser struct {
	timeout        time.Duration
	allowLocalhost bool
	maxBodySize    int64
}

// Config holds browser configuration.
type Config struct {
	AllowLocalhost bool
	Timeout        time.Duration
	MaxBodySize    int64 // 0 = default (5MB)
}

// DefaultConfig returns safe defaults.
func DefaultConfig() Config {
	return Config{
		AllowLocalhost: false,
		Timeout:        30 * time.Second,
		MaxBodySize:    5 * 1024 * 1024,
	}
}

// New creates a Browser with the given configuration.
func New(cfg Config) *Browser {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.MaxBodySize <= 0 {
		cfg.MaxBodySize = 5 * 1024 * 1024
	}
	return &Browser{
		timeout:        cfg.Timeout,
		allowLocalhost: cfg.AllowLocalhost,
		maxBodySize:    cfg.MaxBodySize,
	}
}

// FetchResult is the result of a page fetch.
type FetchResult struct {
	URL        string
	StatusCode int
	Content    string
	Format     string // "text", "markdown", or "html"
}

// FetchHeadless navigates to a URL and returns rendered content as text.
// For static/server-rendered pages this works identically to a browser.
// For SPAs that require JavaScript, use FetchMarkdown with mode=headless.
func (b *Browser) FetchHeadless(ctx context.Context, rawURL string) (*FetchResult, error) {
	return b.fetch(ctx, rawURL, "text")
}

// FetchMarkdown fetches a URL and converts HTML to Markdown.
func (b *Browser) FetchMarkdown(ctx context.Context, rawURL string) (*FetchResult, error) {
	return b.fetch(ctx, rawURL, "markdown")
}

// FetchHTML fetches a URL and returns raw HTML.
func (b *Browser) FetchHTML(ctx context.Context, rawURL string) (*FetchResult, error) {
	return b.fetch(ctx, rawURL, "html")
}

// ChromeAvailable reports whether headless Chrome rendering was compiled in
// (build with -tags chromedp).
func (b *Browser) ChromeAvailable() bool { return chromeAvailable() }

// FetchRendered renders a URL with headless Chrome (executing JavaScript) and
// returns the result in the requested format ("text", "markdown" or "html").
// Falls back with an error if the binary was not built with -tags chromedp.
func (b *Browser) FetchRendered(ctx context.Context, rawURL, format string) (*FetchResult, error) {
	if !strings.Contains(rawURL, "://") {
		rawURL = "https://" + rawURL
	}
	if err := b.validateURL(rawURL); err != nil {
		return nil, err
	}
	htmlContent, err := renderHeadless(ctx, rawURL, b.timeout, !b.allowLocalhost)
	if err != nil {
		return nil, err
	}
	content := htmlContent
	switch format {
	case "text":
		content = HTMLToText(htmlContent)
	case "markdown":
		content = HTMLToMarkdown(htmlContent)
	case "html":
		// keep as-is
	default:
		content = HTMLToText(htmlContent)
		format = "text"
	}
	const outputLimit = 100000
	if len(content) > outputLimit {
		// outputLimit is a BYTE budget, so content[:outputLimit] used to cut
		// straight through a multi-byte character (100000 is not a multiple of
		// 3, so any CJK page hit this): the result was invalid UTF-8 that shows
		// up as U+FFFD in the TUI and can be rejected by a provider's JSON
		// encoder. ClipBytes ends the prefix on a rune boundary.
		content = textutil.ClipBytes(content, outputLimit, "\n... [truncated from "+strconv.Itoa(len(htmlContent))+" bytes]")
	}
	return &FetchResult{
		URL: rawURL,
		// The headless render does not observe the document response, so
		// the status is unknown (0): reporting 200 made an error page look
		// like a successful fetch to the model.
		StatusCode: 0,
		Content:    strings.TrimSpace(content),
		Format:     format,
	}, nil
}

// Screenshot renders a URL with headless Chrome and returns a full-page PNG.
// Requires a build with -tags chromedp.
func (b *Browser) Screenshot(ctx context.Context, rawURL string) ([]byte, error) {
	if !strings.Contains(rawURL, "://") {
		rawURL = "https://" + rawURL
	}
	if err := b.validateURL(rawURL); err != nil {
		return nil, err
	}
	return captureScreenshot(ctx, rawURL, b.timeout, !b.allowLocalhost)
}

func (b *Browser) fetch(ctx context.Context, rawURL string, format string) (*FetchResult, error) {
	// Add default scheme if missing
	if !strings.Contains(rawURL, "://") {
		rawURL = "https://" + rawURL
	}

	// Validate URL safety
	if err := b.validateURL(rawURL); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()

	client := b.newHTTPClient()
	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("request creation failed: %w", err)
	}
	req.Header.Set("User-Agent", "cove/1.0 (Mozilla/5.0 compatible)")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, b.maxBodySize))
	if err != nil {
		return nil, fmt.Errorf("read failed: %w", err)
	}

	// maxBodySize is a byte budget applied by LimitReader, so a body larger than
	// the cap is cut at an arbitrary byte — regularly inside a multi-byte
	// character. Drop that half rune before the bytes become a string.
	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	isHTML := strings.Contains(contentType, "text/html") || strings.Contains(contentType, "application/xhtml+xml")
	content, err := decodeBody(body, contentType, isHTML)
	if err != nil {
		return nil, err
	}
	content = trimPartialTrailingRune(content)

	if isHTML {
		switch format {
		case "text":
			content = HTMLToText(content)
		case "markdown":
			content = HTMLToMarkdown(content)
		case "html":
			// Keep as-is
		}
	}

	// Truncate if too large. ClipBytes, not content[:outputLimit]: see the note
	// in FetchRendered — a byte-indexed cut mangles the last character.
	const outputLimit = 100000
	if len(content) > outputLimit {
		content = textutil.ClipBytes(content, outputLimit, "\n... [truncated from "+strconv.Itoa(len(body))+" bytes]")
	}

	return &FetchResult{
		URL:        rawURL,
		StatusCode: resp.StatusCode,
		Content:    strings.TrimSpace(content),
		Format:     format,
	}, nil
}

// newHTTPClient builds the fetch client. Unless localhost access is explicitly
// allowed, it is the shared SSRF-hardened client from internal/safeurl.
func (b *Browser) newHTTPClient() *http.Client {
	if b.allowLocalhost {
		return &http.Client{Timeout: b.timeout}
	}
	return safeurl.NewClient(b.timeout)
}

// validateURL checks that the URL is safe to fetch.
func (b *Browser) validateURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}

	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("unsupported scheme: %s (only http/https allowed)", u.Scheme)
	}

	if b.allowLocalhost {
		return nil
	}

	if isPrivateHost(u.Hostname()) {
		return fmt.Errorf("access to private/internal hosts is blocked: %s", u.Hostname())
	}

	return nil
}

// isPrivateHost delegates to internal/safeurl, the single implementation of
// these SSRF predicates. This file carried a byte-for-byte duplicate of
// internal/tool's copy, and the two had already diverged (this one was missing
// the 100.64/10 carrier-NAT and fe80::/10 ranges).
func isPrivateHost(host string) bool { return safeurl.IsPrivateHost(host) }

// trimPartialTrailingRune drops an incomplete UTF-8 sequence from the end of s,
// which is what a byte-count cap (maxBodySize) leaves behind when it lands in
// the middle of a multi-byte character.
//
// Only the tail is inspected: bytes elsewhere in the body are left alone, so a
// response that genuinely is not UTF-8 is not silently rewritten.
func trimPartialTrailingRune(s string) string {
	// Only the last utf8.UTFMax-1 bytes can hold a truncated sequence.
	for i := len(s) - 1; i >= 0 && i > len(s)-utf8.UTFMax; i-- {
		if !utf8.RuneStart(s[i]) {
			continue // a continuation byte: keep walking back to its lead byte
		}
		if s[i] < utf8.RuneSelf {
			return s // ASCII last character: nothing was cut
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		// A lead byte in [0xC2,0xF4] that fails to decode is a real multi-byte
		// character whose continuation bytes the cap chopped off. Any other
		// undecodable byte was never valid UTF-8 to begin with, so it is left
		// in place rather than quietly eaten.
		if r == utf8.RuneError && size <= 1 && s[i] >= 0xC2 && s[i] <= 0xF4 {
			return s[:i]
		}
		return s
	}
	return s
}

// HTMLToText strips HTML tags and returns plain text.
func HTMLToText(s string) string {
	return convertHTML(s, false)
}

// HTMLToMarkdown converts HTML to a readable Markdown representation.
func HTMLToMarkdown(s string) string {
	return convertHTML(s, true)
}

type htmlTextWriter struct {
	strings.Builder
	space bool
	lines int
}

func (writer *htmlTextWriter) flushSpace() {
	if writer.Len() > 0 {
		if writer.lines > 0 {
			writer.WriteString(strings.Repeat("\n", min(writer.lines, 2)))
		} else if writer.space {
			writer.WriteByte(' ')
		}
	}
	writer.space, writer.lines = false, 0
}

func (writer *htmlTextWriter) text(text string) {
	for _, value := range text {
		switch {
		case value == '\n' || value == '\r':
			writer.lines = min(writer.lines+1, 2)
		case unicode.IsSpace(value):
			writer.space = true
		default:
			writer.flushSpace()
			writer.WriteRune(value)
		}
	}
}

func (writer *htmlTextWriter) codeBlock(code string) {
	longest, run := 0, 0
	for _, value := range code {
		if value == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", max(3, longest+1))
	writer.lines = max(writer.lines, 2)
	writer.flushSpace()
	writer.WriteString(fence + "\n")
	writer.WriteString(code)
	if !strings.HasSuffix(code, "\n") {
		writer.WriteByte('\n')
	}
	writer.WriteString(fence)
	writer.lines = 2
}

func convertHTML(source string, markdown bool) string {
	tokenizer := html.NewTokenizer(strings.NewReader(source))
	var writer htmlTextWriter
	var code strings.Builder
	var anchors []string
	skipped := ""
	inPre := false
	for {
		kind := tokenizer.Next()
		if kind == html.ErrorToken {
			if inPre {
				writer.codeBlock(code.String())
			}
			return strings.TrimSpace(writer.String())
		}
		if kind == html.TextToken {
			if skipped == "" {
				text := string(tokenizer.Text())
				if inPre {
					code.WriteString(text)
				} else {
					writer.text(text)
				}
			}
			continue
		}
		if kind != html.StartTagToken && kind != html.EndTagToken && kind != html.SelfClosingTagToken {
			continue
		}
		token := tokenizer.Token()
		ending := kind == html.EndTagToken
		if skipped != "" {
			if ending && token.Data == skipped {
				skipped = ""
			}
			continue
		}
		if token.Data == "script" || token.Data == "style" {
			if !ending {
				skipped = token.Data
				writer.space = true
			}
			continue
		}
		if inPre {
			if ending && token.Data == "pre" {
				writer.codeBlock(code.String())
				code.Reset()
				inPre = false
			}
			continue
		}
		if markdown {
			switch token.Data {
			case "pre":
				if !ending {
					inPre = true
				}
				continue
			case "h1", "h2", "h3", "h4", "h5", "h6":
				writer.lines = min(writer.lines+1, 2)
				if !ending {
					writer.text(strings.Repeat("#", int(token.Data[1]-'0')) + " ")
				}
				continue
			case "li":
				if !ending {
					writer.lines = min(writer.lines+1, 2)
					writer.text("- ")
				}
				continue
			case "code":
				writer.text("`")
				continue
			case "a":
				if !ending {
					href := ""
					for _, attribute := range token.Attr {
						if attribute.Key == "href" {
							href = attribute.Val
							break
						}
					}
					anchors = append(anchors, href)
					if href != "" {
						writer.text("[")
					}
				} else if len(anchors) > 0 {
					href := anchors[len(anchors)-1]
					anchors = anchors[:len(anchors)-1]
					if href != "" {
						writer.text("](" + href + ")")
					}
				}
				continue
			}
		}
		switch token.Data {
		case "p", "div", "section", "article", "br", "hr", "ul", "ol", "table", "tr", "td", "th", "blockquote":
			writer.lines = min(writer.lines+1, 2)
		default:
			writer.space = true
		}
	}
}
