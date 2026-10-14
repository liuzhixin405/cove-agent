package command

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/config"
)

// A bedrock run with IAM credentials only signs with SigV4; /doctor said
// "API key: 未设置" and /config showed no credentials at all.
func TestDoctorAndConfigReportBedrockSigV4(t *testing.T) {
	dir := t.TempDir()
	for k, v := range map[string]string{
		"AWS_BEARER_TOKEN_BEDROCK": "", "BEDROCK_API_KEY": "", "LLM_API_KEY": "", "LLM_BASE_URL": "", "AWS_SESSION_TOKEN": "", "AWS_PROFILE": "",
		"AWS_SHARED_CREDENTIALS_FILE": filepath.Join(dir, "credentials"), "AWS_CONFIG_FILE": filepath.Join(dir, "config"),
		"AWS_ACCESS_KEY_ID": "AKIDEXAMPLE", "AWS_SECRET_ACCESS_KEY": "secret", "AWS_REGION": "us-east-1",
	} {
		t.Setenv(k, v)
	}
	cfg := &config.Config{Model: "anthropic.claude-sonnet-5", Provider: config.ProviderConfig{Name: "bedrock"}}
	out, err := (&DoctorCmd{}).Execute(context.Background(), Input{Config: cfg, Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.Message, "API key: 未设置") || !strings.Contains(out.Message, "SigV4") || !strings.Contains(out.Message, "env") {
		t.Fatalf("/doctor output:\n%s", out.Message)
	}
	if strings.Contains(out.Message, "secret") || strings.Contains(out.Message, "AKIDEXAMPLE") {
		t.Fatal("/doctor must not print credential values")
	}
	if rc := renderConfig(cfg); !strings.Contains(rc, `"aws_credentials": "env"`) {
		t.Fatalf("renderConfig:\n%s", rc)
	}
}
