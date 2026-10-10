package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// With no key the REPL starts in the setup wizard; the fake model answers
// the verification call and the key is saved. The REPL runs in plain
// (non-TTY) mode here, so the provider list is the numbered text form.
func TestSetupWizardConfiguresKeyOnFirstRun(t *testing.T) {
	fake := newFakeModel(t, fakeStep{Content: "ok"})
	_, _ = e2eHomeWithoutKey(t, fake)
	s := startREPL(t)
	s.WaitFor("选择模型供应商", e2eTimeout)
	s.Type("10") // openai-compatible keeps the config's base_url (the fake)
	s.WaitFor("API key", e2eTimeout)
	s.Type("sk-test-123")
	s.WaitFor("已保存到", e2eTimeout)
	if strings.Contains(s.Output(), "sk-test-123") {
		t.Fatal("the key must never be echoed")
	}
	data, err := os.ReadFile(filepath.Join(os.Getenv("COVE_CONFIG_DIR"), "config.json"))
	if err != nil || !strings.Contains(string(data), "sk-test-123") {
		t.Fatalf("key not saved: %v %s", err, data)
	}
	if len(fake.Requests()) != 1 {
		t.Fatalf("verification must be one request, got %d", len(fake.Requests()))
	}
	s.Type("/exit")
	s.WaitFor("再见", e2eTimeout)
}

func TestSetupWizardEmptyKeySkips(t *testing.T) {
	fake := newFakeModel(t)
	_, _ = e2eHomeWithoutKey(t, fake)
	s := startREPL(t)
	s.WaitFor("选择模型供应商", e2eTimeout)
	s.Type("1")
	s.WaitFor("API key", e2eTimeout)
	s.Type("")
	s.WaitFor("已跳过配置", e2eTimeout)
	s.Type("hello")
	s.WaitFor("未配置 API key", e2eTimeout)
	if len(fake.Requests()) != 0 {
		t.Fatal("no request may be sent without a key")
	}
	s.Type("/exit")
	s.WaitFor("再见", e2eTimeout)
}

func TestSetupWizardVerifyFailureOffersRetryOrSkip(t *testing.T) {
	fake := newFakeModel(t, fakeStep{Status: 401, Body: `{"error":{"message":"bad key"}}`})
	_, _ = e2eHomeWithoutKey(t, fake)
	s := startREPL(t)
	s.WaitFor("选择模型供应商", e2eTimeout)
	s.Type("10")
	s.WaitFor("API key", e2eTimeout)
	s.Type("sk-wrong")
	s.WaitFor("验证失败", e2eTimeout)
	s.WaitFor("[r] 重新输入", e2eTimeout)
	s.Type("s")
	s.WaitFor("已跳过配置", e2eTimeout)
	data, _ := os.ReadFile(filepath.Join(os.Getenv("COVE_CONFIG_DIR"), "config.json"))
	if strings.Contains(string(data), "sk-wrong") {
		t.Fatal("a key that failed verification must not be saved")
	}
	s.Type("hello")
	s.WaitFor("未配置 API key", e2eTimeout)
	s.Type("/exit")
	s.WaitFor("再见", e2eTimeout)
}

func TestSetupWizardDisabledByEnv(t *testing.T) {
	fake := newFakeModel(t)
	_, _ = e2eHomeWithoutKey(t, fake)
	t.Setenv("COVE_NO_SETUP", "1")
	s := startREPL(t)
	s.Type("hello")
	s.WaitFor("未配置 API key", e2eTimeout)
	if strings.Contains(s.Output(), "选择模型供应商") {
		t.Fatal("wizard must be off")
	}
	s.Type("/exit")
	s.WaitFor("再见", e2eTimeout)
}
