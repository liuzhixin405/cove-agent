package sigv4

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/api/awscreds"
)

// AWS's published SigV4 test-suite values (botocore tests/unit/auth/aws4_testsuite).
var (
	suiteCreds = awscreds.Credentials{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"}
	suiteTime  = time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)
)

func TestComputeMatchesSuiteGetVanilla(t *testing.T) {
	h := http.Header{}
	h.Set("X-Amz-Date", "20150830T123600Z")
	r := Compute(Params{Method: "GET", Host: "example.amazonaws.com", Path: "/", Headers: h,
		Region: "us-east-1", Service: "service", Now: suiteTime, Creds: suiteCreds})
	wantCReq := "GET\n/\n\nhost:example.amazonaws.com\nx-amz-date:20150830T123600Z\n\nhost;x-amz-date\ne3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if r.CanonicalRequest != wantCReq {
		t.Fatalf("canonical request:\n%s\nwant:\n%s", r.CanonicalRequest, wantCReq)
	}
	wantSTS := "AWS4-HMAC-SHA256\n20150830T123600Z\n20150830/us-east-1/service/aws4_request\nbb579772317eb040ac9ed261061d46c1f17a8133879d6129b6e1c25292927e63"
	if r.StringToSign != wantSTS {
		t.Fatalf("string to sign:\n%s\nwant:\n%s", r.StringToSign, wantSTS)
	}
	wantAuthz := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=host;x-amz-date, Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31"
	if r.Authorization != wantAuthz {
		t.Fatalf("authorization:\n%s\nwant:\n%s", r.Authorization, wantAuthz)
	}
}

func TestComputeMatchesSuitePostFormURLEncoded(t *testing.T) {
	h := http.Header{}
	h.Set("Content-Type", "application/x-www-form-urlencoded")
	h.Set("X-Amz-Date", "20150830T123600Z")
	r := Compute(Params{Method: "POST", Host: "example.amazonaws.com", Path: "/", Headers: h, Payload: []byte("Param1=value1"),
		Region: "us-east-1", Service: "service", Now: suiteTime, Creds: suiteCreds})
	wantCReq := "POST\n/\n\ncontent-type:application/x-www-form-urlencoded\nhost:example.amazonaws.com\nx-amz-date:20150830T123600Z\n\ncontent-type;host;x-amz-date\n9095672bbd1f56dfc5b65f3e153adc8731a4a654192329106275f4c7b24d0b6e"
	if r.CanonicalRequest != wantCReq {
		t.Fatalf("canonical request:\n%s\nwant:\n%s", r.CanonicalRequest, wantCReq)
	}
	if !strings.HasSuffix(r.StringToSign, "42a5e5bb34198acb3e84da4f085bb7927f2bc277ca766e6d19c73c2154021281") {
		t.Fatalf("string to sign hash wrong:\n%s", r.StringToSign)
	}
	wantAuthz := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=content-type;host;x-amz-date, Signature=ff11897932ad3f4e8b18135d722051e5ac45fc38421b1da7b9d196a0fe09473a"
	if r.Authorization != wantAuthz {
		t.Fatalf("authorization:\n%s\nwant:\n%s", r.Authorization, wantAuthz)
	}
}

// Header values are trimmed and runs of spaces collapsed; names sorted.
func TestComputeCanonicalizesHeaderValues(t *testing.T) {
	h := http.Header{}
	h.Set("X-Amz-Date", "20150830T123600Z")
	h.Set("Content-Type", "  application/json ;  charset=utf-8  ")
	r := Compute(Params{Method: "POST", Host: "example.amazonaws.com", Path: "/anthropic/v1/messages", Headers: h,
		Region: "us-east-1", Service: "bedrock-mantle", Now: suiteTime, Creds: suiteCreds})
	if !strings.Contains(r.CanonicalRequest, "\ncontent-type:application/json ; charset=utf-8\nhost:") {
		t.Fatalf("header value not canonicalized:\n%s", r.CanonicalRequest)
	}
	if r.SignedHeaders != "content-type;host;x-amz-date" {
		t.Fatalf("SignedHeaders = %q", r.SignedHeaders)
	}
}

