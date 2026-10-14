// Package sigv4 implements AWS Signature Version 4 for cove's Bedrock
// provider with the standard library only. Compute exposes the
// intermediate strings so the AWS test-suite vectors can pin each stage;
// SignRequest applies the Bedrock header set to an *http.Request.
package sigv4

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/api/awscreds"
)

const (
	algorithm  = "AWS4-HMAC-SHA256"
	dateFormat = "20060102T150405Z"
)

type Params struct {
	Method, Host, Path, RawQuery string
	// Headers are the headers to sign (host comes from Host). Names are
	// matched case-insensitively; values are canonicalized.
	Headers         http.Header
	Payload         []byte
	Region, Service string
	Now             time.Time
	Creds           awscreds.Credentials
}

type Result struct {
	CanonicalRequest, StringToSign, Authorization string
	SignedHeaders, AmzDate, PayloadHash           string
}

// Compute derives the SigV4 Authorization value for p, plus the canonical
// request and string to sign it was built from.
func Compute(p Params) Result {
	now := p.Now.UTC()
	amzDate := now.Format(dateFormat)
	date := amzDate[:8]
	payloadHash := hexSHA256(p.Payload)

	names := []string{"host"}
	values := map[string]string{"host": strings.TrimSpace(p.Host)}
	for name, vs := range p.Headers {
		lower := strings.ToLower(name)
		if lower == "host" || len(vs) == 0 {
			continue
		}
		canon := make([]string, 0, len(vs))
		for _, v := range vs {
			canon = append(canon, collapseSpaces(v))
		}
		values[lower] = strings.Join(canon, ",")
		names = append(names, lower)
	}
	sort.Strings(names)
	var canonHeaders strings.Builder
	for _, n := range names {
		canonHeaders.WriteString(n + ":" + values[n] + "\n")
	}
	signedHeaders := strings.Join(names, ";")

	path := p.Path
	if path == "" {
		path = "/"
	}
	canonicalRequest := strings.Join([]string{
		p.Method, canonicalPath(path), canonicalQuery(p.RawQuery),
		canonHeaders.String(), signedHeaders, payloadHash,
	}, "\n")

	scope := date + "/" + p.Region + "/" + p.Service + "/aws4_request"
	stringToSign := strings.Join([]string{algorithm, amzDate, scope, hexSHA256([]byte(canonicalRequest))}, "\n")

	key := hmacSHA256([]byte("AWS4"+p.Creds.SecretAccessKey), []byte(date))
	key = hmacSHA256(key, []byte(p.Region))
	key = hmacSHA256(key, []byte(p.Service))
	key = hmacSHA256(key, []byte("aws4_request"))
	signature := hex.EncodeToString(hmacSHA256(key, []byte(stringToSign)))

	return Result{
		CanonicalRequest: canonicalRequest,
		StringToSign:     stringToSign,
		Authorization:    algorithm + " Credential=" + p.Creds.AccessKeyID + "/" + scope + ", SignedHeaders=" + signedHeaders + ", Signature=" + signature,
		SignedHeaders:    signedHeaders,
		AmzDate:          amzDate,
		PayloadHash:      payloadHash,
	}
}

// SignRequest signs req for Bedrock: it sets x-amz-date, x-amz-content-sha256
// and, with a session token, x-amz-security-token, then signs content-type,
// host and those headers. Other headers (anthropic-version) are sent
// unsigned. Any earlier Authorization is replaced.
func SignRequest(req *http.Request, payload []byte, creds awscreds.Credentials, region, service string, now time.Time) {
	now = now.UTC()
	req.Header.Set("x-amz-date", now.Format(dateFormat))
	req.Header.Set("x-amz-content-sha256", hexSHA256(payload))
	if creds.SessionToken != "" {
		req.Header.Set("x-amz-security-token", creds.SessionToken)
	} else {
		req.Header.Del("x-amz-security-token")
	}
	toSign := http.Header{}
	for _, name := range []string{"Content-Type", "x-amz-content-sha256", "x-amz-date", "x-amz-security-token"} {
		if v := req.Header.Get(name); v != "" {
			toSign.Set(name, v)
		}
	}
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	r := Compute(Params{
		Method: req.Method, Host: host, Path: req.URL.EscapedPath(), RawQuery: req.URL.RawQuery,
		Headers: toSign, Payload: payload, Region: region, Service: service, Now: now, Creds: creds,
	})
	req.Header.Set("Authorization", r.Authorization)
}

// canonicalPath URI-encodes each path segment once (RFC 3986; "/" kept).
func canonicalPath(path string) string {
	segments := strings.Split(path, "/")
	for i, s := range segments {
		if s == "" {
			continue
		}
		if unescaped, err := url.PathUnescape(s); err == nil {
			s = unescaped
		}
		segments[i] = uriEncode(s)
	}
	return strings.Join(segments, "/")
}

// canonicalQuery sorts parameters by key, then value, each RFC 3986 encoded.
func canonicalQuery(raw string) string {
	if raw == "" {
		return ""
	}
	type kv struct{ k, v string }
	var pairs []kv
	for _, part := range strings.Split(raw, "&") {
		if part == "" {
			continue
		}
		k, v, _ := strings.Cut(part, "=")
		if uk, err := url.QueryUnescape(k); err == nil {
			k = uk
		}
		if uv, err := url.QueryUnescape(v); err == nil {
			v = uv
		}
		pairs = append(pairs, kv{uriEncode(k), uriEncode(v)})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].k != pairs[j].k {
			return pairs[i].k < pairs[j].k
		}
		return pairs[i].v < pairs[j].v
	})
	out := make([]string, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, p.k+"="+p.v)
	}
	return strings.Join(out, "&")
}

// uriEncode is the SigV4 flavour of percent-encoding: unreserved
// characters (A-Z a-z 0-9 - _ . ~) pass through, everything else is %XX
// upper-case, including space (%20) and "/".
func uriEncode(s string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&15])
		}
	}
	return b.String()
}

// collapseSpaces trims v and folds runs of spaces into one, as the
// canonical header value rules require.
func collapseSpaces(v string) string {
	return strings.Join(strings.Fields(v), " ")
}

func hexSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key, data []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(data)
	return m.Sum(nil)
}
