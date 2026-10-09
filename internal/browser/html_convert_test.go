package browser

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestHTMLToText(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "strips script and style, unescapes entities",
			in:   `<html><head><style>body{color:red}</style><script>alert('x')</script></head><body><h1>Hello &amp; World</h1><p>First &lt;para&gt;</p></body></html>`,
			want: "Hello & World\nFirst <para>",
		},
		{
			name: "block elements become line breaks",
			in:   `<div>one</div><div>two</div><br><p>three</p>`,
			want: "one\n\ntwo\n\nthree",
		},
		{
			// Text mode breaks on the list/table container, not on <li>, so the
			// items of one list run together on a single line while each table
			// cell gets its own paragraph. Markdown mode is the one that renders
			// items individually (see TestHTMLToMarkdown).
			name: "list items are space-joined, table cells are separated",
			in:   `<ul><li>alpha</li><li>beta</li></ul><table><tr><td>c1</td><td>c2</td></tr></table>`,
			want: "alpha beta\n\nc1\n\nc2",
		},
		{
			name: "link text is kept, href is dropped",
			in:   `<p>see <a href="https://example.com/doc">the docs</a> now</p>`,
			want: "see the docs now",
		},
		{
			name: "runs of whitespace collapse and blank lines cap at one",
			in:   "<p>a\t\t  b</p>\n\n\n\n<p>c</p>",
			want: "a b\n\nc",
		},
		{
			name: "CJK text and numeric entities survive intact",
			in:   `<h2>中文标题</h2><p>价格 &gt; 100 元 &#20803;</p>`,
			want: "中文标题\n价格 > 100 元 元",
		},
		{
			name: "quoted greater-than stays inside an attribute",
			in:   `<p title="a > b">visible</p>`,
			want: "visible",
		},
		{
			name: "comments with angle brackets are not content",
			in:   `<p>one</p><!-- hidden > <b>secret</b> --><p>two</p>`,
			want: "one\n\ntwo",
		},
		{
			name: "unclosed script body is not content",
			in:   `<p>visible</p><script>secret body <p>not text</p>`,
			want: "visible",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := HTMLToText(tt.in)
			if got != tt.want {
				t.Fatalf("HTMLToText() =\n%q\nwant\n%q", got, tt.want)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("HTMLToText() produced invalid UTF-8: %q", got)
			}
		})
	}
}

// TestHTMLToText_ScriptBodyNeverLeaks is the specific regression that matters
// for a page whose script body contains prose-looking text: the code must not
// reach the model as if it were page content.
func TestHTMLToText_ScriptBodyNeverLeaks(t *testing.T) {
	in := `<body><SCRIPT type="text/javascript">
var secret = "do not leak me";
</SCRIPT><style media="print">.a{content:"nor me"}</style><p>visible</p></body>`
	got := HTMLToText(in)
	if strings.Contains(got, "do not leak me") || strings.Contains(got, "nor me") || strings.Contains(got, "var secret") {
		t.Fatalf("script/style body leaked into text output: %q", got)
	}
	if got != "visible" {
		t.Fatalf("HTMLToText() = %q, want %q", got, "visible")
	}
}

func TestHTMLToMarkdown(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "headings map to hash levels",
			in:   `<h1>H1</h1><h2>H2</h2><h3>H3</h3><h4>H4</h4><h5>H5</h5><h6>H6</h6>`,
			want: "# H1\n\n## H2\n\n### H3\n\n#### H4\n\n##### H5\n\n###### H6",
		},
		{
			name: "script and style stripped, heading and paragraph kept",
			in:   `<html><head><style>body{color:red}</style><script>alert('x')</script></head><body><h1>Hello &amp; World</h1><p>First &lt;para&gt;</p></body></html>`,
			want: "# Hello & World\n\nFirst <para>",
		},
		{
			name: "lists, links, pre blocks and inline code",
			in:   `<ul><li>one</li><li><a href="https://example.com/a">Two</a></li></ul><pre><code>go build ./...</code></pre><p>inline <code>x := 1</code> done</p>`,
			want: "- one\n- [Two](https://example.com/a)\n\n```\ngo build ./...\n```\n\ninline `x := 1` done",
		},
		{
			name: "single-quoted href is accepted",
			in:   `<p><a class="x" href='https://example.com/q?a=1'>Q</a></p>`,
			want: "[Q](https://example.com/q?a=1)",
		},
		{
			name: "multi-line pre keeps its lines inside the fence",
			in:   "<pre>line one\nline two</pre>",
			want: "```\nline one\nline two\n```",
		},
		{
			name: "CJK heading, list and entities",
			in:   `<h2>中文标题</h2><ul><li>第一项 &amp; 第二项</li></ul><p>价格 &gt; 100 元</p>`,
			want: "## 中文标题\n\n- 第一项 & 第二项\n\n价格 > 100 元",
		},
		{
			name: "quoted greater-than stays inside an attribute",
			in:   `<h2 title="a > b">Heading</h2><p>visible</p>`,
			want: "## Heading\n\nvisible",
		},
		{
			name: "unquoted href and entities are supported",
			in:   `<p><a href=/docs?a=1&amp;b=2>Docs</a></p>`,
			want: "[Docs](/docs?a=1&b=2)",
		},
		{
			name: "pre preserves indentation and repeated spaces",
			in:   "<pre><code>func main() {\n\tfmt.Println(\"x  y\")\n}\n</code></pre>",
			want: "```\nfunc main() {\n\tfmt.Println(\"x  y\")\n}\n```",
		},
		{
			name: "pre preserves leading trailing and internal blank lines",
			in:   "<pre>\n  first\n\n\n  last\n\n</pre>",
			want: "```\n\n  first\n\n\n  last\n\n```",
		},
		{
			name: "pre fence is longer than code backticks",
			in:   "<pre>```\nexample\n```</pre>",
			want: "````\n```\nexample\n```\n````",
		},
		{
			name: "unclosed pre preserves code and decoded entities",
			in:   "<pre><code>\tif x &lt; 2 {\n\t\treturn &amp;value\n",
			want: "```\n\tif x < 2 {\n\t\treturn &value\n```",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := HTMLToMarkdown(tt.in)
			if got != tt.want {
				t.Fatalf("HTMLToMarkdown() =\n%q\nwant\n%q", got, tt.want)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("HTMLToMarkdown() produced invalid UTF-8: %q", got)
			}
		})
	}
}
