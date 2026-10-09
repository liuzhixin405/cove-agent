# 显式工作流（首版）

所有功能默认关闭。命令注册于交互 REPL 与 headless 的公共前端；远程监督仅支持活跃交互 REPL。模型执行可能产生 API 费用，测试命令和项目代码必须可信。结果存放在配置目录，不自动写入原项目。

## 维护与结果收件箱

维护任务需要 Git 项目根目录与已提交的 HEAD。每次尝试使用新建的 detached worktree；只以 committed HEAD 为输入，未提交、暂存和 untracked 文件不参与。任务和验证共享超时，预算在有限重试间分配；模型请求进行中可能超出估算预算。

维护 spec（例如放在项目外的 `maintenance.json`）：

```json
{"version":1,"spec":{"id":"maintenance","prompt":"Fix one small failing test","enabled":true,"every_seconds":3600,"event":"tests-failed","timeout_seconds":300,"budget_usd":1,"max_turns":8,"retries":1,"verify":[["go","test","./internal/automation"]]}}
```

```text
/automations add C:\Temp\maintenance.json
/automations run maintenance
/automations tick
/automations event tests-failed build-42
/inbox list
/inbox show <result-id>
/inbox patch <result-id> C:\Temp\result.patch
/inbox review <result-id> accepted
```

添加不会启动 worker。`add <文件> session` 将任务绑定当前会话，独立扫描会跳过会话绑定任务。定时扫描仅处理到期任务一次，需 OS 调度器显式调用：

```text
cove --automation tick D:\project
cove --automation event D:\project tests-failed build-42
cove --automation-inbox list D:\project
```

独立入口开关必须位于第一个参数。事件按项目、任务与 occurrence key 去重，失败或崩溃也不能自动重放该 key。过期租约记为 uncertain，审阅前阻止继续运行。收件箱保留基线、命令 argv、退出码、输出、补丁和审阅状态；accepted 只记录决定，不应用、合并或推送。补丁导出只能创建项目外的新文件。

## 浏览器实操验收

需要 Chrome/Chromium 和带 `chromedp` 标签的构建：

```text
go build -tags chromedp -o cove.exe ./cli/cove
/browser-verify C:\Temp\workflow.json --allow-local
/browser-verify results
/browser-verify artifacts <run-id>
/acceptance
```

无标签或 Chrome 不可用时，状态为 unverified，不会假绿。本地服务要由操作员先启动，并显式允许 loopback（`localhost` 会依次尝试 `127.0.0.1` 与 `::1`）；JSON 不能自行开放内网访问。子资源域名解析失败记为 `network_unavailable`，安全拒绝与无效工作流记为 unverified，都不算产品验收失败。

```json
{"url":"http://127.0.0.1:3000","fixtures":{"reference":"fixture-42"},"steps":[{"action":"fill","selector":"#name","fixture":"reference"},{"action":"click","selector":"#submit"},{"action":"assert_text","selector":"#result","text":"Confirmed fixture-42"},{"action":"assert_visible","selector":"#result"}]}
```

支持 navigate、click、fill、assert_text、assert_visible、assert_url，必须有明确断言。桌面 1280×800 与移动 390×844 独立重放，保留 PNG、尺寸、SHA256 和机器状态。错误断言为 fail，取消或安全拒绝为 unverified。验收按当前项目/会话持久化，之后的工具写入、回滚与竞跑应用会作废旧通过证据。

fixtures 只用非敏感测试值，不接受密码输入，敏感页面截图拒绝保存。结构化报告不记录 fixture 值、URL 或 DOM 文本；截图仍可能包含页面可见内容，只在可信测试页面运行。

## 双方案竞跑

需要项目根目录处于本地 Git 分支且完全干净，包括无 untracked/ignored 文件；无 Git 项目直接拒绝。spec 文件应存放在项目外：

```json
{"version":1,"prompts":["Implement solution A","Implement alternative B"],"verify":[["go","test","./internal/example"]],"total_budget_usd":2,"candidate_budget_usd":1,"total_timeout_seconds":300,"candidate_timeout_seconds":240,"verify_timeout_seconds":60}
```

```text
/race run C:\Temp\race.json
/race show <run-id>
/race cancel <run-id>
/race select <run-id> a
```

run 立即返回 ID，两个独立 worktree 使用同一显式 argv 验证器；模型自评不能决定通过。候选子进程继承当前 profile 的 provider/model，只覆盖预算、权限模式与迭代上限。捕获补丁前会清掉 worktree 里被 gitignore 的文件（构建缓存、`node_modules` 等），它们不进入补丁，验证器也看不到；验证器需要的依赖必须能在干净检出上自行准备。报告包括耗时、验证结果、补丁字节与哈希；没有结构化成本证据则 cost 为 null/unverified。仅通过候选可 select（总超时或取消的 run 里已完整通过验证的候选也可以），应用前检查项目、分支、commit、干净状态、验证器一致性与补丁哈希；应用后旧验收失效。退出会取消 worker 并有界清理 worktree。状态复核不是针对外部编辑器的原子 CAS。

## 跨设备监督与审批

```text
/remote start
/remote status
/remote stop
```

默认只监听随机 loopback 端口，显示 URL 和私有凭据文件路径，不显示 token。所有 HTTP 端点需要 Bearer 认证，严格验证 Host/Origin，不支持 URL token、任意文件或 shell 接口。手机访问需操作员配置可信 HTTPS 隧道与 `--public-origin`；直接私有 LAN 访问需显式 `--allow-lan` 和 TLS 证书。不会自动部署隧道或开放公网。

远程可查看任务摘要、排队和引导状态，发送 steer/cancel/pause，以及当前待审批工具的 approve/deny。pause 只暂停队列，cancel 不撤销已发生的副作用。本地审批仍可用，远程批准不保存规则、不改变权限模式。

请求 ID 防重放，scope 绑定会话、项目、任务与状态版本。审批额外绑定工具、实际输入摘要、有效期和单次消费；本地答案、取消、状态漂移、停服与退出都撤销 pending。远程 cancel、停服与退出会拒绝本地待答的授权提示；状态漂移（本地插入指引、队列变化）与远程有效期到期只撤销远程审批，本地提示继续按自己的超时等待。HTTP 线程仅入队，真实 REPL 所在线程复核并执行，不需要按 Enter 才处理。

API 与不把 token 放进参数/聊天记录的 PowerShell 客户端例子见 [远程协议说明](../../internal/remote/README.md)。当前首版是认证 API，不包含手机原生监督界面；已有 gomobile 聊天 Provider 不等于此活跃 CLI 服务。

## 安全边界

Git worktree 不是 OS、网络或进程沙箱。worker 可访问宿主环境，Git 管理数据仍共享；只运行可信项目、提示与命令。输出、补丁和截图可能包含敏感内容，避免同步配置/证据目录。临时模型配置包含凭据，正常退出会删除；宿主崩溃可能遗留配置或 worktree，需人工检查清理。预算是模型费用估算上限，不是供应商账单保证。
