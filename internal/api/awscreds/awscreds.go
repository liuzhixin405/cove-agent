// Package awscreds resolves static AWS credentials the way the AWS SDKs do
// on a developer machine: environment variables, then the shared
// credentials file, then the config file. It is what cove's bedrock
// provider signs with when no Bedrock API key is configured.
//
// Not supported, by design: credential_process, SSO, assumed roles,
// web identity, and the EC2/ECS metadata services. A profile that relies
// on them is reported as such with the aws-cli command that exports
// static credentials.
package awscreds

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	// Source says where the credentials came from, for diagnostics only:
	// "env", "credentials:<profile>" or "config:<profile>".
	Source string
}

// NotFoundError: no source held static credentials. Detail says what was
// looked at; it never contains a credential value.
type NotFoundError struct{ Detail string }

func (e *NotFoundError) Error() string {
	msg := "bedrock credentials required: set AWS_BEARER_TOKEN_BEDROCK (Bedrock API key), or AWS_ACCESS_KEY_ID + AWS_SECRET_ACCESS_KEY (+ AWS_SESSION_TOKEN), or a static profile in ~/.aws/credentials (AWS_PROFILE)"
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	return msg
}

// UnsupportedProfileError: the profile exists but gets its credentials
// through a mechanism cove does not implement.
type UnsupportedProfileError struct {
	Profile string
	Key     string
}

func (e *UnsupportedProfileError) Error() string {
	return fmt.Sprintf("aws profile %q uses %s, which cove does not support; export static credentials first: aws configure export-credentials --profile %s --format env", e.Profile, e.Key, e.Profile)
}

// unsupportedKeys mark a profile that needs the full AWS credential chain.
var unsupportedKeys = []string{"credential_process", "sso_session", "sso_start_url", "role_arn", "web_identity_token_file", "credential_source"}

// Resolve returns the first static credentials found: environment, shared
// credentials file, config file. It reads the files on every call, so
// credentials refreshed on disk take effect without a restart.
func Resolve() (Credentials, error) {
	if c, ok := fromEnv(); ok {
		return c, nil
	}
	var detail []string
	id, secret := strings.TrimSpace(os.Getenv("AWS_ACCESS_KEY_ID")), strings.TrimSpace(os.Getenv("AWS_SECRET_ACCESS_KEY"))
	if (id == "") != (secret == "") {
		missing := "AWS_SECRET_ACCESS_KEY"
		if id == "" {
			missing = "AWS_ACCESS_KEY_ID"
		}
		detail = append(detail, "environment sets only one of the pair, "+missing+" is missing")
	}
	profile := profileName()
	for _, src := range []struct {
		label, path string
		config      bool
	}{
		{"credentials", credentialsPath(), false},
		{"config", configPath(), true},
	} {
		c, found, err := fromFile(src.path, profile, src.config)
		if err != nil {
			return Credentials{}, err
		}
		if found {
			c.Source = src.label + ":" + profile
			return c, nil
		}
	}
	detail = append(detail, fmt.Sprintf("profile %q not found in %s or %s", profile, credentialsPath(), configPath()))
	return Credentials{}, &NotFoundError{Detail: strings.Join(detail, "; ")}
}

// ProfileRegion is the region of the current profile in the config file,
// or "" when there is none.
func ProfileRegion() string {
	sections, err := readINI(configPath())
	if err != nil {
		return ""
	}
	if sec, ok := lookupSection(sections, profileName(), true); ok {
		return sec["region"]
	}
	return ""
}

func fromEnv() (Credentials, bool) {
	id, secret := strings.TrimSpace(os.Getenv("AWS_ACCESS_KEY_ID")), strings.TrimSpace(os.Getenv("AWS_SECRET_ACCESS_KEY"))
	if id == "" || secret == "" {
		return Credentials{}, false
	}
	return Credentials{AccessKeyID: id, SecretAccessKey: secret, SessionToken: strings.TrimSpace(os.Getenv("AWS_SESSION_TOKEN")), Source: "env"}, true
}

// fromFile reads profile from the INI file at path. isConfig selects the
// config file's "[profile x]" naming. found is false when the file or the
// profile is absent; an unsupported profile is an error.
func fromFile(path, profile string, isConfig bool) (Credentials, bool, error) {
	sections, err := readINI(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Credentials{}, false, nil
		}
		return Credentials{}, false, fmt.Errorf("read aws credentials %s: %w", path, err)
	}
	sec, ok := lookupSection(sections, profile, isConfig)
	if !ok {
		return Credentials{}, false, nil
	}
	id, secret := sec["aws_access_key_id"], sec["aws_secret_access_key"]
	if id != "" && secret != "" {
		return Credentials{AccessKeyID: id, SecretAccessKey: secret, SessionToken: sec["aws_session_token"]}, true, nil
	}
	for _, k := range unsupportedKeys {
		if _, has := sec[k]; has {
			return Credentials{}, false, &UnsupportedProfileError{Profile: profile, Key: k}
		}
	}
	return Credentials{}, false, nil
}

// lookupSection finds profile in sections: "[default]" is always bare; in
// the config file other profiles are "[profile name]".
func lookupSection(sections map[string]map[string]string, profile string, isConfig bool) (map[string]string, bool) {
	name := strings.ToLower(profile)
	if isConfig && name != "default" {
		name = "profile " + name
	}
	sec, ok := sections[name]
	return sec, ok
}

func readINI(path string) (map[string]map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseINI(string(raw)), nil
}

func profileName() string {
	if p := strings.TrimSpace(os.Getenv("AWS_PROFILE")); p != "" {
		return p
	}
	return "default"
}

func credentialsPath() string {
	if p := strings.TrimSpace(os.Getenv("AWS_SHARED_CREDENTIALS_FILE")); p != "" {
		return p
	}
	return filepath.Join(homeDir(), ".aws", "credentials")
}

func configPath() string {
	if p := strings.TrimSpace(os.Getenv("AWS_CONFIG_FILE")); p != "" {
		return p
	}
	return filepath.Join(homeDir(), ".aws", "config")
}

func homeDir() string {
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return "."
}
