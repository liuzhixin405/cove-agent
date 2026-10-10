package main

// A local web page for business-flow testing: pick a scenario, type (or
// draw) an input, run it against the real REPL with the fake model, and see
// the terminal transcript next to each check's verdict. It reuses the e2e
// harness, so a scenario here is the same thing the e2e tests run; what it
// adds is a screen a person can look at and an input they can vary.
//
//	COVE_TESTPAGE=1 go test ./cli/cove -run TestPage -timeout 0

import (
	"encoding/json"
	"fmt"
	"html"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type tpCheck struct {
	Name   string `json:"name"`
	Pass   bool   `json:"pass"`
	Detail string `json:"detail"`
}

type tpScenario struct {
	ID, Name, Desc string
	// DefaultInput fills the input box; Inputs is the pool "随机" draws from.
	DefaultInput string
	Inputs       []string
	Run          func(t *testing.T, input string) (transcript string, checks []tpCheck)
}

type tpResult struct {
	Scenario   string    `json:"scenario"`
	Input      string    `json:"input"`
	Transcript string    `json:"transcript_html"`
	Checks     []tpCheck `json:"checks"`
	Passed     int       `json:"passed"`
	Total      int       `json:"total"`
	Seconds    float64   `json:"seconds"`
	At         string    `json:"at"`
}

// waitOut is WaitFor without the Fatal: the page reports, it does not abort.
func waitOut(s *e2eSession, want string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(s.Output(), want) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func tpContains(name, out, want string) tpCheck {
	return tpCheck{Name: name, Pass: strings.Contains(out, want), Detail: "期望输出包含：" + want}
}

func tpNotContains(name, out, unwanted string) tpCheck {
	return tpCheck{Name: name, Pass: !strings.Contains(out, unwanted), Detail: "期望输出不包含：" + unwanted}
}

func tpFile(name, path, want string) tpCheck {
	data, err := os.ReadFile(path)
	if err != nil {
		return tpCheck{Name: name, Pass: want == "", Detail: "文件不存在：" + filepath.Base(path)}
	}
	return tpCheck{Name: name, Pass: string(data) == want, Detail: fmt.Sprintf("文件内容 %q，期望 %q", string(data), want)}
}

func tpModelSaw(name string, model *fakeModel, sub string) tpCheck {
	for _, req := range model.Requests() {
		for _, m := range req.Messages {
			if strings.Contains(m.Text(), sub) {
				return tpCheck{Name: name, Pass: true, Detail: "模型收到了：" + sub}
			}
		}
	}
	return tpCheck{Name: name, Pass: false, Detail: "模型没有收到：" + sub}
}

const tpWait = 15 * time.Second

var tpScenarios = []tpScenario{
	{
		ID: "setup-wizard", Name: "首次配置向导", Desc: "没有 API key 时启动，选供应商、掩码输入 key、自动验证并保存。输入即 key。",
		DefaultInput: "sk-test-123", Inputs: []string{"sk-test-123", "sk-" + strings.Repeat("x", 48), "key with spaces", "密钥中文", ""},
		Run: func(t *testing.T, in string) (string, []tpCheck) {
			model := newFakeModel(t, fakeStep{Content: "ok"})
			_, _ = e2eHomeWithoutKey(t, model)
			s := startREPL(t)
			var cs []tpCheck
			cs = append(cs, tpCheck{Name: "启动进入向导", Pass: waitOut(s, "选择模型供应商", tpWait)})
			s.Type("10")
			cs = append(cs, tpCheck{Name: "进入 key 输入", Pass: waitOut(s, "API key", tpWait)})
			s.Type(in)
			if strings.TrimSpace(in) == "" {
				// Enter alone leaves the wizard; nothing may be saved.
				cs = append(cs, tpCheck{Name: "空 key 跳过向导", Pass: waitOut(s, "已跳过配置", tpWait)})
				data, _ := os.ReadFile(filepath.Join(os.Getenv("COVE_CONFIG_DIR"), "config.json"))
				cs = append(cs, tpCheck{Name: "config.json 仍无 key", Pass: strings.Contains(string(data), `"api_key": ""`), Detail: "读取配置文件"})
				out := s.Output()
				s.Exit()
				return out, cs
			}
			saved := waitOut(s, "已保存到", tpWait)
			cs = append(cs, tpCheck{Name: "验证通过并保存", Pass: saved})
			out := s.Output()
			cs = append(cs, tpNotContains("key 不回显", out, in))
			data, _ := os.ReadFile(filepath.Join(os.Getenv("COVE_CONFIG_DIR"), "config.json"))
			cs = append(cs, tpCheck{Name: "config.json 含 key", Pass: strings.Contains(string(data), in), Detail: "读取配置文件"})
			s.Exit()
			return out, cs
		},
	},
	{
		ID: "deny-reason", Name: "授权拒绝并说明理由", Desc: "模型要跑 bash，按 e 拒绝并输入理由，理由应回给模型且命令不执行。输入即理由。",
		DefaultInput: "用 switch 别用 checkout", Inputs: []string{"用 switch 别用 checkout", "don't touch git", "先写测试再改", "理由里有\x1b[2J控制符", ""},
		Run: func(t *testing.T, in string) (string, []tpCheck) {
			model := newFakeModel(t,
				fakeStep{ToolCalls: []fakeToolCall{{Name: "bash", Args: `{"command":"mkdir denied_dir"}`}}},
				fakeStep{Content: "明白，换个做法"},
			)
			_, project := e2eHome(t, model)
			s := startREPL(t)
			s.Type("建一个目录")
			var cs []tpCheck
			cs = append(cs, tpCheck{Name: "弹出授权提示", Pass: waitOut(s, "需要授权", tpWait)})
			cs = append(cs, tpContains("提示含 [e] 选项", s.Output(), "[e] 拒绝并说明"))
			s.Type("e")
			cs = append(cs, tpCheck{Name: "询问理由", Pass: waitOut(s, "拒绝理由", tpWait)})
			s.Type(in)
			cs = append(cs, tpCheck{Name: "模型继续回答", Pass: waitOut(s, "换个做法", tpWait)})
			out := s.Output()
			if strings.TrimSpace(in) == "" {
				// Enter alone on the reason line is a plain denial.
				cs = append(cs, tpContains("空理由按普通拒绝", out, "已拒绝 bash"))
				cs = append(cs, tpModelSaw("模型收到 user rejected（无理由）", model, "(user rejected)."))
			} else {
				cs = append(cs, tpContains("本地回显理由已转给模型", out, "理由已转给模型"))
				cs = append(cs, tpModelSaw("模型收到 user rejected 与理由", model, "user rejected: "+strings.TrimSpace(strings.ReplaceAll(in, "\x1b[2J", ""))))
			}
			_, err := os.Stat(filepath.Join(project, "denied_dir"))
			cs = append(cs, tpCheck{Name: "命令没有执行", Pass: os.IsNotExist(err), Detail: "目录不应存在"})
			s.Exit()
			return out, cs
		},
	},
	{
		ID: "plan-exit", Name: "退出计划模式的审批框", Desc: "模型进入计划模式再请求退出，框里应渲染计划摘要。输入 y 执行、n 继续规划。",
		DefaultInput: "y", Inputs: []string{"y", "n"},
		Run: func(t *testing.T, in string) (string, []tpCheck) {
			model := newFakeModel(t,
				fakeStep{ToolCalls: []fakeToolCall{{Name: "plan_mode", Args: `{"reason":"先想清楚"}`}}},
				fakeStep{ToolCalls: []fakeToolCall{{Name: "exit_plan_mode", Args: `{"summary":"## 计划\n1. 先写测试\n2. 再改实现"}`}}},
				fakeStep{Content: "按计划开始"},
			)
			e2eHome(t, model)
			s := startREPL(t)
			s.Type("改一下权限模块")
			var cs []tpCheck
			cs = append(cs, tpCheck{Name: "弹出计划审批框", Pass: waitOut(s, "退出计划模式", tpWait)})
			out := s.Output()
			cs = append(cs, tpContains("渲染了计划摘要", out, "再改实现"))
			cs = append(cs, tpContains("选项为执行/修改/继续规划", out, "[y] 执行计划"))
			cs = append(cs, tpNotContains("不显示英文 reason", out, "exiting plan mode requires confirmation"))
			s.Type(in)
			if strings.EqualFold(strings.TrimSpace(in), "y") {
				cs = append(cs, tpCheck{Name: "y 后模型继续", Pass: waitOut(s, "按计划开始", tpWait)})
			} else {
				cs = append(cs, tpCheck{Name: "n 后继续规划", Pass: waitOut(s, "继续规划", tpWait)})
			}
			out = s.Output()
			s.Exit()
			return out, cs
		},
	},
	{
		ID: "undo-confirm", Name: "/undo 先预览再确认", Desc: "模型写了一个文件后执行 /undo：先弹确认框，y 回退、n 保留。输入 y 或 n。",
		DefaultInput: "n", Inputs: []string{"y", "n", "随便打点别的"},
		Run: func(t *testing.T, in string) (string, []tpCheck) {
			model := newFakeModel(t,
				fakeStep{ToolCalls: []fakeToolCall{{Name: "write", Args: `{"file_path":"undo_me.txt","content":"v1"}`}}},
				fakeStep{Content: "写好了"},
			)
			_, project := e2eHome(t, model)
			s := startREPL(t)
			s.Type("写个文件")
			var cs []tpCheck
			if waitOut(s, "需要授权", tpWait) {
				s.Type("y")
			}
			cs = append(cs, tpCheck{Name: "文件写入完成", Pass: waitOut(s, "写好了", tpWait)})
			// The answer streams before the task runner settles; /undo is
			// refused while a task runs, so wait for idle first.
			s.WaitIdle(tpWait)
			path := filepath.Join(project, "undo_me.txt")
			cs = append(cs, tpFile("写入后内容为 v1", path, "v1"))
			s.Type("/undo")
			cs = append(cs, tpCheck{Name: "弹出确认框", Pass: waitOut(s, "整树回退到检查点", tpWait)})
			s.Type(in)
			switch strings.ToLower(strings.TrimSpace(in)) {
			case "y":
				cs = append(cs, tpCheck{Name: "y 后回退", Pass: waitOut(s, "已回退", tpWait)})
				cs = append(cs, tpFile("文件被回退（不存在）", path, ""))
			default:
				cs = append(cs, tpCheck{Name: "非 y 取消", Pass: waitOut(s, "已取消", tpWait)})
				cs = append(cs, tpFile("文件保留 v1", path, "v1"))
			}
			out := s.Output()
			s.Exit()
			return out, cs
		},
	},
	{
		ID: "help-command", Name: "/help <命令>", Desc: "查看单个命令的用法。输入即命令名（别名也可）。",
		DefaultInput: "race", Inputs: []string{"race", "cls", "/undo", "automations", "不存在的命令"},
		Run: func(t *testing.T, in string) (string, []tpCheck) {
			model := newFakeModel(t)
			e2eHome(t, model)
			s := startREPL(t)
			s.Type("/help " + in)
			name := strings.TrimPrefix(strings.TrimSpace(in), "/")
			var cs []tpCheck
			if name == "不存在的命令" {
				cs = append(cs, tpCheck{Name: "未知命令给出提示", Pass: waitOut(s, "未找到命令", tpWait)})
			} else {
				cs = append(cs, tpCheck{Name: "显示命令用法", Pass: waitOut(s, "/", tpWait) && waitOut(s, "/"+name, tpWait) || waitOut(s, "别名", tpWait)})
				cs = append(cs, tpNotContains("不是全量帮助", s.Output(), "插件命令:"))
			}
			time.Sleep(200 * time.Millisecond)
			out := s.Output()
			s.Exit()
			return out, cs
		},
	},
}

// ansiToHTML keeps bold/dim/colour SGR codes as spans, drops other control
// sequences and escapes the text.
var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\a]*\a|\x1b.`)

func ansiToHTML(s string) string {
	var sb strings.Builder
	open := 0
	last := 0
	for _, loc := range ansiRe.FindAllStringIndex(s, -1) {
		sb.WriteString(html.EscapeString(s[last:loc[0]]))
		seq := s[loc[0]:loc[1]]
		last = loc[1]
		if !strings.HasSuffix(seq, "m") || !strings.HasPrefix(seq, "\x1b[") {
			continue
		}
		for _, code := range strings.Split(strings.TrimSuffix(strings.TrimPrefix(seq, "\x1b["), "m"), ";") {
			switch code {
			case "", "0":
				for ; open > 0; open-- {
					sb.WriteString("</span>")
				}
			case "1":
				sb.WriteString(`<span class="b">`)
				open++
			case "2":
				sb.WriteString(`<span class="d">`)
				open++
			default:
				if n, err := fmt.Sscanf(code, "%d", new(int)); err == nil && n == 1 {
					sb.WriteString(`<span class="c` + code + `">`)
					open++
				}
			}
		}
	}
	sb.WriteString(html.EscapeString(s[last:]))
	for ; open > 0; open-- {
		sb.WriteString("</span>")
	}
	return strings.ReplaceAll(sb.String(), "\r\n", "\n")
}

const tpPage = `<!doctype html><html lang="zh"><head><meta charset="utf-8"><title>cove 业务流测试台</title>
<style>
body{font:14px/1.5 system-ui,sans-serif;margin:0;display:grid;grid-template-columns:260px 1fr;height:100vh;color:#222}
aside{border-right:1px solid #ddd;padding:12px;overflow:auto;background:#fafafa}
aside h1{font-size:16px;margin:0 0 8px}
.sc{padding:8px;border-radius:6px;cursor:pointer;margin-bottom:4px}.sc:hover{background:#eee}.sc.on{background:#dfe9ff}
.sc small{display:block;color:#666}
main{padding:12px 16px;overflow:auto;display:grid;grid-template-rows:auto auto 1fr;gap:12px}
.bar{display:flex;gap:8px;align-items:center;flex-wrap:wrap}
input{flex:1;min-width:240px;padding:6px 8px;font:inherit}
button{padding:6px 14px;font:inherit;cursor:pointer}
.score{font-weight:600}.score.ok{color:#1a7f37}.score.bad{color:#b42318}
.cols{display:grid;grid-template-columns:1fr 360px;gap:12px;min-height:0}
pre{background:#111;color:#ddd;padding:12px;border-radius:6px;overflow:auto;margin:0;font:12.5px/1.45 Consolas,monospace;white-space:pre-wrap}
.b{font-weight:700}.d{opacity:.6}.c31,.c91{color:#f66}.c32,.c92{color:#6c6}.c33,.c93{color:#dc6}.c34,.c94{color:#69f}.c35,.c95{color:#c6c}.c36,.c96{color:#6cc}.c90{color:#888}
table{border-collapse:collapse;width:100%;font-size:13px}td{padding:6px 8px;border-bottom:1px solid #eee;vertical-align:top}
td.p{width:28px;text-align:center}.pass{color:#1a7f37}.fail{color:#b42318}
#hist div{padding:4px 0;border-bottom:1px dashed #eee;font-size:12px;color:#555}
</style></head><body>
<aside><h1>业务流测试台</h1><div id="list"></div><h1 style="margin-top:16px">历史</h1><div id="hist"></div></aside>
<main>
<div><h2 id="title" style="margin:0 0 4px"></h2><div id="desc" style="color:#555"></div></div>
<div class="bar"><input id="input" placeholder="输入"><button id="run">运行</button><button id="rand">随机输入</button><span id="score" class="score"></span><span id="status" style="color:#666"></span></div>
<div class="cols"><pre id="out">（选择左侧场景，输入后点运行）</pre><div><table id="checks"></table></div></div>
</main>
<script>
let scs=[],cur=null,hist=[];
async function load(){scs=await (await fetch('/api/scenarios')).json();const l=document.getElementById('list');l.innerHTML='';
scs.forEach((s,i)=>{const d=document.createElement('div');d.className='sc';d.innerHTML='<b>'+s.name+'</b><small>'+s.id+'</small>';d.onclick=()=>pick(i);l.appendChild(d)});pick(0)}
function pick(i){cur=scs[i];document.querySelectorAll('.sc').forEach((e,j)=>e.classList.toggle('on',i===j));
document.getElementById('title').textContent=cur.name;document.getElementById('desc').textContent=cur.desc;document.getElementById('input').value=cur.default_input}
async function run(random){if(!cur)return;const st=document.getElementById('status');st.textContent='运行中…';document.getElementById('run').disabled=true;
try{const r=await fetch('/api/run',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({scenario:cur.id,input:document.getElementById('input').value,random})});
const j=await r.json();if(random)document.getElementById('input').value=j.input;show(j);hist.unshift(j);renderHist()}catch(e){st.textContent='失败：'+e}
document.getElementById('run').disabled=false}
function show(j){document.getElementById('out').innerHTML=j.transcript_html||'（无输出）';const sc=document.getElementById('score');sc.textContent=j.passed+' / '+j.total+'（'+Math.round(100*j.passed/Math.max(1,j.total))+'%）';sc.className='score '+(j.passed===j.total?'ok':'bad');
document.getElementById('status').textContent=j.seconds.toFixed(1)+' 秒 · 输入：'+j.input;
document.getElementById('checks').innerHTML=j.checks.map(c=>'<tr><td class="p '+(c.pass?'pass':'fail')+'">'+(c.pass?'✓':'✗')+'</td><td>'+esc(c.name)+'<br><small style="color:#777">'+esc(c.detail||'')+'</small></td></tr>').join('')}
function renderHist(){document.getElementById('hist').innerHTML=hist.slice(0,30).map((j,i)=>'<div style="cursor:pointer" onclick="show(hist['+i+'])">'+j.at+' '+esc(j.scenario)+' '+j.passed+'/'+j.total+'</div>').join('')}
function esc(s){return String(s).replace(/[&<>]/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;'}[c]))}
document.getElementById('run').onclick=()=>run(false);document.getElementById('rand').onclick=()=>run(true);load();
</script></body></html>`

// TestPage serves the test page until the process is stopped. It only runs
// when asked for: COVE_TESTPAGE=1 go test ./cli/cove -run TestPage -timeout 0
func TestPage(t *testing.T) {
	if os.Getenv("COVE_TESTPAGE") == "" {
		t.Skip("set COVE_TESTPAGE=1 to serve the test page")
	}
	var mu sync.Mutex // scenarios share HOME, cwd and the terminal: one at a time
	byID := map[string]tpScenario{}
	for _, sc := range tpScenarios {
		byID[sc.ID] = sc
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(tpPage))
	})
	mux.HandleFunc("/api/scenarios", func(w http.ResponseWriter, r *http.Request) {
		type item struct {
			ID           string `json:"id"`
			Name         string `json:"name"`
			Desc         string `json:"desc"`
			DefaultInput string `json:"default_input"`
		}
		var items []item
		for _, sc := range tpScenarios {
			items = append(items, item{sc.ID, sc.Name, sc.Desc, sc.DefaultInput})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(items)
	})
	mux.HandleFunc("/api/run", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Scenario string `json:"scenario"`
			Input    string `json:"input"`
			Random   bool   `json:"random"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		sc, ok := byID[req.Scenario]
		if !ok {
			http.Error(w, "unknown scenario", 404)
			return
		}
		input := req.Input
		if req.Random && len(sc.Inputs) > 0 {
			input = sc.Inputs[rand.IntN(len(sc.Inputs))]
		}
		mu.Lock()
		defer mu.Unlock()
		start := time.Now()
		res := tpResult{Scenario: sc.ID, Input: input, At: start.Format("15:04:05")}
		t.Run(sc.ID+"/"+start.Format("150405"), func(t *testing.T) {
			transcript, checks := sc.Run(t, input)
			res.Transcript = ansiToHTML(transcript)
			res.Checks = checks
		})
		for _, c := range res.Checks {
			if c.Pass {
				res.Passed++
			}
		}
		res.Total = len(res.Checks)
		res.Seconds = time.Since(start).Seconds()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(res)
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	url := "http://" + ln.Addr().String()
	fmt.Fprintln(os.Stderr, "测试台: "+url)
	if os.Getenv("COVE_TESTPAGE_NO_OPEN") == "" {
		switch runtime.GOOS {
		case "windows":
			_ = exec.Command("cmd", "/c", "start", "", url).Start()
		case "darwin":
			_ = exec.Command("open", url).Start()
		default:
			_ = exec.Command("xdg-open", url).Start()
		}
	}
	_ = http.Serve(ln, mux)
}

// ansiToHTML is unit-tested here so the page's transcript stays readable.
func TestAnsiToHTML(t *testing.T) {
	got := ansiToHTML("\x1b[1m粗\x1b[0m <x> \x1b[2m暗\x1b[0m\x1b[K")
	want := `<span class="b">粗</span> &lt;x&gt; <span class="d">暗</span>`
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