// Query strings are sorted by key then value and RFC 3986 encoded.
func TestComputeCanonicalQuery(t *testing.T) {
	h := http.Header{}
	h.Set("X-Amz-Date", "20150830T123600Z")
	r := Compute(Params{Method: "GET", Host: "example.amazonaws.com", Path: "/", RawQuery: "b=2&a=x%20y&a=1", Headers: h,
		Region: "us-east-1", Service: "service", Now: suiteTime, Creds: suiteCreds})
	if !strings.HasPrefix(r.CanonicalRequest, "GET\n/\na=1&a=x%20y&b=2\n") {
		t.Fatalf("canonical query:\n%s", r.CanonicalRequest)
	}
}

// SignRequest wires the Bedrock header set: date, payload hash, optional
// session token, and keeps the host (with port) exactly as the URL has it.
func TestSignRequestSetsBedrockHeaders(t *testing.T) {
	body := []byte(`{"model":"anthropic.claude-sonnet-5","messages":[{"role":"user","content":"你好"}]}`)
	req, _ := http.NewRequest("POST", "https://127.0.0.1:34567/anthropic/v1/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	// A clock in UTC+8 must still produce a UTC x-amz-date.
	now := time.Date(2026, 10, 14, 20, 0, 0, 0, time.FixedZone("CST", 8*3600))
	creds := awscreds.Credentials{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "secret", SessionToken: "tok"}
	SignRequest(req, body, creds, "us-east-1", "bedrock-mantle", now)

	if got := req.Header.Get("x-amz-date"); got != "20261014T120000Z" {
		t.Fatalf("x-amz-date = %q, want UTC 20261014T120000Z", got)
	}
	if req.Header.Get("x-amz-security-token") != "tok" {
		t.Fatal("session token header missing")
	}
	if len(req.Header.Get("x-amz-content-sha256")) != 64 {
		t.Fatalf("payload hash = %q", req.Header.Get("x-amz-content-sha256"))
	}
	authz := req.Header.Get("Authorization")
	if !strings.HasPrefix(authz, "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20261014/us-east-1/bedrock-mantle/aws4_request, SignedHeaders=content-type;host;x-amz-content-sha256;x-amz-date;x-amz-security-token, Signature=") {
		t.Fatalf("Authorization = %q", authz)
	}
	// The same inputs through Compute must agree (host keeps its port).
	toSign := http.Header{}
	for _, n := range []string{"Content-Type", "x-amz-content-sha256", "x-amz-date", "x-amz-security-token"} {
		toSign.Set(n, req.Header.Get(n))
	}
	want := Compute(Params{Method: "POST", Host: "127.0.0.1:34567", Path: "/anthropic/v1/messages", Headers: toSign, Payload: body,
		Region: "us-east-1", Service: "bedrock-mantle", Now: now, Creds: creds})
	if authz != want.Authorization {
		t.Fatalf("SignRequest and Compute disagree:\n%s\n%s", authz, want.Authorization)
	}
	if !strings.Contains(want.CanonicalRequest, "\nhost:127.0.0.1:34567\n") {
		t.Fatalf("host with port not preserved:\n%s", want.CanonicalRequest)
	}
}

func TestSignRequestWithoutSessionTokenOmitsHeader(t *testing.T) {
	req, _ := http.NewRequest("POST", "https://bedrock-mantle.us-east-1.api.aws/anthropic/v1/messages", bytes.NewReader([]byte("{}")))
	req.Header.Set("Content-Type", "application/json")
	SignRequest(req, []byte("{}"), awscreds.Credentials{AccessKeyID: "a", SecretAccessKey: "b"}, "us-east-1", "bedrock-mantle", suiteTime)
	if req.Header.Get("x-amz-security-token") != "" {
		t.Fatal("no session token must mean no header")
	}
	if !strings.Contains(req.Header.Get("Authorization"), "SignedHeaders=content-type;host;x-amz-content-sha256;x-amz-date,") {
		t.Fatalf("Authorization = %q", req.Header.Get("Authorization"))
	}
}
