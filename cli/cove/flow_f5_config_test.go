package main

// Flow F5 (test design 5.3): configuration, profiles and project trust —
// /config, /profile save|switch, a profile field cleared to zero, the project
// .cove.json trust gate with /trust and /restart, /cd to another project and
// a .cove.json that is a link or too large. One home and one fake model are
// shared; the subtests run in order and some build on the config the earlier
// ones saved (the "work" profile).
//
// The start-up notices (untrusted fields, an ignored .cove.json) are written
// by main to stderr before any front end starts, so those branches run cove
// as a child process with -p (runCove); the rest drive the in-process REPL.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFlowF5_ConfigProfileTrust(t *testing.T) {
	model := newFakeModel(t)
	home, project := e2eHome(t, model)
	cfgDir := filepath.Join(home, ".cove")
	cfgPath := filepath.Join(cfgDir, "config.json")
	trustPath := filepath.Join(cfgDir, "trusted_projects.json")

	start := func(t *testing.T, steps ...fakeStep) (*e2eSession, flow) {
		t.Helper()
		model.Reset(steps...)
		s := startREPL(t)
		return s, flow{t: t, s: s, model: model}
	}
	readFile := func(t *testing.T, path string) []byte {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		return b
	}
	str := func(raw json.RawMessage) string {
		var s string
		_ = json.Unmarshal(raw, &s)
		return s
	}
	// newProject makes a sibling project directory with the given
	// .cove.json ("" for none).
	newProject := func(t *testing.T, name, coveJSON string) string {
		t.Helper()
		dir := filepath.Join(home, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if coveJSON != "" {
			writeTestFile(t, filepath.Join(dir, ".cove.json"), coveJSON)
		}
		return dir
	}
	trustStore := func() map[string]string {
		m := map[string]string{}
		if b, err := os.ReadFile(trustPath); err == nil {
			_ = json.Unmarshal(b, &m)
		}
		return m
	}
	trusted := func(sub string) bool {
		for k := range trustStore() {
			if strings.Contains(strings.ToLower(k), strings.ToLower(sub)) {
				return true
			}
		}
		return false
	}

	// ---------- /config ----------

	t.Run("/config设置model后下一请求用新模型", func(t *testing.T) {
		s, f := start(t, textReply("新模型已经接手，这是它的第一条回答，一切正常。"))
		mark := s.Mark()
		s.Type("/config model qwen-f5-new")
		s.WaitForSince(mark, "已保存 model = qwen-f5-new", e2eTimeout)
		s.Type("换了模型之后说一句话")
		s.WaitFor("这是它的第一条回答", e2eTimeout)
		s.WaitIdle(e2eTimeout)
		if strings.Contains(s.OutputSince(mark), "重启 cove 后生效") {
			f.Fatalf("/config model was not applied to the running session")
		}
		if got := f.Requests(1)[0].Model; got != "qwen-f5-new" {
			f.Fatalf("request model = %q, want qwen-f5-new after /config model", got)
		}
		if got := str(readConfigFile(t, home)["model"]); got != "qwen-f5-new" {
			f.Fatalf("config.json model = %q, want qwen-f5-new", got)
		}
		// Back to the home's model for the subtests that follow.
		mark = s.Mark()
		s.Type("/config model qwen-test")
		s.WaitForSince(mark, "已保存 model = qwen-test", e2eTimeout)
	})

	t.Run("/config非法键报错且config.json不变", func(t *testing.T) {
		before := readFile(t, cfgPath)
		s, f := start(t, textReply("模型没变，还是原来的那个，回答完毕。"))
		mark := s.Mark()
		// "set" is not a key: the syntax is /config <键> <值>.
		s.Type("/config set model qwen-f5-bad")
		s.WaitForSince(mark, "unknown config key: set", e2eTimeout)
		s.Type("/config colour red")
		s.WaitForSince(mark, "unknown config key: colour", e2eTimeout)
		s.Type("非法配置之后问一句")
		s.WaitFor("还是原来的那个", e2eTimeout)
		s.WaitIdle(e2eTimeout)
		if strings.Contains(s.OutputSince(mark), "已保存") {
			f.Fatalf("an invalid /config key was reported as saved")
		}
		if got := f.Requests(1)[0].Model; got != "qwen-test" {
			f.Fatalf("request model = %q after an invalid /config, want qwen-test", got)
		}
		if after := readFile(t, cfgPath); !bytes.Equal(before, after) {
			f.Fatalf("config.json changed after invalid /config:\nbefore:\n%s\nafter:\n%s", before, after)
		}
	})

	// ---------- /profile ----------

	// profileKeys are the fields a saved profile may hold (config.Profile).
	profileKeys := map[string]bool{
		"model": true, "model_fast": true, "provider": true, "permission_mode": true,
		"max_budget_usd": true, "debug": true, "verbose": true,
		"system_prompt": true, "max_iterations": true, "max_turn_minutes": true,
		"subagent_max_iterations": true, "max_sessions": true,
	}
	profile := func(t *testing.T, name string) map[string]json.RawMessage {
		t.Helper()
		var profiles map[string]map[string]json.RawMessage
		if err := json.Unmarshal(readConfigFile(t, home)["profiles"], &profiles); err != nil || profiles[name] == nil {
			t.Fatalf("profile %s not in config.json (%v):\n%s", name, err, readFile(t, cfgPath))
		}
		return profiles[name]
	}

	t.Run("/profile save只保存profile字段不含项目.cove.json的值", func(t *testing.T) {
		dir := newProject(t, "f5-proj-profile", `{"model": "qwen-f5-project", "effort": "high", "max_iterations": 7}`)
		t.Chdir(dir)
		s, f := start(t, textReply("项目配置里的模型在回答，本会话照常使用它。"))
		mark := s.Mark()
		s.Type("/profile save work")
		s.WaitForSince(mark, "profile 已保存: work", e2eTimeout)
		s.Type("用项目模型回答一句")
		s.WaitFor("本会话照常使用它", e2eTimeout)
		s.WaitIdle(e2eTimeout)
		// The project's model still drives this session...
		if got := f.Requests(1)[0].Model; got != "qwen-f5-project" {
			f.Fatalf("request model = %q, want the project's qwen-f5-project", got)
		}
		// ...but the saved profile holds the user's own settings only.
		p := profile(t, "work")
		for k := range p {
			if !profileKeys[k] {
				f.Fatalf("profile work holds %q, which is not a profile field: %v", k, p)
			}
		}
		if got := str(p["model"]); got != "qwen-test" {
			f.Fatalf("profile work model = %q, want the user's qwen-test (not the project's)", got)
		}
		if _, ok := p["max_iterations"]; ok {
			f.Fatalf("profile work took the project's max_iterations: %s", p["max_iterations"])
		}
		if raw := readFile(t, cfgPath); bytes.Contains(raw, []byte("qwen-f5-project")) || bytes.Contains(raw, []byte(`"effort"`)) {
			f.Fatalf("a .cove.json value reached config.json:\n%s", raw)
		}
	})

	t.Run("/profile switch后/config显示切换后的值", func(t *testing.T) {
		s, f := start(t, textReply("切换 profile 以后由 work 的模型回答，回答完毕。"))
		mark := s.Mark()
		s.Type("/model qwen-f5-home")
		s.WaitForSince(mark, "模型: qwen-f5-home（已保存）", e2eTimeout)
		s.Type("/profile save home")
		s.WaitForSince(mark, "profile 已保存: home", e2eTimeout)

		mark = s.Mark()
		s.Type("/profile switch work")
		s.WaitForSince(mark, "已切换到 profile: work", e2eTimeout)
		s.Type("/config")
		s.WaitForSince(mark, `"model": "qwen-test"`, e2eTimeout)
		s.Type("切换之后问一句")
		s.WaitFor("由 work 的模型回答", e2eTimeout)
		s.WaitIdle(e2eTimeout)
		if got := f.Requests(1)[0].Model; got != "qwen-test" {
			f.Fatalf("request model = %q after /profile switch work, want qwen-test", got)
		}

		mark = s.Mark()
		s.Type("/profile switch home")
		s.WaitForSince(mark, "已切换到 profile: home", e2eTimeout)
		s.Type("/config")
		s.WaitForSince(mark, `"model": "qwen-f5-home"`, e2eTimeout)

		cfg := readConfigFile(t, home)
		if got := str(cfg["active_profile"]); got != "home" {
			f.Fatalf("config.json active_profile = %q, want home", got)
		}
		if got := str(profile(t, "home")["model"]); got != "qwen-f5-home" {
			f.Fatalf("profile home model = %q", got)
		}
		// work is the active profile for the next subtest.
		mark = s.Mark()
		s.Type("/profile switch work")
		s.WaitForSince(mark, "已切换到 profile: work", e2eTimeout)
	})

	t.Run("清零profile字段后重启仍为0", func(t *testing.T) {
		if b := profile(t, "work")["max_budget_usd"]; string(b) != "10" {
			t.Fatalf("precondition: profile work max_budget_usd = %s, want 10", b)
		}
		s, _ := start(t)
		mark := s.Mark()
		s.Type("/budget")
		s.WaitForSince(mark, "当前预算: $10.00", e2eTimeout)
		// /budget only takes amounts above 0; off + save is how the cap is
		// cleared in the config.
		s.Type("/budget off")
		s.WaitForSince(mark, "已取消本会话的预算上限", e2eTimeout)
		s.Type("/budget save")
		s.WaitForSince(mark, "已把预算 无上限 写入配置", e2eTimeout)
		s.Exit()

		p := profile(t, "work")
		if b, ok := p["max_budget_usd"]; ok && string(b) != "0" {
			t.Fatalf("profile work still holds max_budget_usd %s after clearing it", b)
		}
		if b := readConfigFile(t, home)["max_budget_usd"]; string(b) != "0" {
			t.Fatalf("config.json max_budget_usd = %s, want 0 (else the next start falls back to it)", b)
		}

		s2, f2 := start(t, textReply("没有预算上限也照常回答，回答完毕。"))
		mark = s2.Mark()
		s2.Type("/budget")
		s2.WaitForSince(mark, "当前未设置预算上限", e2eTimeout)
		s2.Type("重启之后问一句")
		s2.WaitFor("照常回答", e2eTimeout)
		s2.WaitIdle(e2eTimeout)
		if strings.Contains(s2.OutputSince(mark), "当前预算: $") {
			f2.Fatalf("the cleared budget came back after the restart")
		}
		if got := f2.Requests(1)[0].Model; got != "qwen-test" {
			f2.Fatalf("request model = %q under profile work", got)
		}

		// Leave the profiles behind: the trust subtests run on the base config.
		mark = s2.Mark()
		s2.Type("/profile delete work")
		s2.WaitForSince(mark, "profile 已删除: work", e2eTimeout)
		s2.Type("/profile delete home")
		s2.WaitForSince(mark, "profile 已删除: home", e2eTimeout)
		// Two changes: the value in memory is still the deleted profile's
		// qwen-test, so "/model qwen-test" alone would change nothing to
		// save (see the next subtest).
		s2.Type("/model qwen-f5-reset")
		s2.WaitForSince(mark, "模型: qwen-f5-reset（已保存）", e2eTimeout)
		s2.Type("/model qwen-test")
		s2.WaitForSince(mark, "模型: qwen-test（已保存）", e2eTimeout)
		s2.Exit()
		if got := str(readConfigFile(t, home)["model"]); got != "qwen-test" {
			f2.Fatalf("config.json model = %q after the clean-up, want qwen-test", got)
		}
	})

	t.Run("删除当前profile后/model设为同值仍写入配置", func(t *testing.T) {
		s, _ := start(t)
		mark := s.Mark()
		s.Type("/model qwen-f5-pm")
		s.WaitForSince(mark, "模型: qwen-f5-pm（已保存）", e2eTimeout)
		s.Type("/profile save pm")
		s.WaitForSince(mark, "profile 已保存: pm", e2eTimeout)
		s.Type("/model qwen-test")
		s.WaitForSince(mark, "模型: qwen-test（已保存）", e2eTimeout)
		s.Type("/profile switch pm")
		s.WaitForSince(mark, "已切换到 profile: pm", e2eTimeout)
		s.Exit()
		// The top level says qwen-test, the active profile qwen-f5-pm.
		s2, f2 := start(t, textReply("删除 profile 以后按保存的模型回答，回答完毕。"))
		mark = s2.Mark()
		s2.Type("/profile delete pm")
		s2.WaitForSince(mark, "profile 已删除: pm", e2eTimeout)
		s2.Type("/model qwen-f5-pm")
		s2.WaitForSince(mark, "模型: qwen-f5-pm（已保存）", e2eTimeout)
		s2.Exit()
		if got := str(readConfigFile(t, home)["model"]); got != "qwen-f5-pm" {
			// Leave the home as the next subtests expect it.
			s3, _ := start(t)
			m3 := s3.Mark()
			s3.Type("/model qwen-test")
			s3.WaitForSince(m3, "（已保存）", e2eTimeout)
			s3.Exit()
			f2.Fatalf("/model qwen-f5-pm said 已保存 but config.json model = %q: the next start runs another model", got)
		}
		// The next start runs the saved model. Checked through /config rather
		// than the request body: the deleted profile also carried the fast
		// model it was saved with (qwen-test), which now lives at the top
		// level too, and a one-line question is routed to it exactly as it
		// was before the profile was deleted.
		s3, f3 := start(t, textReply("重启以后仍是刚才保存的模型，回答完毕。"))
		m3 := s3.Mark()
		s3.Type("/config")
		s3.WaitForSince(m3, `"model": "qwen-f5-pm"`, e2eTimeout)
		s3.Type("重启之后问一句")
		s3.WaitFor("仍是刚才保存的模型", e2eTimeout)
		s3.WaitIdle(e2eTimeout)
		f3.Requests(1)
		m3 = s3.Mark()
		s3.Type("/model qwen-test")
		s3.WaitForSince(m3, "模型: qwen-test（已保存）", e2eTimeout)
		s3.Exit()
		// The deleted profile left its explicit fast model at the top level;
		// later subtests expect the fast model to follow the model again.
		m := readConfigFile(t, home)
		delete(m, "model_fast")
		b, _ := json.MarshalIndent(m, "", "  ")
		writeTestFile(t, cfgPath, string(b))
	})

	// ---------- project .cove.json trust ----------

	const trustMarker = "F5-TRUST-MARKER 项目提示词"
	trustDir := newProject(t, "f5-proj-trust", `{"system_prompt": "`+trustMarker+`"}`)

	t.Run("未信任的.cove.json需信任的键不生效并提示", func(t *testing.T) {
		t.Chdir(trustDir)
		model.Reset(textReply("未信任时的回答"))
		f := flow{t: t, model: model}
		r := runCove(t, nil, nil, "-p", "问一句")
		if r.code != 0 || strings.TrimSpace(r.stdout) != "未信任时的回答" {
			f.Fatalf("cove -p: code %d stdout %q stderr:\n%s", r.code, r.stdout, r.stderr)
		}
		if !strings.Contains(r.stderr, "需要信任后才生效") || !strings.Contains(r.stderr, "system_prompt") {
			f.Fatalf("stderr lacks the untrusted-field notice:\n%s", r.stderr)
		}
		if strings.Contains(systemText(f.Requests(1)[0]), "F5-TRUST-MARKER") {
			f.Fatalf("the untrusted system_prompt reached the model")
		}
		if trusted("f5-proj-trust") {
			f.Fatalf("the project is in the trust store before /trust: %v", trustStore())
		}
	})

	t.Run("/trust后/restart生效", func(t *testing.T) {
		t.Chdir(trustDir)
		s, f := start(t, textReply("信任之前的回答，项目提示词还没生效。"))
		mark := s.Mark()
		s.Type("/trust")
		s.WaitForSince(mark, "输入 /restart 让 system_prompt 生效", e2eTimeout)
		s.Type("信任之后重启之前问一句")
		s.WaitFor("项目提示词还没生效", e2eTimeout)
		s.WaitIdle(e2eTimeout)
		if strings.Contains(systemText(f.Requests(1)[0]), "F5-TRUST-MARKER") {
			f.Fatalf("the system_prompt applied before /restart")
		}
		if !trusted(filepath.Join("f5-proj-trust", ".cove.json")) {
			f.Fatalf("/trust did not record the .cove.json: %v", trustStore())
		}
		s.Type("/restart")
		s.WaitExited(e2eTimeout)
		if !s.Restarted() {
			f.Fatalf("/restart did not ask for a restart")
		}
		s.Exit()

		s2, f2 := start(t, textReply("重启以后项目提示词生效了，回答完毕。"))
		s2.Type("重启之后问一句")
		s2.WaitFor("项目提示词生效了", e2eTimeout)
		s2.WaitIdle(e2eTimeout)
		if !strings.Contains(systemText(f2.Requests(1)[0]), "F5-TRUST-MARKER") {
			f2.Fatalf("the trusted system_prompt is not in the request after the restart")
		}
		// And the start-up notice is gone.
		model.Reset(textReply("信任后的一次性回答"))
		r := runCove(t, nil, nil, "-p", "再问一句")
		if r.code != 0 || strings.Contains(r.stderr, "需要信任后才生效") {
			f2.Fatalf("after /trust: code %d stderr:\n%s", r.code, r.stderr)
		}
	})

	// ---------- /cd ----------

	t.Run("/cd到另一项目后policies.json按新项目重载", func(t *testing.T) {
		other := newProject(t, "f5-proj-cd", "")
		rules, _ := json.Marshal([]map[string]any{{
			"id": "allow-mkdir-a", "tool_pattern": "bash", "action": "allow", "enabled": true,
			"command_prefix": "mkdir", "scope": project,
		}})
		writeTestFile(t, filepath.Join(cfgDir, "policies.json"), string(rules))
		t.Cleanup(func() { _ = os.Remove(filepath.Join(cfgDir, "policies.json")) })
		s, f := start(t,
			toolReply(bashCall("mkdir f5_cd_a")),
			textReply("第一个项目里的目录 f5_cd_a 按已有的允许规则直接建好了，没有询问，也没有其他改动。"),
			toolReply(bashCall("mkdir f5_cd_b")),
			textReply("第二个项目里没有这条允许规则，建目录的请求被拒绝了，所以没有建 f5_cd_b。"))
		s.Type("在这里建目录 f5_cd_a")
		s.WaitFor("按已有的允许规则直接建好了", e2eTimeout)
		s.WaitIdle(e2eTimeout)
		if strings.Contains(s.Output(), "需要授权") {
			f.Fatalf("the project rule did not apply before /cd")
		}
		mark := s.Mark()
		s.Type("/cd " + other)
		s.WaitForSince(mark, "已切换到: ", e2eTimeout)
		s.Type("在这里建目录 f5_cd_b")
		s.WaitForSince(mark, "需要授权", e2eTimeout)
		s.Type("n")
		s.WaitFor("所以没有建 f5_cd_b", e2eTimeout)
		s.WaitIdle(e2eTimeout)
		reqs := f.Requests(4)
		if got := lastMessage(reqs[3]); got.Role != "tool" || !strings.Contains(got.Text(), "user rejected") {
			f.Fatalf("the model did not get the refusal: %s %q", got.Role, got.Text())
		}
		if _, err := os.Stat(filepath.Join(project, "f5_cd_a")); err != nil {
			f.Fatalf("f5_cd_a not created in the first project: %v", err)
		}
		if _, err := os.Stat(filepath.Join(other, "f5_cd_b")); !os.IsNotExist(err) {
			f.Fatalf("f5_cd_b created although the rule belongs to the other project (stat err %v)", err)
		}
	})

	t.Run("/cd后新项目的.cove.json在/restart后生效", func(t *testing.T) {
		other := newProject(t, "f5-proj-cd-model", `{"model": "qwen-f5-cd"}`)
		s, f := start(t, textReply("切换目录后仍是启动时的模型在回答。"))
		mark := s.Mark()
		s.Type("/cd " + other)
		s.WaitForSince(mark, "已切换到: ", e2eTimeout)
		s.Type("切换目录之后问一句")
		s.WaitFor("仍是启动时的模型", e2eTimeout)
		s.WaitIdle(e2eTimeout)
		// The manual promises a reload of policies.json on /cd, not of the
		// config: .cove.json is read at start-up.
		if got := f.Requests(1)[0].Model; got != "qwen-test" {
			f.Fatalf("request model = %q right after /cd, want the start-up qwen-test", got)
		}
		s.Type("/restart")
		s.WaitExited(e2eTimeout)
		s.Exit()
		t.Chdir(other)
		s2, f2 := start(t, textReply("新目录的项目模型在回答。"))
		s2.Type("重启之后问一句")
		s2.WaitFor("新目录的项目模型", e2eTimeout)
		s2.WaitIdle(e2eTimeout)
		if got := f2.Requests(1)[0].Model; got != "qwen-f5-cd" {
			f2.Fatalf("request model = %q after the restart in the new directory, want qwen-f5-cd", got)
		}
		if b := readConfigFile(t, home)["model"]; str(b) != "qwen-test" {
			f2.Fatalf("the project model reached config.json: %s", b)
		}
	})

	t.Run("/cd后/trust不信任启动目录", func(t *testing.T) {
		// Started in a project with an untrusted .cove.json: after /cd,
		// /trust refuses rather than trusting the start-up file.
		startDir := newProject(t, "f5-proj-cd-from", `{"system_prompt": "F5-CD-FROM"}`)
		toDir := newProject(t, "f5-proj-cd-to", "")
		t.Chdir(startDir)
		s, f := start(t)
		mark := s.Mark()
		s.Type("/cd " + toDir)
		s.WaitForSince(mark, "已切换到: ", e2eTimeout)
		s.Type("/trust")
		s.WaitForSince(mark, "工作目录已切换", e2eTimeout)
		s.Exit()
		if trusted("f5-proj-cd-from") || trusted("f5-proj-cd-to") {
			f.Fatalf("/trust after /cd trusted something: %v", trustStore())
		}
		// Started where nothing is untrusted: /trust after /cd trusts the
		// directory it is in now.
		t.Chdir(project)
		s2, f2 := start(t)
		mark = s2.Mark()
		s2.Type("/cd " + toDir)
		s2.WaitForSince(mark, "已切换到: ", e2eTimeout)
		s2.Type("/trust")
		s2.WaitForSince(mark, "已信任此项目目录", e2eTimeout)
		if !strings.Contains(s2.OutputSince(mark), "f5-proj-cd-to") {
			f2.Fatalf("/trust named another directory than the new one")
		}
		store := trustStore()
		if !trusted("f5-proj-cd-to") {
			f2.Fatalf("the new directory is not trusted: %v", store)
		}
		for k := range store {
			if strings.EqualFold(strings.TrimPrefix(k, "dir:"), project) {
				f2.Fatalf("/trust after /cd trusted the start-up directory: %v", store)
			}
		}
		f2.Requests(0)
	})

	// ---------- a .cove.json that is not read ----------

	ignored := func(t *testing.T, dir string) {
		t.Helper()
		t.Chdir(dir)
		before := readFile(t, cfgPath)
		model.Reset(textReply("忽略项目配置后的回答"))
		f := flow{t: t, model: model}
		r := runCove(t, nil, nil, "-p", "问一句")
		if r.code != 0 || strings.TrimSpace(r.stdout) != "忽略项目配置后的回答" {
			f.Fatalf("cove -p: code %d stdout %q stderr:\n%s", r.code, r.stdout, r.stderr)
		}
		if !strings.Contains(r.stderr, "project config ignored") {
			f.Fatalf("stderr does not say the .cove.json was ignored:\n%s", r.stderr)
		}
		if got := f.Requests(1)[0].Model; got == "qwen-f5-ignored" {
			f.Fatalf("the ignored .cove.json's model was used")
		}
		if after := readFile(t, cfgPath); !bytes.Equal(before, after) {
			f.Fatalf("config.json changed")
		}
	}

	t.Run(".cove.json为符号链接时忽略并提示", func(t *testing.T) {
		dir := newProject(t, "f5-proj-link", "")
		target := filepath.Join(home, "f5-link-target.json")
		writeTestFile(t, target, `{"model": "qwen-f5-ignored"}`)
		if err := os.Symlink(target, filepath.Join(dir, ".cove.json")); err != nil {
			t.Skipf("cannot create a symlink here (unprivileged): %v", err)
		}
		ignored(t, dir)
	})

	t.Run(".cove.json超过1MiB时忽略并提示", func(t *testing.T) {
		pad := strings.Repeat("x", 1<<20)
		dir := newProject(t, "f5-proj-big", `{"model": "qwen-f5-ignored", "pad": "`+pad+`"}`)
		ignored(t, dir)
	})
}
