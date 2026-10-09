package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/text/encoding/htmlindex"

	"github.com/liuzhixin405/cove-agent/internal/browser"
	"github.com/liuzhixin405/cove-agent/internal/safeurl"
	"github.com/liuzhixin405/cove-agent/internal/textutil"
)

type WebFetchTool struct{ baseTool }

func NewWebFetchTool() Tool {
	return &WebFetchTool{baseTool{def: Def{
		Name: "webfetch", Description: "Fetch content from a URL. Returns the page content. HTTP upgraded to HTTPS.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{
				"url":{"type":"string","description":"The URL to fetch"},
				"format":{"type":"string","description":"Return format: text, markdown, or html"}
			},
			"required":["url"]
		}`),
		IsReadOnly: true, IsConcurrencySafe: true, UserFacingName: "WebFetch",
	}}}
}

func (t *WebFetchTool) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	url, _ := input["url"].(string)
	if url == "" {
		return Result{Data: "Error: url required", IsError: true}, nil
	}
	format, _ := input["format"].(string)
	format = strings.ToLower(strings.TrimSpace(format))
	if format == "" {
		format = "text"
	}
	if format != "text" && format != "markdown" && format != "html" {
		return Result{Data: "Error: format must be one of text, markdown, html", IsError: true}, nil
	}

	url = normalizeFetchURL(url)

	client := newSafeHTTPClient(30 * time.Second)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return Result{Data: "Error: " + err.Error(), IsError: true}, nil
	}
	req.Header.Set("User-Agent", "cove/1.0")

	resp, err := client.Do(req)
	if err != nil {
		return Result{Data: "Error fetching: " + err.Error(), IsError: true}, nil
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 5*1024*1024))
	if err != nil {
		return Result{Data: "Error reading: " + err.Error(), IsError: true}, nil
	}

	content, err := fetchedText(body, resp.Header.Get("Content-Type"), format)
	if err != nil {
		return Result{Data: "Error: " + url + ": " + err.Error(), IsError: true}, nil
	}

	limit := 100000
	content = textutil.ClipBytes(content, limit,
		"\n... [truncated from "+strconv.Itoa(len(body))+" bytes]")

	return Result{Data: "URL: " + url + "\nStatus: " + strconv.Itoa(resp.StatusCode) + "\nFormat: " + format + "\n\n" + strings.TrimSpace(content)}, nil
}

// normalizeFetchURL adds https:// to a URL without a scheme and upgrades
// http:// to https://. The scheme test used to be HasPrefix(url, "http"), so
// "httpbin.org/get" or "http-docs.example.org" got no scheme at all and the
// request failed with unsupported protocol scheme "".
func normalizeFetchURL(u string) string {
	u = strings.TrimSpace(u)
	i := strings.Index(u, "://")
	if i <= 0 || !isURLScheme(u[:i]) {
		return "https://" + u
	}
	if strings.EqualFold(u[:i], "http") {
		return "https://" + u[i+3:]
	}
	return u
}

// isURLScheme reports whether s is an RFC 3986 scheme: a letter followed by
// letters, digits, '+', '-' or '.'. It keeps "example.com/?next=http://" from
// counting as having one.
func isURLScheme(s string) bool {
	for i, c := range s {
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z':
		case i > 0 && ('0' <= c && c <= '9' || c == '+' || c == '-' || c == '.'):
		default:
			return false
		}
	}
	return s != ""
}

// fetchedText turns a response body into text for the model. Binary bodies
// (PDFs, images, archives) are refused: dumped as text they were up to 100KB
// of noise in the context.
func fetchedText(body []byte, contentType, format string) (string, error) {
	ct := strings.ToLower(contentType)
	isHTML := strings.Contains(ct, "text/html") || strings.Contains(ct, "application/xhtml+xml")
	content, decoded := decodeFetchedCharset(body, contentType, isHTML)
	if !decoded {
		sample := body
		if len(sample) > 8192 {
			sample = sample[:8192]
		}
		if sniffText(sample) == textBinary {
			if contentType == "" {
				contentType = "unknown type"
			}
			return "", fmt.Errorf("binary content (%s, %d bytes); webfetch only returns text. Download it with bash (curl -o) if you need the file", contentType, len(body))
		}
	}
	if isHTML {
		switch format {
		case "text":
			content = htmlToText(content)
		case "markdown":
			content = htmlToMarkdown(content)
		}
	}
	return content, nil
}

// reFetchMetaCharset finds a charset declared in an HTML <meta> tag, either
// <meta charset="gbk"> or <meta http-equiv="Content-Type" content="...; charset=gb2312">.
// Copied from internal/browser (decodeBody there is unexported).
var reFetchMetaCharset = regexp.MustCompile(`(?i)<meta[^>]+charset\s*=\s*["']?\s*([a-z0-9_\-:.]+)`)

// decodeFetchedCharset converts body to UTF-8 from the charset the server
// declared (Content-Type, else a <meta> tag in the first 2KB of an HTML page),
// or from GBK when nothing is declared and the bytes are GBK rather than
// UTF-8. The body used to be taken as UTF-8 whatever was declared, so GBK
// pages, still common on Chinese sites, came back as mojibake. decoded is
// false when the body is returned as it was.
func decodeFetchedCharset(body []byte, contentType string, isHTML bool) (text string, decoded bool) {
	cs := ""
	if _, params, err := mime.ParseMediaType(contentType); err == nil {
		cs = strings.ToLower(strings.TrimSpace(params["charset"]))
	}
	if cs == "" && isHTML {
		head := body[:min(len(body), 2048)]
		if m := reFetchMetaCharset.FindSubmatch(head); m != nil {
			cs = strings.ToLower(string(m[1]))
		}
	}
	if cs != "" && cs != "utf-8" && cs != "utf8" {
		if enc, err := htmlindex.Get(cs); err == nil {
			if out, err := enc.NewDecoder().Bytes(body); err == nil {
				return string(out), true
			}
		}
	}
	if cs == "" && !utf8.Valid(body) {
		if lt, ok := decodeGBK(body); ok {
			return lt.text, true
		}
	}
	return string(body), false
}

func htmlToText(s string) string {
	return browser.HTMLToText(s)
}

func htmlToMarkdown(s string) string {
	return browser.HTMLToMarkdown(s)
}

func (t *WebFetchTool) CheckPermissions(input Input, tctx Context) PermissionDecision {
	u, _ := input["url"].(string)
	// Judge the URL Call will fetch. The raw input was checked before, and
	// safeurl prepends https:// only when "://" appears nowhere, so
	// "example.com/?next=http://127.0.0.1/" parsed with the scheme
	// "example.com/?next=http" and was denied as private although the fetch
	// went to https://example.com/.
	if isPrivateURL(normalizeFetchURL(u)) {
		return Denied("access to private/internal URLs is blocked")
	}
	return Allowed("webfetch is read-only")
}

// isPrivateURL delegates to internal/safeurl, which is the single
// implementation of these SSRF predicates. This file used to carry its
// own copy, byte-for-byte duplicated in internal/browser and absent entirely
// from the skills registry fetch — the classic way a security control drifts
// out of sync between call sites.
func isPrivateURL(rawURL string) bool { return safeurl.IsPrivateURL(rawURL) }

// newSafeHTTPClient returns the shared SSRF-hardened client.
func newSafeHTTPClient(timeout time.Duration) *http.Client {
	return safeurl.NewClient(timeout)
}
