package awscreds

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// clearEnv blanks every variable Resolve reads so the test owns them.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_PROFILE", "AWS_SHARED_CREDENTIALS_FILE", "AWS_CONFIG_FILE"} {
		t.Setenv(k, "")
	}
	// Point both files at an empty temp dir so a developer's real ~/.aws
	// never leaks into a test.
	dir := t.TempDir()
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "credentials"))
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "config"))
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestResolveFromEnvironment(t *testing.T) {
	clearEnv(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	c, err := Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if c.AccessKeyID != "AKIDEXAMPLE" || c.SecretAccessKey != "secret" || c.SessionToken != "" || c.Source != "env" {
		t.Fatalf("got %+v", c)
	}
	t.Setenv("AWS_SESSION_TOKEN", "tok")
	c, _ = Resolve()
	if c.SessionToken != "tok" {
		t.Fatalf("session token not read: %+v", c)
	}
}

func TestResolveHalfEnvironmentIsNotFoundAndNamesTheGap(t *testing.T) {
	clearEnv(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	_, err := Resolve()
	var nf *NotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("err = %v, want *NotFoundError", err)
	}
	if !strings.Contains(err.Error(), "AWS_SECRET_ACCESS_KEY is missing") || !strings.Contains(err.Error(), "AWS_BEARER_TOKEN_BEDROCK") {
		t.Fatalf("message should name the missing variable and the bearer alternative: %v", err)
	}
	if strings.Contains(err.Error(), "AKIDEXAMPLE") {
		t.Fatal("message must not echo credential values")
	}
}

func TestResolveFromCredentialsFileDefaultProfileWithCRLF(t *testing.T) {
	clearEnv(t)
	writeFile(t, os.Getenv("AWS_SHARED_CREDENTIALS_FILE"),
		"# comment\r\n[default]\r\naws_access_key_id = AKIDFILE\r\naws_secret_access_key=wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY \r\naws_session_token = tok\r\n")
	c, err := Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if c.AccessKeyID != "AKIDFILE" || c.SecretAccessKey != "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY" || c.SessionToken != "tok" {
		t.Fatalf("CRLF/whitespace not trimmed: %+v", c)
	}
	if c.Source != "credentials:default" {
		t.Fatalf("Source = %q", c.Source)
	}
}

func TestResolveHonoursAWSProfileAndConfigFileSectionNaming(t *testing.T) {
	clearEnv(t)
	t.Setenv("AWS_PROFILE", "work")
	writeFile(t, os.Getenv("AWS_SHARED_CREDENTIALS_FILE"), "[default]\naws_access_key_id = WRONG\naws_secret_access_key = WRONG\n")
	// config uses "[profile work]"; a bare "[work]" there is not a profile.
	writeFile(t, os.Getenv("AWS_CONFIG_FILE"), "[work]\naws_access_key_id = BARE\naws_secret_access_key = BARE\n\n[profile work]\nregion = eu-central-1\naws_access_key_id = AKIDWORK\naws_secret_access_key = s\n")
	c, err := Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if c.AccessKeyID != "AKIDWORK" || c.Source != "config:work" {
		t.Fatalf("got %+v", c)
	}
	if got := ProfileRegion(); got != "eu-central-1" {
		t.Fatalf("ProfileRegion = %q", got)
	}
}

func TestResolveCredentialsFileWinsOverConfigFile(t *testing.T) {
	clearEnv(t)
	writeFile(t, os.Getenv("AWS_SHARED_CREDENTIALS_FILE"), "[default]\naws_access_key_id = AKIDCRED\naws_secret_access_key = s\n")
	writeFile(t, os.Getenv("AWS_CONFIG_FILE"), "[default]\naws_access_key_id = AKIDCONF\naws_secret_access_key = s\n")
	c, err := Resolve()
	if err != nil || c.AccessKeyID != "AKIDCRED" {
		t.Fatalf("got %+v, %v", c, err)
	}
}

func TestResolveUnsupportedProfileNamesTheKey(t *testing.T) {
	clearEnv(t)
	writeFile(t, os.Getenv("AWS_CONFIG_FILE"), "[default]\nsso_start_url = https://x.awsapps.com/start\nsso_region = us-east-1\nregion = us-east-1\n")
	_, err := Resolve()
	var up *UnsupportedProfileError
	if !errors.As(err, &up) || up.Key != "sso_start_url" || up.Profile != "default" {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "aws configure export-credentials --profile default --format env") {
		t.Fatalf("message lacks the workaround: %v", err)
	}
}

func TestResolveMissingFilesAndProfileIsNotFound(t *testing.T) {
	clearEnv(t)
	if _, err := Resolve(); err == nil || !errors.As(err, new(*NotFoundError)) {
		t.Fatalf("no files: err = %v", err)
	}
	t.Setenv("AWS_PROFILE", "nope")
	writeFile(t, os.Getenv("AWS_SHARED_CREDENTIALS_FILE"), "[default]\naws_access_key_id = a\naws_secret_access_key = b\n")
	_, err := Resolve()
	if err == nil || !strings.Contains(err.Error(), `profile "nope"`) {
		t.Fatalf("missing profile should be named: %v", err)
	}
	if ProfileRegion() != "" {
		t.Fatal("ProfileRegion without config must be empty")
	}
}

func TestParseINIBasics(t *testing.T) {
	sections := parseINI("; c\n[A]\nk1 = v1\nK2=v2\n[profile b]\nk = v\n")
	if sections["a"]["k1"] != "v1" || sections["a"]["k2"] != "v2" {
		t.Fatalf("section a = %v", sections["a"])
	}
	if sections["profile b"]["k"] != "v" {
		t.Fatalf("section 'profile b' = %v", sections["profile b"])
	}
	if _, ok := sections[""]; ok {
		t.Fatal("keys before the first section must be dropped")
	}
}

// PowerShell 5.1's Out-File / Set-Content -Encoding utf8 and older Notepad
// write a UTF-8 BOM; the first section must still be found.
func TestResolveCredentialsFileWithUTF8BOM(t *testing.T) {
	clearEnv(t)
	writeFile(t, os.Getenv("AWS_SHARED_CREDENTIALS_FILE"), string(rune(0xFEFF))+"[default]\r\naws_access_key_id = AKIDBOM\r\naws_secret_access_key = s\r\n")
	c, err := Resolve()
	if err != nil || c.AccessKeyID != "AKIDBOM" {
		t.Fatalf("BOM file: %+v, %v", c, err)
	}
}
