# Cove 使用手册

> **cove** — Go 语言 AI 编程助手，单文件二进制，零依赖，终端即用。

## 目录

- [快速开始](#快速开始)
- [安装与启动](#安装与启动)
- [提供商与模型配置](#提供商与模型配置)
- [REPL 命令参考](#repl-命令参考)
- [Agent 工具参考](#agent-工具参考)
- [权限模式](#权限模式)
- [配置系统](#配置系统)
- [技能系统](#技能系统)
- [MCP 协议支持](#mcp-协议支持)
- [插件系统](#插件系统)
- [后台任务与异步执行](#后台任务与异步执行)
- [单轮上限与 /continue](#单轮上限与-continue)
- [完成判定与收尾](#完成判定与收尾)
- [计划执行器 (Plan Executor)](#计划执行器-plan-executor)
- [子智能体与团队协作](#子智能体与团队协作)
- [后台学习](#后台学习)
- [上下文与提示词](#上下文与提示词)
- [护栏与安全](#护栏与安全)
- [检查点与回退](#检查点与回退)
- [会话管理](#会话管理)
- [记忆系统](#记忆系统)
- [费用追踪](#费用追踪)
- [诊断系统](#诊断系统)
- [附件功能](#附件功能)
- [Git 集成](#git-集成)
- [CovePhone (Android)](#covephone-android)
- [高级技巧](#高级技巧)

---

## 快速开始

```bash
# 交互式 REPL
cove

# 单次查询
cove -p "定位并修复 pkg/engine 里偶发失败的测试，并补一个回归测试"

# 带附件查询
cove -p "分析这张图片" --image screenshot.png
cove -p "审查这个文件" --file config.json

# 查看版本
cove --version

# 系统诊断
cove --doctor

# 查看当前配置
cove --config

# 调试模式
cove -d
```

---

## 安装与启动

### 预编译二进制

前往 [Releases](https://github.com/liuzhixin405/cove-agent/releases) 下载对应平台的压缩包：

| 平台 | 文件 |
|------|------|
| Windows (amd64) | `cove-v*-windows-amd64.zip` |
| macOS (Intel) | `cove-v*-darwin-amd64.tar.gz` |
| macOS (Apple Silicon) | `cove-v*-darwin-arm64.tar.gz` |
| Linux (amd64) | `cove-v*-linux-amd64.tar.gz` |

解压运行：
```bash
# macOS / Linux
tar -xzf cove-v*-linux-amd64.tar.gz
./cove

# Windows (PowerShell)
Expand-Archive cove-v*-windows-amd64.zip -DestinationPath .
.\cove.exe
```

### 从源码构建

需要 Go 1.25+：
```bash
git clone https://github.com/liuzhixin405/cove-agent.git
cd cove-agent
go build -o cove ./cli/cove
./cove --version
```

### 启动参数

| 参数 | 说明 |
|------|------|
| `-p, --print <prompt>` | 单次查询模式，非交互（见下方说明） |
| `--image <path>` | 附加图片（可多次指定） |
| `--file <path>` | 附加文件（可多次指定） |
| `--max-turns <N>` | 仅与 `-p` 连用（单独使用报错，退出码 2）：本轮最多调用模型 N 次，覆盖配置 `max_iterations`（默认 200）；`0` 表示不限制；到达上限时 stderr 输出 `Error: 已达到单轮最大迭代次数 N（可用 --max-turns 调整）`、退出码 1（见[单轮上限](#单轮上限与-continue)） |
| `-v, --version` | 显示版本信息 |
| `-d, --debug` | 调试模式 |
| `--doctor` | 系统诊断（git、ripgrep、供应商与 API key 是否设置） |
| `--config` | 查看配置（不显示 API key，只显示 `api_key_set`） |
| `-r, --resume <id>` | 按 ID 恢复会话后启动；可与 `-p` 连用继续该会话。会话属于其他项目目录时给出提示；ID 不存在时报错退出 |
| `--list-sessions [all]` | 列出当前项目的会话；加 `all` 列出所有项目的会话 |
| `--dump-system-prompt` | 打印系统提示词 |
| `--no-auto` | 禁用后台自学习功能 |
| `--no-tui` | 使用 headless 模式（按行读 stdin，答案写 stdout，提示写 stderr）；任一轮失败（请求出错、取消、缺 API key、超预算、附件读不到）时以退出码 1 结束；收到 SIGTERM 时（含执行斜杠命令期间）保存会话后结束，不再读后续输入 |
| `--tui` | 即使 stdin/stdout 不是终端也强制使用交互界面 |
| `--profile <name>` | 使用指定 profile 启动 |
| `--record <dir>` | 录制本次会话的请求与响应到目录 |
| `--replay <dir>` | 用录制数据回放，不调用真实 API |
| `-h, --help` | 帮助信息 |

未知参数（例如拼错的 `--no-tiu`）和没有 `-p` 的多余文字会报错退出（退出码 2），不再被静默忽略。

**`-p` 单次查询说明：**

- 只有最终答案写到 stdout，提示、警告和错误都写到 stderr，因此 `cove -p "..." > out.txt` 只得到答案。
- 管道输入会附在提示后面一起发送：`cat app.log | cove -p "解释这段日志"`；只有管道输入时它就是提示本身。管道输入上限 8MB，超出部分截断并在 stderr 提示。
- 提示可以不加引号：`cove -p 解释 这段 代码` 等同于 `cove -p "解释 这段 代码"`。
- 退出码：`0` 成功，`1` 失败（API 错误、附件读取失败、到达迭代上限等），`2` 参数错误，`130` 被 Ctrl+C 中断。
- 单轮上限：`-p` 下迭代上限是**硬上限**（`--max-turns N` 优先，否则取 `max_iterations`，默认 200；`0` 不限制），到达即以退出码 1 结束，不会询问；时间上限默认不施加，只有配置（`config.json`、`.cove.json` 或 profile）里显式写了 `max_turn_minutes` 才生效；停滞检测只记日志。一次性运行结束后没有续跑命令，需要更多步数时加大 `--max-turns` 重新运行。详见[单轮上限](#单轮上限与-continue)。
- 回答输出后最多等待 20 秒让本轮的记忆提取完成再退出；`-p` 不做技能回顾（进程退出时回顾来不及完成，白白花一次调用）；一次 `-p` 只有 1 个回合，达不到记忆整理的 `min_turns`（默认 2），所以脚本批量调用 `-p` 不会每次都启动整理（见[记忆整理](#记忆整理-dream)）。
- 撞上迭代上限（或显式配置的时间上限）时，先向 stdout 打印一段不调用工具的收尾总结（已完成什么、还剩什么、建议下一步），再在 stderr 输出错误，退出码仍为 1（见[收尾总结](#收尾总结)）。
- `-p` 不注册 `question` 工具（没有人能回答）。
- 没有人能回答授权询问：凡是当前权限模式下需要询问的工具调用一律拒绝，不会卡住等待，模型会收到拒绝原因。注意 `auto` 模式**并不放行一切**：它只额外放行构建/测试命令和项目内的写入/编辑，git 写操作（`git commit`/`git push` 等）、包安装、网络请求、未知命令在 `-p` 下仍会被拒绝（见[权限模式](#权限模式)）。要让 `-p` 完全无人值守，二选一：
  - 用 `bypass` 模式（配置 `permission_mode`，或用一个设置了该模式的 `--profile`）；
  - 或先在交互模式中对需要的命令选 `[p] 永久允许`（或手工编辑 `policies.json`），把允许规则持久化到本项目，之后 `-p` 运行会直接命中这些规则。
- 注意 `/mode` 会写入配置文件，之后的交互会话也会沿用。

---

## 提供商与模型配置

### 支持的原生提供商

| 提供商 | 类型 | 环境变量 |
|--------|------|---------|
| **Anthropic** | 原生 | `ANTHROPIC_API_KEY` |
| **OpenAI** | 原生 | `OPENAI_API_KEY` |
| **DeepSeek** | 原生 | `DEEPSEEK_API_KEY` |

### 支持的兼容提供商 (OpenAI 兼容接口)

| 提供商 | 环境变量 |
|--------|---------|
| GLM (智谱) | `GLM_API_KEY` / `ZHIPU_API_KEY` |
| Kimi (月之暗面) | `KIMI_API_KEY` / `MOONSHOT_API_KEY` |
| Qwen (通义千问) | `QWEN_API_KEY` / `DASHSCOPE_API_KEY` |
| Doubao (豆包) | `DOUBAO_API_KEY` / `ARK_API_KEY` |
| OpenRouter | `OPENROUTER_API_KEY` |
| SiliconFlow (硅基流动) | `SILICONFLOW_API_KEY` |
| Groq | `GROQ_API_KEY` |
| Together | `TOGETHER_API_KEY` |
| Fireworks | `FIREWORKS_API_KEY` |
| xAI (Grok) | `XAI_API_KEY` |
| Mistral | `MISTRAL_API_KEY` |
| 自定义 | `LLM_API_KEY` + `LLM_BASE_URL` |

### 在 REPL 中切换

```
/provider deepseek        # 切换到 DeepSeek
/model deepseek-v4-pro    # 切换模型
/api-key sk-xxx           # 设置 API Key
/base-url https://...     # 设置自定义接口地址
```

### 模型切换策略（简单任务 vs 复杂任务）

系统支持在 `config.json` 中配置**两个模型**，根据任务复杂度**自动切换**，无需手动干预。

#### 配置方式

```json
{
  "model": "deepseek-v4-pro",        // ← 高级模型：用于复杂任务
  "model_fast": "deepseek-v4-flash", // ← 快速模型：用于简单任务
  "provider": {
    "name": "deepseek"
  }
}
```

| 配置字段 | 用途 | 推荐值 |
|---------|------|-------|
| `model` | **复杂任务模型**（高级、昂贵、能力强） | `deepseek-v4-pro`, `claude-sonnet-4-20250514`, `gpt-4o` |
| `model_fast` | **简单任务模型**（快速、便宜、够用） | `deepseek-flash`（旧名 `deepseek-v4-flash` 仍可用）, `gpt-4o-mini`, `claude-haiku-4-5` |

> 如果只配置 `model`，不配置 `model_fast`，则 `model_fast` 与 `model` 相同，即不做模型切换。
>
> 快速模型和高级模型使用同一套提示词和行为规则，不会因为走了快速模型就额外加限制。复杂任务进行中的简短跟进（如"继续"）会留在当前模型上；快速模型在一轮中连续失败时，该轮后续会自动改用高级模型。

#### 自动切换规则

系统对用户每条消息打分，**分数 ≥ 0.35** 时用高级模型（`model`），否则用快速模型（`model_fast`）：

| 信号 | 加分 | 说明 |
|------|------|------|
| 复杂度关键词：`refactor`、`architecture`、`design`、`migrate`、`rewrite`、`debug`、`optimize`、`performance`、`security audit`、`重构`、`架构`、`设计`、`迁移`、`重写`、`调试`、`优化`、`性能`、`安全审计` | +0.45 | 单独命中即可越过阈值 |
| 消息长度 | 最多 +0.20 | 按**字符数**计（不是字节：中文不会被放大 3 倍），1000 字符时满分；≥ 2000 字符直接用高级模型 |
| 消息里提到的文件路径（`*.go`、`*.py`、`*.json` 等） | 最多 +0.15 | 3 个及以上满分 |
| 近期快速模型失败率 | 最多 +0.10 | |
| 预算紧张 | 最多 −0.10 | 剩余预算越少越倾向快速模型 |
| 用户手动 `/model xxx` 指定 | — | 强制使用指定模型 |

例如：没有关键词时，一条 1000 字符以上、提到 3 个文件的消息（0.20 + 0.15 = 0.35）会走高级模型；原阈值 0.40 在没有关键词时实际上达不到。

**模型状态行**：配置了 `model_fast` 且与 `model` 不同时，交互模式每个回合开始显示一行淡色 `模型：<本轮实际使用的模型>`（经过"短追问沿用当前模型"修正后的结果）。`-p` 不显示。

#### 代码实现

- **配置层**: `internal/config/config.go` — `Config.Model` + `Config.ModelFast`
- **路由层**: `internal/api/router.go` — `ModelRouter` 使用策略链自动决策
- **引擎层**: `internal/engine/engine.go` — 每次用户消息前调用 `Route()` 获取目标模型

#### 默认值

未指定模型时，系统按提供商自动填充默认值：

```json
{
  "model": "deepseek-v4-pro",        // DeepSeek 提供商的默认高级模型
  "model_fast": "deepseek-v4-flash"  // 默认快速模型
}
```

#### 视觉模型自动切换

当检测到图片附件时，系统自动切换到支持视觉的模型：
| 提供商 | 视觉模型 |
|--------|---------|
| DeepSeek | `deepseek-v4-flash` |
| OpenAI | `gpt-4o` |
| Anthropic | `claude-sonnet-4-20250514` |

### 配置优先级（从低到高）

1. **环境变量** — `LLM_API_KEY`, `LLM_BASE_URL`, 各提供商专用变量
2. **用户配置** — `~/.cove/config.json`
3. **项目配置** — 当前目录下的 `.cove.json`（能执行命令或改变密钥去向的字段需先 `/trust`，见[项目配置的信任](#项目配置的信任)）
4. **profile** — `active_profile` 或 `--profile` 选中的配置档案。档案里设置了的字段，用 `/model`、`/provider`、`/api-key`、`/base-url`、`/mode`、`/budget save` 修改时写回该档案（以前写到顶层，重启后又被档案覆盖回去）

---

## REPL 命令参考

### 回答的 Markdown 渲染

交互式 REPL 中，模型回答边流式输出边渲染 Markdown：标题去掉 `#` 并加粗；`**粗体**` 加粗、`` `行内代码` `` 反色；围栏代码块（```` ``` ```` / `~~~`）画暗色边框（`┌─ go` … `└─`），代码行缩进并带竖线，块内不做任何格式化；`-`/`*`/`+` 列表符号显示为 `•`，`>` 引用显示为暗色竖线，有序列表保持原样。只有可能是标记开头的少量字符会暂缓到下一个分块再显示，普通文字立即输出。控制台不能显示 Unicode 时（可用 `COVE_TUI_ASCII=1` 强制）改用 `-`、`|`、`+-`。`-p` 与 headless 模式输出原始文本，不做渲染。

### 供应商与模型

| 命令 | 说明 |
|------|------|
| `/model <名称>` | 切换 AI 模型 |
| `/provider <名称>` | 切换提供商（anthropic/deepseek/openai/openai-compatible/glm/kimi/qwen/doubao/openrouter/siliconflow/groq/together/fireworks/xai/mistral） |
| `/api-key <密钥>` | 保存 API 密钥 |
| `/base-url <地址>` | 设置自定义接口地址 |
| `/mode <模式>` | 设置权限模式 |
| `/profile [list\|switch\|save\|delete\|show]` | 管理具名配置档案 |
| `/record [status\|start\|stop]` | 控制会话事件录制 |
| `/budget <金额\|auto\|off\|save>` | 设置**本会话**预算上限（$）：`<金额>` 与 `auto`（按历史用量自动调整）只改本会话，`off` 取消本会话上限，`save` 把当前会话预算写入 `config.json`（见[预算管理](#预算管理)） |
| `/cost` | 查看用量和费用 |
| `/ratelimit` | 查看 API 速率限制状态 |
| `/attach <文件...>` | 挂载图片或文件（支持 `list`/`remove`/`clear` 子命令） |
| `/config` | 查看完整配置 |

### 会话

| 命令 | 说明 |
|------|------|
| `/compact` | 立即压缩对话历史（强制摘要，至少 4 条消息即可），打印压缩前后的 token 数（见[上下文压缩](#上下文压缩)） |
| `/undo` | 回退到上一个检查点 |
| `/undo files <检查点> <文件>...` | 预览选中文件的回滚；带空格的文件名加引号 |
| `/undo apply <预览ID>` | 确认当前项目/会话的文件级回滚预览；发现文件漂移则拒绝 |
| `/undo cancel` | 放弃文件级回滚预览，不改文件 |
| `/checkpoints` | 列出所有检查点 |
| `/history` | 查看和恢复历史会话 |
| `/history detail <id>` | 查看某次会话详情 |
| `/history delete <编号\|id>` | 删除一个历史会话（当前会话不能删） |
| `/history clear` | 清空当前项目的历史会话：先显示将删除的数量，输入 `/history clear confirm` 才真正删除；`/history clear all confirm` 清空所有项目 |
| `/history clean` | 修复历史文件（补标题、标记注入消息）并备份，**不删除**任何会话 |
| `/resume [id]` | 恢复已保存的会话 |
| `/continue` | 从中断处继续上一轮（因上限停止、Ctrl+C、API 错误等中断后），已完成的工具步骤不会重做 |
| `/export` | 导出当前对话 |

### 记忆

| 命令 | 说明 |
|------|------|
| `/memory add <名称> <内容>` | 添加持久记忆 |
| `/memory list` | 列出所有记忆（显示“共 N 条”，每条附大小与首行摘要，并标注来源 `(项目)` / `(全局)` / `(指令文件)`） |
| `/memory search <关键词>` | 按关键词（BM25）检索记忆 |
| `/memory remove <名称>` | 删除一条记忆 |
| `/memory stats` | 记忆统计：条数、大小、使用率，以及“上次提取: 时间，保存 N 条” |
| `/memory source <名称>` | 查看当前正文版本的来源会话、项目、消息位置、输入哈希和依据片段 |

### 后台任务

| 命令 | 说明 |
|------|------|
| `/tasks` | 查看运行中/排队任务（TUI）；headless 显示同步执行状态 |
| `/tasks saved` | 列出当前项目持久化队列；仅交互式 REPL 支持恢复及队列管理 |
| `/acceptance` | 查看当前会话最新任务的验收证据；交互式与 headless 均可查看 |
| `/automations` | 添加、手动运行或扫描维护任务；事件显式触发并按 key 去重 |
| `/inbox` | 查看维护结果、验证日志和补丁；accepted/rejected 只记录审阅决定 |
| `/browser-verify` | 执行 DOM 场景，保存桌面/移动截图、哈希并接入会话验收 |
| `/race` | 双 worktree 竞跑，共用验证器；明确 select 后才应用补丁 |
| `/remote` | 在活跃交互 REPL 中启动认证远程监督与一次性审批 |
| `/clear` | 清屏并清空回滚区（别名 `/cls`，快捷键 Ctrl+L）；不影响对话上下文 |
| `/x [编号] [all]` | 展开工具块折叠的输出（编号是工具块标题后的 `#N`，省略则展开最近一个；默认最多 200 行，`all` 显示全部）；edit/write 展开后是改动的 diff。别名 `/expand` |
| `/keys` | 查看输入快捷键（别名 `/shortcuts`） |
| `/new` | 保存当前会话并开始新会话（清空对话上下文）；旧会话可在 `/history` 找回 |
| `/stop` 或 `/cancel` | 取消当前任务（TUI）；headless 无后台任务可取消 |

> 表中的“TUI”指默认的交互式 REPL（终端里直接运行 `cove`）。这一叫法沿用自早期的全屏界面，该界面已移除。

详细场景、JSON 示例及限制见[工作流指南](guide/workflows.md)。独立维护入口 `cove --automation tick <项目>` 与 `cove --automation-inbox list <项目>` 在普通启动参数解析前处理，必须作为第一个开关使用，不与普通启动参数混用。OS 调度器需显式定期调用 tick；程序不默认启动守护进程。维护只使用 committed HEAD，不包括未提交内容。收件箱接受不等于应用；远程暂停仅暂停排队任务；竞跑退出时停止并清理 worker。

### Git 集成

| 命令 | 说明 |
|------|------|
| `/commit [msg]` | Git add + commit |
| `/review` | 审查工作区变更 |
| `/diff` | 显示 git diff |

### 系统

| 命令 | 说明 |
|------|------|
| `/mcp` | MCP 服务器管理 |
| `/plugin` | 插件管理 |
| `/skills` | 列出可用技能 |
| `/skill <名称>` | 查看或调用一个技能（别名 `/skills`） |
| `/tools` | 列出可用工具 |
| `/doctor` | 快速检查：git、ripgrep、供应商与 API key，末尾附“后台学习”“权限规则文件”两项 |
| `/diagnose [quick\|errors\|archive\|codes\|trace N]` | 完整系统诊断与错误分析（含“后台学习”“权限规则文件”）；`trace` 查看最近 N 条交互轨迹 |
| `/status` | 查看代理状态与会话信息 |
| `/stats` | 查看消息数与费用统计 |
| `/permissions` | 查看当前权限模式 |
| `/init [apply\|discard]` | 让模型阅读仓库（两级目录、README、清单文件、已有 AGENTS.md）起草 CLAUDE.md，以 diff 展示草稿；`/init apply` 写入（不覆盖已有文件），`/init discard` 放弃。已有 AGENTS.md 时提示“已检测到 AGENTS.md，将同时加载” |
| `/cd <路径>` | 切换工作目录；`policies.json` 中的持久化规则会按新目录的项目根重新加载（旧项目的规则，包括 `[p]` 写入的规则，不再生效）；新目录的 `policies.json` 无法解析时给出警告并沿用切换前已加载的规则 |
| `/context` | 查看当前上下文；项目结构与代码大纲在首次使用时后台生成并缓存（最多等 5 秒，超时提示稍后再看） |
| `/system <提示词>` | 设置自定义系统提示词 |
| `/dream [status\|run]` | 无参数或 `status`：查看记忆整理（dream）的触发方式、上次结果与用量费用；`run`：忽略门槛立即在后台整理 |
| `/hooks` | 列出从用户级 `hooks.json` 加载的钩子（事件、匹配工具、同步/异步、超时、命令），以及未加载条目的原因 |
| `/help` | 显示帮助 |
| `/trust` | 信任当前项目：项目目录（回合结束自动运行构建/测试校验）以及它的 `.cove.json`（按本进程读到的内容记录哈希，`/restart` 后其中需要信任的设置生效，文件一改就失去信任）。`/cd` 之后先 `/restart` 再信任新目录。见[项目配置的信任](#项目配置的信任) |
| `/restart` | 保存会话并重启 cove，新进程用 `-r <会话 ID>` 接着当前会话（空会话则新开）。用于让新装的技能（`/skill install`）、插件、配置文件改动或升级后的二进制生效。退出部分与 `/exit` 相同（SessionEnd hook、MCP 断开）；启动参数沿用，但去掉 `-r`/`--resume` 和 `--image`/`--file`。任务运行中不能执行；headless 模式不支持。Windows 没有进程替换，首次重启后原进程留作监督进程等待新进程，之后的重启都由它发起，不会一层层叠加 |
| `/exit` | 退出 REPL |

---

## Agent 工具参考

Agent（AI）在对话中可以调用以下工具。每个工具有其权限要求（R=只读安全，W=可能需要确认）。

### 文件操作

| 工具 | 说明 | 权限 |
|------|------|------|
| `read` | 读取文件或目录内容 | R |
| `write` | 写入文件（创建或覆盖） | W |
| `edit` | 精确字符串替换编辑文件 | W |
| `glob` | 文件模式匹配查找 | R |
| `grep` | 正则表达式搜索文件内容 | R |
| `repo_map` | 按路径片段或标识符查询代码大纲（类型/函数/方法签名与行号），见[代码大纲按需查询](#代码大纲按需查询) | R |

### 终端执行

| 工具 | 说明 | 权限 |
|------|------|------|
| `bash` | 执行 shell 命令（Windows 上为 Git Bash，找不到时回退 PowerShell/cmd；其他系统为 bash/sh） | W |
| `powershell` | 执行 PowerShell 命令（**仅在 Windows 上注册**） | W |

> **文件工具要点**：
> - `read`：单行超过 2000 字符会截断并标注 `…[+N chars]`；因行数上限（默认 2000 行或 `limit`）截断时，结果末尾是 `... [showing lines A-B of T]` 和 `[next: offset=N]`，用 `offset=N` 继续读。结果再被引擎按输出上限截断时，`[next: offset=N]` 仍是最后一行，N 按实际保留的最后一行计算。
> - `edit`：精确匹配多处且未开 `replaceAll` 时，错误里列出所有命中行号（最多 20 个）；空白归一化的模糊匹配唯一命中时会把 newString 按文件实际缩进重排（tab/空格、缩进宽度），命中块自身缩进混乱且 newString 为多行时拒绝写入。缩进单位从**整个文件**推断（相邻行缩进增量的众数）：命中块内有两种及以上缩进深度时用块自身的单位；只有一种深度时，仅当整文件单位能把它映射到与 oldString 相同的层级数才用整文件单位，否则仍用块自身的猜测。成功消息附带改动区域及前后各 2 行（带行号）。
> - `grep`：参数 `ignore_case`（等同 `-i`）、`context`（0–10，等同 `-C`）、`files_only`（等同 `-l`）；输出路径一律相对当前工作目录；最多显示 100 行，超出时提示缩小范围：装有 ripgrep 时提示带数量，形如 `... at least N more matches not shown (...)`（`files_only` 时为 files，带 `context` 时为 lines；某个文件达到单文件匹配上限时括号里注明 `some files hit the per-file cap`），因此 N 是下限；内置搜索的提示不带数量。
>
> **终端工具要点**（`bash` / `powershell`）：
> - 默认超时 120 秒，`timeout` 参数单位毫秒，上限 10 分钟（600000）。超时或取消时仍返回已产生的输出，末尾追加 `[timed out after Ns]` 或 `[cancelled]`。
> - 每次调用都是新的 shell：`cd`、`export`、`$env:` 的改动不会保留到下一次调用。
> - 子进程环境设置 `NO_COLOR=1`、`TERM=dumb`、`CLICOLOR=0`、`FORCE_COLOR=0` 关闭彩色输出；cmd 回退时自动 `chcp 65001`；Windows 下 Git Bash/cmd 的非 UTF-8 输出按行尝试 GBK 解码；运行中的实时进度同样逐行解码后再推送（不再显示乱码），没有换行的长行超过 4KB 也会推送且不会切断 GBK 字符；输出超过 2MB 截断时，保留的头尾按行边界切。
> - 每个输出流最多捕获 2MB（首尾各 1MB），交给模型时 stdout 保留 30000 字节、stderr 保留 10000 字节（首尾保留）。

### 网络与浏览器

| 工具 | 说明 | 权限 |
|------|------|------|
| `webfetch` | HTTP 获取网页内容并转为文本/Markdown | R |
| `websearch` | 网络搜索：按配置 `web_search` 或环境变量选用 Tavily / Brave，都没有时抓取 DuckDuckGo | R |
| `browser` | 控制 headless Chrome 浏览器（渲染 JS 页面/截图）；**只在用 `-tags chromedp` 构建时注册** | R/W |

> **browser 工具说明**：
> - `navigate`：渲染 JS 页面并返回文本/Markdown/HTML
> - `screenshot`：截图保存为 PNG
> - 需要 `chromedp` 构建标签；默认构建不注册 `browser`（没有 Chrome 时它只是更差的 `webfetch`），请用 `webfetch`

### 计划与任务管理

| 工具 | 说明 | 权限 |
|------|------|------|
| `todowrite` | 创建和管理结构化任务列表 | W(本地) |
| `plan_mode` | 进入计划模式（只读操作） | R |
| `exit_plan_mode` | 退出计划模式 | W |
| `execute_plan` | 执行计划中的任务（通过子智能体）；`max_agents`（1–8，默认 4）限制并行子智能体数 | W |
| `task` | 记录一个待办任务（只记录，不会执行）；需开启 `experimental_tools` | W |
| `task_list` / `task_get` / `task_output` | 列出 / 查看已记录的任务；需开启 `experimental_tools` | R |
| `task_update` / `task_stop` | 更新任务状态或输出；需开启 `experimental_tools` | W |
| `brief` | 生成会话或上下文摘要；需开启 `experimental_tools` | R |

### 智能体与团队

| 工具 | 说明 | 权限 |
|------|------|------|
| `agent` | 生成子智能体处理复杂多步骤任务 | W |
| `regression_verify` | Go 回归测试反向验证：检查点旧源码 overlay 必须触发真实测试失败，同一测试在当前源码上通过；执行项目代码，需授权 | W |
| `team_create` | 创建智能体团队并行工作；需开启 `experimental_tools` | W |
| `team_delete` | 删除智能体团队；需开启 `experimental_tools` | W |
| `send_message` | 向任务/团队发送消息；需开启 `experimental_tools` | W |

### 协作

> `lsp` 与 `cron` 工具已移除（11.1.0）：前者没有接入任何语言服务器，后者只记录从不触发。

| 工具 | 说明 | 权限 |
|------|------|------|
| `sleep` | 暂停执行指定秒数（最多 300 秒）；需开启 `experimental_tools` | R |
| `question` | 向用户提问（多选题）；**只在交互式界面注册** | R |
| `skill` | 执行预定义技能 | W |

### MCP 与插件

| 工具 | 说明 | 权限 |
|------|------|------|
| `mcp` | 调用 MCP 服务器工具 | 取决于 MCP 工具 |
| `mcp_resources` | 列出 MCP 资源 | R |
| `mcp_read_resource` | 读取 MCP 资源 | R |

### 技能

| 工具 | 说明 | 权限 |
|------|------|------|
| `skills_list` | 列出可用技能 | R |
| `skill_view` | 加载并查看技能内容 | R |

### Git 工作树

| 工具 | 说明 | 权限 |
|------|------|------|
| `worktree` | 创建 Git 工作树用于隔离开发 | W |
| `exit_worktree` | 退出工作树并清理 | W |

### 工具注册条件

发给模型的工具列表按运行环境裁剪，用不上的工具不占上下文：

- **`question`**：只在交互式 REPL 注册；`-p`、headless（`--no-tui`）和管道模式不注册。在 REPL 里，问题显示在输入行上方，你输入的下一行就是回答（可以输入选项编号）；15 分钟没回答按空答案处理。等待回答时按 Ctrl+C，工具立即返回 `Error: cancelled by user`，后面的问题不再问，你之后输入的内容也不会被当成回答。
- **`browser`**：只在用 `-tags chromedp` 构建时注册。
- **`powershell`**：只在 Windows 上注册。
- **实验性协作工具**：`task`、`task_list`、`task_get`、`task_output`、`task_update`、`task_stop`、`team_create`、`team_delete`、`send_message`、`brief`、`sleep` 默认不注册，配置 `"experimental_tools": true` 后才注册。
- **`execute_plan`** 的 `max_agents`（1–8，默认 4）真正限制并行子智能体数。

---

## 权限模式

四种权限模式，按“自动放行的范围”逐级放宽：

| 模式 | 自动放行（不弹窗） | 仍需确认 |
|------|------------------|---------|
| `default` | 只读工具（`read`/`grep`/`glob` 等）；**整行都是只读简单命令**的 `bash`/`powershell` 命令，如 `git status`、`git diff --stat`、`git log --oneline -5`、`ls -la`、`cat go.mod`、`grep -rn foo .`、`git status && git diff` | 其余一切：写文件、编辑、git 写操作、构建、安装、网络请求（`curl`/`wget` 即使只是 GET 也询问） |
| `auto` | `default` 的全部 + 构建/测试命令（`go build`/`go test`/`go vet`/`go list`、`gofmt`、`cargo test`、`make`/`make test`、`dotnet test/build/run`、`pytest`、`npm run test:unit` 这类 test/lint/build 脚本等）+ 目标路径位于项目工作目录内的 `write`/`edit` | git 写操作（`commit`/`push` 等）、包安装、网络请求、未知命令、项目外写入、MCP 工具、`draw_image`、浏览器截图、`worktree` 等 |
| `bypass` | 全部（`deny` 规则仍生效） | 无（灾难命令仍被硬拦截，见下文） |
| `plan` | 只读工具；整行只读的 `bash`/`powershell` 命令；只影响会话本身的工具（`todowrite`、`question`、`skill`，以及 `agent`/`execute_plan`，子代理的每个调用照样受 plan 限制） | 不询问：其余工具一律拒绝，不管工具自己怎么回答（之前选过的“总是允许”在此模式下不生效）。模型用 `plan_mode` 自行进入的计划模式同样按此执行；它可以用 `exit_plan_mode` 退出（需你确认），但你用 `/mode plan` 设置的计划模式只能由你切换 |

- 没有任何规则命中时，任何模式下都是“询问”（旧版本 `auto` 模式在无规则命中时直接放行，等于放行一切，已修正）。
- 用户在 `policies.json` 或本次会话中配置的 `deny` / `ask` 规则优先于上述自动放行，也优先于 `allow` 规则（判定顺序与匹配方式见[规则判定顺序与匹配](#规则判定顺序与匹配)）。
- Windows 上 `bash` 工具回退到 cmd.exe 时，`default` 与 `auto` 模式都不自动放行任何命令（见[命令运行在哪个 shell](#命令运行在哪个-shell)）。
- `auto` 模式的“项目内写入”按真实路径判断：会解析符号链接与 Windows junction（`mklink /J`），项目内指向项目外的链接及其下的新文件视为项目外；链接超过 40 跳或成环也视为项目外。
- `-p` 单次查询没有人回答询问，“仍需确认”的调用会被直接拒绝；要无人值守地执行这些操作，用 `bypass` 模式，或事先用 `[p]` / `policies.json` 持久化允许规则（见[启动参数](#启动参数)下的 `-p` 说明）。

**“只读命令”如何判定**：按分词结果逐条判断，一行里的所有简单命令都必须只读。出现 `$(`、反引号、`<(`、`>(`、`${`，或任一命令把输出重定向到真实文件（`/dev/null`、`NUL`、`$null` 除外）即不算只读。`env`/`sudo`/`xargs`/`time`/`nohup` 包裹、带路径的可执行文件（`./ls`）、`VAR=1 cmd` 前缀都不算只读。`cd`/`pushd`/`popd`/`Set-Location` 本身算只读（切换目录不改变任何东西，之后的命令各自判定），所以 `cd proj && git status` 整行只读。git 只认 `status`/`log`/`diff`/`show`/`blame`/`ls-files`/`rev-parse` 等只读子命令；`git branch newname`、`git tag v1`、`git stash`、`git config k v`、带 `--output=` 的命令以及任何 `git -c …` 都不算只读。`find` 带 `-delete`/`-exec`/`-execdir`/`-ok`/`-okdir`/`-fprint*`/`-fls` 不算只读。

切换方式：
```
/mode auto
```

### 授权提示

需要确认时，提示形如：

```
  ┃ 需要授权  bash
  ┃ git push
  ┃
    [y] 允许   [a] 本会话记住   [p] 本项目记住   [n] 拒绝
        记住范围: bash 中 git 常规操作（add/commit/push/pull/switch 等，不含 --force、reset、clean、checkout）
```

左侧竖条样式没有右边框，命令再长也不会把框撑破；多行命令（heredoc）每行都在竖条之后。

- `y` / `yes` — 只允许这一次
- `a` / `always` / `总是` — 本次会话内记住“记住范围”一行所写的内容（不写文件，退出 cove 即失效）
- `p` / `permanent` / `永久` — 本次会话生效，并写入 `~/.cove/policies.json`（设置了 `COVE_CONFIG_DIR` 时位于该目录下），**只对当前项目生效**（项目根 = 从启动目录向上找到的含 `.git` 的目录，找不到时为启动目录）。之后在同一项目启动 cove 不再询问。成功时提示会显示实际写入的文件路径，写入的规则作为本项目的磁盘规则装入本会话：用 `/cd` 切换到其他项目时与其他 `policies.json` 规则一起卸下，同一命令会重新询问，按新项目的规则判断。写入失败时提示“未能写入，仅本次会话有效”，规则退化为会话规则
- `n` 或其他输入 — 拒绝
- 15 分钟内没有回答视为拒绝（提示“授权超时”）

提示尽量不带多余文字：“按键即答，无需回车；Ctrl+C 拒绝并停止任务”只在本进程第一次询问时显示；无法按规则记住的命令（带重定向、组合了无法归类的程序等）只是不提供 `[a]`/`[p]`，不再额外说明原因；只有已经记住过相关规则、这一行却没被覆盖时，才用一行暗色字说明哪条规则、为什么没覆盖，避免看起来像“记住了却没生效”。

“记住范围”只列**真正需要授权**的那部分。`cd`、`echo`、`git status`、`git log` 这类只读命令在任何模式下都自动放行，所以既不出现在范围里，也不需要单独记住：`cd proj && git remote -v` 这样整行只读的命令直接执行，不再因为 `cd` 而询问。

**git 常规操作一组**：一行里出现 `add`、`commit`、`push`、`pull`、`fetch`、`switch`、`merge`、`rebase`、`stash`、`branch`、`tag`、`cherry-pick`、`mv` 这些子命令的常规用法时，`a`/`p` 记住的是整组“git 常规操作”，所以“暂存 → 提交 → 推送”只问一次。下面这些不在组内、照常询问（记住整组之后也一样）：`push --force`/`-f`/`--force-with-lease`/`--delete`/`--mirror`/`origin :branch`/`origin +branch`（`+` 引用规格即强推）；`reset`、`clean`、`rm`、`restore`、**`checkout`**（整体不在组内：`checkout <路径>` 会丢弃工作区改动，而路径和分支名无法区分，请让模型用 `switch`）；`branch -D`/`-M`/`-C`、`tag -d`；`switch --discard-changes`；`stash drop`/`clear`；`rebase -i`/`--exec`/`--edit-todo`、`pull --rebase=interactive`；任何子命令的 `--interactive`/`--patch`/`--edit`（`-i`/`-p`/`-e`，会挂住非交互 shell）、`--force`/`-f`（`add -f` 除外）、`--receive-pack`/`--upload-pack`（会执行程序）；没有 `-m`/`-F`/`-C`/`--no-edit` 的 `commit` 和没有 `-m`/`-F` 的 `tag -a`/`-s`（会打开编辑器）；长选项按 git 的唯一前缀规则匹配，`--force-w`、`--del`、`--disc` 这类缩写同样被拒；`git -c …` 等 `-C <dir>`/`--no-pager` 以外的全局选项。不在组内的 git 写操作仍按“程序 + 子命令”前缀记住，例如 `"git push" 开头的命令`（记住后 `git push --force` 也会放行，请留意）。`policies.json` 里对应 `"command_group": "git"`，规则 ID 为 `allow-bash-group-git`。

其他命令对 `bash`/`powershell` 只记住**命令前缀**，范围里写作 `"go test" 开头的命令`：

- 前缀取法：`git`、`go`、`npm`、`docker`、`kubectl`、`dotnet`、`cargo`、`pip` 等带子命令的工具取“程序 + 子命令”（`go test`、`npm run`、`docker compose`），其他程序只取程序名（`rm`、`sed`）。复合命令会为其中每条需要授权的命令各记一个前缀（`cd src && go test ./... | tee out.txt` 一次回答记住 `go test` 和 `tee`）
- 之后一行命令里的**每一条**命令（`&&`、`||`、`;`、`&`、管道、换行、子 shell 分隔的都算）都必须以已允许的前缀开头、属于已记住的组、或本身只读，才免询问，按词比较：允许 `go test` 后，`go test ./... && rm -rf x`、`go test ./... | tee out.txt`（除非也允许了 `tee`）、`sudo go test`、`FOO=1 go test`、`go vet` 仍会询问；`cd src && go test ./... && echo done` 放行
- **引号内的参数**：在 Git Bash / sh 和 PowerShell 下，整词位于引号内的参数可以包含 `; & | < > ( )`，所以 `git commit -m "fix(api): handle 429; retry"`、`git commit -m 'a && b'` 能被 `git commit` 前缀覆盖。**cmd.exe** 下保持严格：单引号在 cmd 中不是引号，任何含这些字符的参数都不被覆盖（照常询问）。行内出现 `\"` 或 `\'` 时不信任引号；PowerShell 下未加引号的 `$变量` 参数不覆盖（`$x.Method()` 会执行代码）
- **heredoc**：`<<EOF`、`<<'EOF'`、`<<-EOF` 与 here-string `<<<` 的内容是 stdin 数据，不参与前缀判断，所以 `git commit -F- <<'EOF' … EOF` 能被 `git commit` 前缀覆盖
- 仍然不会被前缀规则放行、照常询问的情况：任意位置出现 `$(`、反引号、`${`、`<(`、`>(`（引号内也算）；输出重定向到文件（`/dev/null`、`NUL`、`$null` 除外）
- `sudo`、`env`、`xargs`、`bash -c`、`VAR=值` 开头，或 `git -C dir …` 这类取不到子命令的命令无法安全地记住前缀，提示中不提供 `[a]`/`[p]`；此时输入 `a` 或 `p` 只允许本次。整行只读却仍被询问的命令（`ask` 规则命中、cmd.exe 回退）同样没有可记住的范围
- Windows 上 `bash` 回退到 cmd.exe 时不信任任何只读判定：复合命令里的每一条（包括 `cd`）都会被记成前缀，也都必须被前缀覆盖
- 其他工具（`write`、`edit` 等）选 `a`/`p` 对整个工具生效（范围写作 `工具 write 的所有调用`）；MCP 调用按“服务器 + 工具名”记住
- plan 模式下这些规则不起作用，非只读工具照样被拒绝
- 以上逐词比较只针对**允许**规则（`[a]`/`[p]`/`allow`）；`deny`/`ask` 规则按归一化后的命令匹配，见下一节

### 规则判定顺序与匹配

一次工具调用按以下顺序判定，先命中者决定结果：

1. 工具自身拒绝 → 拒绝（如 `webfetch`/`browser` 拒绝内网地址、plan 模式下的写入），任何规则和模式都不能覆盖
2. `deny` 规则 → 拒绝（任何模式，包括 `bypass`，与 `priority` 无关）
3. `plan` 模式 → 只放行只读工具、整行只读的 shell 命令和只影响会话的工具（见上表）
4. `bypass` 模式 → 放行（`ask` 规则在 `bypass` 下不生效，只有工具自身拒绝、`deny` 与 `plan` 能拦住 `bypass`）
5. `ask` 规则 → 询问。即使同一命令还被 `[a]`/`[p]`、整工具允许或 `policies.json` 的 `allow` 覆盖，也照样询问——除非那条 `allow` 的 `priority` 更高
6. `allow` 规则 → 放行
7. 权限模式的默认行为（见上表）

`priority` 只在 `ask` 与 `allow` 之间起作用：`priority` 更高的 `allow` 先于优先级较低的 `ask`，同级时 `ask` 取胜，与书写顺序无关；`[a]`/`[p]` 记住的规则优先级为 0。`deny` 不看 `priority`，总是取胜。所有规则（包括通配工具名如 `mcp__*` 与 `param_match`）都按同一顺序判定。

**`deny` / `ask` 前缀规则按归一化后的命令匹配**，换个写法绕不开：

- 程序名去掉目录与 `.exe`，不区分大小写（`/usr/bin/git`、`GIT.EXE` 都视为 `git`）
- 剥掉包裹命令（含其选项）：`sudo`、`doas`、`env`、`nohup`、`time`、`command`、`nice`、`exec`、`busybox`、`timeout`，以及 `VAR=value` 前缀
- `git`、`docker`、`kubectl`、`helm`、`go`、`npm` 等带子命令的工具跳过子命令之前的全局选项（`-C dir`、`-c k=v`、`--no-pager`、`-P`、`--git-dir=…`、`-n ns`、`--context c` 等）

例如 deny `git push` 能命中 `git -C . push`、`/usr/bin/git push`、`sudo git push`、`command git push`、`env X=1 git push`。

**`allow` 规则不做归一化**，仍按原样逐词比较（见上一节），所以允许 `go test` 不会放行 `sudo go test` 或 `/usr/local/go/bin/go test`。

**`deny` / `ask` 规则也看嵌套命令**：`bash -c "git push"`、`sh -c`、`cmd /c`、`powershell "…"`、`eval`/`iex`、`xargs git push`、`find -exec/-execdir/-ok`、管道进 shell 的 heredoc 里的命令都会被展开匹配。命令行上定义了 git 别名（`git -c alias.p=push`、`--config-env`、`git config alias.*`）时，所有针对 git 的 deny/ask 规则都命中；命令的程序名是变量（`$GIT push`、`G=git; $G push`、`${GIT} push`、`$env:GIT push`、`%GIT% push`，以及嵌套的 `bash -c "$CMD"`、`eval "$x"`）时看不出要运行什么，所有 deny/ask 规则都命中；这类命令也不算只读、不自动批准、不能记住前缀。git 组的 ask/deny 规则只覆盖非只读用法，`git branch -a`、`git tag`、`git stash list`、`git remote -v` 这类只读列表命令不受影响。

**`param_match` 里的 `command`**（bash/powershell 工具）按简单命令逐条匹配，而不是整行前缀：`allow` 要求行内每条命令都匹配该 glob，且不含命令替换或写文件的重定向；`deny`/`ask` 只要任一条命令（含嵌套命令）匹配即生效。末尾单个 `*` 按整词扩展，`git status*` 匹配 `git status --short`，不匹配 `git statusx`，也不再放行 `git status; rm -rf ./src`。

### 灾难命令硬拦截

无论哪种模式（包括 `bypass`），以下命令都会被直接拦截：

- 递归删除根目录/家目录/系统目录或盘符根（如 `rm -rf /`、`rm -rf /**`、`rm -rf ~`、`rm -rfvvv /`、`Remove-Item -Recurse C:\`、`rm -rf C:/Windows`、`rm -rf /c/`（Git Bash 盘符）、`rm -rf /{etc,usr}`、`find / -delete`、`find -L / -exec rm …`、`echo ~ | xargs rm -rf`、`rm$IFS-rf$IFS/`）
- 格式化磁盘或写裸设备（`mkfs`、`dd of=/dev/sda`、`shred /dev/sda`、`tee`/`cp`/`mv` 写入 `/dev/sd*`、`/dev/nvme*`、`\\.\PhysicalDrive*`、`format c:`）、关机重启、fork bomb（含 `function f { f|f& }; f` 写法）
- 把下载或解码的内容直接交给解释器执行：管道形式（`curl … | sh`、`curl … | bash -s -- x`、`| bash /dev/stdin`、`| bash -`、`| python3 -`、`irm … | iex`、`base64 -d | bash`），以及进程替换 / 命令替换形式（`bash <(curl …)`、`source <(curl …)`、`. <(curl …)`、`python3 <(curl …)`、`sh -c "$(curl …)"`、`eval "$(curl …)"`、直接执行 `$(curl …)`、`echo "$(curl …)" | sh`、`iex (iwr …)`、`iex (irm …)`）；下载源包括 `curl`、`wget`、`fetch`、`iwr`/`irm`。只把下载结果当数据参数的不拦（`python3 parse.py <(curl …)`、`diff <(curl a) <(curl b)`、`git commit -m "$(curl …)"`）。已知未覆盖：`iex (New-Object Net.WebClient).DownloadString('…')`
- 通过嵌套 shell 包装的上述命令：`bash -c "rm -rf ~"`、`sh -c 'rm -rf /'`、`cmd /c rd /s /q C:\`、`powershell -Command "Remove-Item -Recurse -Force C:\"`、`powershell "Remove-Item -Recurse -Force C:\Windows"`（第一个位置参数按 -Command 处理）、`pwsh -c "rm -r -fo ~"`、多层 `bash -c "bash -c '…'"`、`eval '…'`/`iex '…'`/`Invoke-Expression "…"`，`xargs`/`find -exec` 行内写死的目标（`xargs rm -rf /`、`find . -exec rm -rf / \;`），以及喂给或管道进 shell 的 heredoc（`bash <<EOF`、`cat <<'EOF' | sh` 里写 `rm -rf /`）
- 含单独 CR、不可见空白或零宽字符的写法按去掉这些字符后的形式检查（`echo hi<CR>Remove-Item … C:\Windows`、`r<零宽空格>m -rf /` 同样拦截）
- `powershell -EncodedCommand …`：内容无法审查，一律拦截（理由 “encoded command”）

不会误拦截：删除项目内的文件或目录（如 `rm -rf build`）、`bash -c "go test ./..."`、`git commit -m "rm -rf /"`、`echo ":(){ :|:& };:"`（引号内只是参数）、`cat <<EOF > notes.md` 里写的 `rm -rf /`（heredoc 喂给 `cat` 只是数据），这些按当前模式正常确认。

### 无法确定含义时一律询问

命令的含义没法可靠判断时，按“需要授权”处理，不会自动放行，也不会被已记住的规则覆盖：

- **不寻常的空白与不可见字符**：PowerShell 把单独的 CR、NEL、U+2028/U+2029 当作换行，把 FF、VT、不间断空格等当作参数分隔，`ls<CR>Remove-Item -Recurse -Force src` 实际是两条命令。含这类字符、其他控制字符、零宽/双向控制等不可见格式字符或非法 UTF-8 的命令，一律不算只读、不自动批准、不被记住的前缀/分组/`param_match` 规则覆盖，改为询问。
- **bash 的 `@{…}`**：`git stash show -p stash@{0}`、`git diff HEAD@{1}`、`git log @{u}..HEAD` 里词内的花括号按字面处理，算只读、可记住；`{a,b}`、`{1..3}` 花括号展开仍询问，PowerShell 下未加引号的花括号仍询问。
- **子命令前有选项**：记住的 npm/pnpm/yarn、go、cargo、dotnet 常规组，不覆盖子命令前带选项的写法（`npm --prefix x publish`、`pnpm -C x test`、`go -C x install`、`cargo +nightly build`），这类命令也不提供记住整组的选项。以前选项的值会被当成子命令，`npm --prefix test publish` 会被当作 `npm test` 放行。
- **bash 的反斜杠**：bash 下按 bash 的规则理解反斜杠续行和转义；`r\m -rf /`、`rm \<换行>-rf ~` 这类写法同样被硬拦截。含义取决于反斜杠处理方式的行（如 `find . -f\<换行>ls x`）需要授权。
- **curl / wget**：只有全部选项都在只读白名单内、方法为 GET/HEAD、输出只到 stdout 时才算只读；`--json`、`-d`、`-F`、`-T`、`-o 文件`、`-D`、`-c`、`--trace`、`-XDELETE` 等都需要授权。PowerShell 下 `curl`/`wget` 是 `Invoke-WebRequest` 的别名，只允许单个 URL（可加 `-Uri`、`-UseBasicParsing`）；`iwr`、`irm` 与 `Invoke-*` 总是询问。
- **可能执行项目脚本的子命令**：yarn、pnpm、composer 遇到不是自身内置命令的子命令，会执行项目里同名的脚本，所以只有确定是内置命令的才算只读（如 `yarn info`/`why`、`pnpm list`/`outdated`/`why`/`audit`、`composer show`/`outdated`/`why`）；`yarn doctor`、`composer freeze` 等需要授权。`audit --fix`（含缩写）需要授权。
- **`git branch`** 只在全部选项都是已知的列表选项时算只读；`-uorigin/main`、`--set-u=…` 这类附带值或缩写的写法会改 `.git/config`，需要授权。
- **`date`**：`--s…`（`--set` 的缩写）、含 `s` 的组合短选项、以及除 `+格式` 以外的参数都需要授权。
- **ask/deny 规则覆盖整组**：对 git、npm 等常规组设置的 ask/deny 规则，同样拦下该程序的高风险写法（`git push --force`、`git reset`、`git -c …`、`npm publish`、`-g`），以前只拦常规用法，高风险的反而放行。`--force-create` 这类以被拒选项开头的长选项、`npm --location=global|user`、`--prefix` 也不算常规。
- **yarn 不再有只读命令**：仓库的 `yarnPath` 会让任何 yarn 命令（包括 `yarn --version`）执行指定脚本。
- 其他不再免询问的：PowerShell 的 `-Path:\\host\share`、`FileSystem::\\host\share`（访问网络共享，会带上凭据）；`ag --pag…`、`rg --hostname-bin`；`docker compose config -o`；`file --comp`/`-C`。
- **auto 模式下与 `go test` 一致**：`npm test`、`npm run test|build|lint|check|typecheck|…`（含 `test:unit`、`lint:fix`、`build:prod` 这类带冒号后缀的脚本名）、`pnpm`/`yarn` 的 `test`/`build` 等，以及 `dotnet test/build/run`（带 `--force` 除外）、`pytest`（带 `--basetemp` 除外）、`go list`、`gofmt`、不带参数的 `make`，都按构建/测试类处理，auto 模式下免询问（`--` 之前带任何选项仍会询问）。
- **子进程 git 不使用夹带的裸仓库**：shell 工具启动的 git 设置 `safe.bareRepository=explicit`，仓库里夹带的裸仓库目录（HEAD、config、objects/）不会在 `cd 该目录 && git status` 时被使用。

### 命令运行在哪个 shell

`bash` 工具在 Windows 上优先使用 Git for Windows 的 bash（通过 `git.exe` 的位置找到），不会使用 `C:\Windows\System32\bash.exe`（WSL 启动器）；找不到时依次回退到 PowerShell、cmd。实际使用的解释器会写进工具描述和系统提示词告诉模型（回退到 PowerShell/cmd 时提示模型使用对应语法）。命令以非交互方式运行：`git commit` 不带 `-m` 会直接失败而不是打开编辑器，git 不会在终端里等待输入密码。

**回退到 cmd.exe 时不自动放行**：cmd 的引号与转义规则无法可靠分析，因此回退到 cmd 时，`default` 与 `auto` 模式都不再把任何命令当作只读或构建命令自动放行，`dir`、`git status`、`go test` 也会询问；只有已记住的前缀规则、`policies.json` 的 `allow` 规则或 `bypass` 模式能免询问。Git Bash 与 PowerShell 下不受影响。建议安装 Git for Windows。

**git 子进程环境**：shell 工具启动的进程带 `GIT_PAGER=cat`，并通过 `GIT_CONFIG_COUNT` / `GIT_CONFIG_KEY_n=core.fsmonitor` / `GIT_CONFIG_VALUE_n=false` 禁用 fsmonitor（环境里已有 `GIT_CONFIG_COUNT` 时顺延序号），防止仓库 `.git/config` 里配置的 fsmonitor 程序随自动放行的 `git status` 执行。

> **已知限制**：仓库自身 git 配置（如 `diff.external`、`.gitattributes` 指定的 `diff.<driver>.command` / `textconv`）里的外部程序仍可能随自动放行的只读 git 命令（`git diff`、`git show`、`git log -p` 等）执行——禁用它们会让普通的 `git diff` 失败，所以没有加。打开来路不明的仓库前，请先检查其 `.git/config` 与 `.gitattributes`。

### 持久化权限规则（policies.json）

持久化规则保存在 `~/.cove/policies.json`（设置了 `COVE_CONFIG_DIR` 时位于该目录下），与 `config.json` 同目录。`[p]` 选项会自动追加规则，也可以手工编辑。启动时按当前项目根加载；用 `/cd` 切换目录后，会按新的项目根重新加载（`scope` 为旧项目的规则不再生效）。新目录的 `policies.json` 无法解析时，`/cd` 输出一行警告：新项目的规则未生效，沿用切换前已加载的规则。`allow`、`deny`、`ask` 三种规则都会生效：`deny` 在 `bypass` 模式下也拒绝；`ask` 让只读命令也先询问，且不会被 `allow` 盖过（`bypass` 下不生效）；同优先级时 `deny` > `ask` > `allow`（见[规则判定顺序与匹配](#规则判定顺序与匹配)）。

文件是一个 JSON 数组，每个元素一条规则：

```json
[
  {
    "id": "allow-bash-git commit",
    "description": "",
    "tool_pattern": "bash",
    "action": "allow",
    "priority": 0,
    "enabled": true,
    "command_prefix": "git commit",
    "scope": "D:\\github\\cove-main"
  },
  {
    "id": "allow-mcp-github-create_issue",
    "tool_pattern": "mcp",
    "action": "allow",
    "enabled": true,
    "input_equals": {"serverName": "github", "toolName": "create_issue"},
    "scope": "D:\\github\\cove-main"
  }
]
```

| 字段 | 说明 |
|------|------|
| `id` | 规则标识 |
| `description` | 说明文字（可省略） |
| `tool_pattern` | 工具名，如 `bash`、`write`、`mcp`；`*` 表示所有工具 |
| `action` | `allow` / `deny` / `ask` |
| `priority` | 数值越大越先评估（可省略，默认 0） |
| `enabled` | `false` 时规则不生效 |
| `command_prefix` | 仅 shell 工具：命令前缀，`allow` 与上文 `[a]`/`[p]` 的前缀规则相同（逐词比较，一行里每条命令都须被覆盖）；`deny`/`ask` 按归一化后的命令匹配 |
| `input_equals` | 要求工具参数中这些字段与给定字符串完全相等（`[p]` 对 MCP 调用使用 `serverName` + `toolName`） |
| `scope` | 规则所属的项目根目录；为空表示所有项目。非空且不等于当前项目根的规则在加载时被忽略（Windows 下不区分大小写） |

- 文件以原子方式写入（临时文件 + rename）；读取失败或 JSON 损坏时不会被覆盖，启动日志会给出警告，规则不生效。`/doctor` 与 `/diagnose` 的“权限规则文件”一项会显示加载失败原因（E3004）。
- 要撤销某条“永久允许”，从文件里删除对应元素即可（下次启动生效）。

---

## 配置系统

### 配置文件位置

- 用户配置：`~/.cove/config.json`
- 项目配置：项目根目录的 `.cove.json`

### 项目配置的信任

`.cove.json` 跟着仓库走，克隆别人的仓库就会带上别人写的配置。其中能执行命令或改变密钥去向的设置，在确认信任前**不生效**：

| 字段 | 何时需要信任 |
|------|-------------|
| `provider.name` / `provider.api_key` / `provider.base_url` | 总是（`base_url` 指向别处，首个请求就会把你的 API key 发过去） |
| `mcp_servers` | 总是（启动时就会拉起这些进程） |
| `done_verify_commands`、`system_prompt`、`memory_embedding`、`web_search` | 总是 |
| `permission_mode` | 取值不是 `default` 或 `plan` 时 |
| `done_verify_auto`、`done_verify_tests` | 取值为 `true` 时 |
| `max_budget_usd` | 调高了全局上限时（调低直接生效） |
| `max_sessions` | 比当前生效值更小时（会删除更多旧会话） |

- 启动时如有被忽略的设置，stderr 用黄色提示文件路径和字段名。确认可信后在交互模式输入 `/trust`，再 `/restart`。
- 信任记录在 `~/.cove/trusted_projects.json`（设置了 `COVE_CONFIG_DIR` 时位于该目录），按 `.cove.json` 的绝对路径保存其内容的 SHA-256；**文件内容一改就失去信任**，需要重新 `/trust`。`/trust` 信任的是本进程启动时读到的内容，提示之后被替换的文件仍不受信任。
- 其余字段（模型、思考、上限、展示类开关等）照常直接生效；更严格的取值（`plan`/`default` 模式、更低的预算、关闭校验）也直接生效。
- `.cove.json` 是符号链接、设备、管道或超过 1 MiB 时直接忽略（stderr 警告）。
- `/profile save` 只保存你自己的设置：`.cove.json` 提供的 provider、`system_prompt`、`permission_mode`、模型、预算等不会被复制进全局 profile（会话中你手动改过的值照常保存）。

**自动校验也需要信任**：回合结束时自动推断并运行的构建/测试校验（`go build`、`cargo check`、`npx --no-install tsc`、`dotnet build`、`npm run build --if-present`、`python -m compileall`，以及自动运行的 `go test`/`pytest`/`dotnet test`）会执行仓库里的代码（`package.json` 脚本、`build.rs`、MSBuild 目标、`conftest.py`）。它们只在**受信任的项目目录**里、或 `auto`/`bypass` 模式下（这两种模式下构建/测试命令本来就免询问）自动运行；否则跳过并暗色提示一次“未信任的项目不自动运行构建/测试校验（npm run build 等会执行仓库里的脚本）；确认可信后输入 /trust”。跳过不算通过也不算失败。你在用户级 `config.json` 里明确写的 `done_verify_commands` 不受影响。

对没有 `.cove.json` 的项目，`/trust` 信任的是项目目录（git 根目录，没有则为当前目录），只对这个目录生效，不包括其他目录；信任 `.cove.json` 时也同时信任它所在的目录。

### config.json 示例

```json
{
  "model": "deepseek-v4-pro",
  "provider": {
    "name": "deepseek",
    "api_key": "sk-***",
    "base_url": ""
  },
  "permission_mode": "default",
  "max_budget_usd": 10,
  "thinking_tokens": 16000,
  "debug": false,
  "mcp_servers": {
    "filesystem": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-filesystem", "/path/to/allowed"],
      "type": "stdio"
    },
    "atlassian": {
      "url": "https://mcp.atlassian.com/v1/mcp",
      "type": "sse"
    }
  }
}
```

### 配置字段说明

| 字段 | 类型 | 说明 |
|------|------|------|
| `model` | string | **复杂任务模型**，如 `deepseek-v4-pro`（高级）、`claude-sonnet-4-20250514`、`gpt-4o` |
| `model_fast` | string | **简单任务模型**（快速/便宜），如 `deepseek-flash`（旧名 `deepseek-v4-flash`）、`gpt-4o-mini`、`claude-haiku-4-5`。不配置则与 `model` 相同，即不做模型切换 |
| `provider.name` | string | 提供商名称（anthropic/deepseek/openai/glm/kimi/qwen/doubao/...）|
| `provider.api_key` | string | API 密钥。留空时读取提供商对应的环境变量（如 `DEEPSEEK_API_KEY`、`LLM_API_KEY`）；配置文件里有值时优先于环境变量 |
| `provider.base_url` | string | 自定义 API 端点（留空则自动匹配提供商默认地址） |
| `permission_mode` | string | 默认权限模式（default/plan/auto/bypass）。拼错的值按 `default` 运行，`/diagnose` 会提示 |
| `max_budget_usd` | number | 会话预算上限（美元）。每次模型调用前都会检查，超过时暂停；主循环、子智能体、记忆提取、上下文压缩等所有模型调用都计入。费用达到 80% 时提示一次（见[预算管理](#预算管理)） |
| `max_iterations` | number | 单轮最多调用模型次数，默认 200（未设置或 ≤0 取默认）。交互模式到达时询问是否继续；`-p` 与 headless 为硬上限（`-p` 可用 `--max-turns` 覆盖）。见[单轮上限](#单轮上限与-continue)（`config.json`、项目 `.cove.json`、profile 三处均可设置，优先级 profile > `.cove.json` > `config.json`） |
| `max_turn_minutes` | number | 单轮用时上限（分钟），交互模式默认 60，`0` 关闭，到达时询问；`-p` 与 headless 只有在配置里显式写了该键时才有时间上限（默认不限时），到达即报错结束（`config.json`、项目 `.cove.json`、profile 三处均可设置，优先级 profile > `.cove.json` > `config.json`）；任一处写了该键都算显式设置 |
| `subagent_max_iterations` | number | 每个子智能体（`agent` 工具、`execute_plan` 的任务）最多调用模型次数，默认 60；到达上限时返回部分结果并标注（`config.json`、项目 `.cove.json`、profile 三处均可设置，优先级 profile > `.cove.json` > `config.json`） |
| `max_sessions` | number | **每个项目**最多保留的会话数，默认 200（按项目的 git 根分组计数，没有记录目录的旧会话自成一组）；更早的会话在回合结束时自动删除（当前会话不会被删），负数关闭自动清理。有删除时回合结束摘要行显示“已清理 N 个旧会话（max_sessions=200）”，进程内首次删除另记一条警告日志。见[会话保存](#会话保存)（`config.json`、项目 `.cove.json`、profile 三处均可设置，优先级 profile > `.cove.json` > `config.json`） |
| `thinking` | string | 支持该能力的提供商（Anthropic）的思考模式：`adaptive`（由模型决定是否思考、思考多少，推理摘要会显示出来）或 `disabled`；留空则使用模型默认值 |
| `effort` | string | 推理深度：`low` / `medium` / `high` / `xhigh` / `max`；留空则使用模型默认值 |
| `done_verify_commands` | string[] | 模型声称完成后必须通过的校验命令（如 `go build ./...`），不通过则打回继续修改 |
| `done_verify_auto` | boolean | 未配置 `done_verify_commands` 时，按项目自动推断校验命令，且只在本轮改过文件时执行：`go.mod` → `go build ./...`，`Cargo.toml` → `cargo check`，本地安装了 TypeScript → `tsc --noEmit`，单个 `*.sln`（没有 sln 时单个 `*.csproj`）→ `dotnet build --nologo -v q`，`package.json` 有非空 `scripts.build` → `npm run build --if-present`，`pyproject.toml`/`setup.py` → `python -m compileall -q -x "(\.venv\|venv\|node_modules\|\.git\|__pycache__)" .`（PATH 上只有 `python3` 时用它）。首轮开始时打印一次“完成校验命令：…”（`/cd` 后再打印一次）。默认开启，设为 `false` 关闭 |
| `done_verify_tests` | boolean | `done_verify_auto` 开启时，在编译检查之后再跑本轮改动涉及的测试：Go 对改动所在的包跑 `go vet` 和 `go test`；.NET 对改动项目所属的测试项目（名字含 Test，或通过 ProjectReference 引用了改动项目）跑 `dotnet test`；Python 对改动的测试文件，以及与改动模块同名的 `test_<模块>.py` 跑 `pytest`（需已安装 pytest）。Rust 和 JS 不支持。默认开启，设为 `false` 关闭 |
| `done_self_review` | string | 完成前自审：`off`（默认）/ `on` / `auto`。开启后，本轮改过文件并通过校验时，由只读的 review 子代理审一遍本轮 diff，有问题就交回模型修改，每轮最多一次。`auto` 只审改动 40 行及以上的轮次。每次自审多一次子代理调用 |
| `done_verify_timeout_seconds` | int | 每条完成校验命令的超时秒数；`0`（默认）为 120 秒，`dotnet`/`npm`/`pytest`/`go test` 命令 300 秒。超时的命令只提示“verification did not finish within N s; not counted as a failure”，不算失败：不打回、不升级模型，本轮正常结束 |
| `done_check` | string | 完成前目标自检：`auto`（默认；空值或未知值也按 auto）/ `on` / `off`。`auto` 只在本轮使用的模型属于快速档（名字含 flash/mini/lite/tiny/fast/haiku/nano）或 provider 不是 anthropic（如 DeepSeek）时启用。见[完成前自检](#完成前自检-done_check)（`config.json` 与 `.cove.json` 可设，profile 暂不支持） |
| `show_reasoning` | boolean | 是否把思考型模型（如 DeepSeek V4）的完整推理过程实时输出到对话区。默认关闭：推理进度只显示在状态行（"思考中… 已推理 N 字"） |
| `disabled_skills` | string[] | 不加载的技能名称列表（内置或自定义均可），如 `["spike", "plan"]` |
| `system_prompt` | string | 你自己的长期指令（如"提交信息用英文"），会**追加**到内置系统提示词末尾，不会替换内置规则 |
| `thinking_tokens` | number | 已不再生效：新版 Claude 模型不接受固定的思考 token 预算，请改用 `thinking` + `effort` |
| `debug` | boolean | 调试模式（开启详细日志） |
| `verbose` | boolean | 预留字段，当前版本没有任何行为；可在 profile 中保存但不会改变输出 |
| `mcp_servers` | object | MCP 服务器配置（支持 stdio/SSE/Streamable HTTP 传输） |
| `profiles` | object | 具名配置组，可覆盖 `model`、`model_fast`、`provider`、`permission_mode`、`max_budget_usd`、`thinking_tokens`、`debug`、`verbose`、`system_prompt`；用 `/profile save/switch` 管理 |
| `active_profile` | string | 启动时应用的 profile 名称（`--profile` 参数优先）；名称不存在时会给出警告并使用基础配置 |
| `context_window` | number | 当前 `model` 的上下文窗口（token）。用于 cove 认不出的模型（本地 llama.cpp、LM Studio 等），决定压缩触发点与每次请求的输出上限；0 或不设按模型名估算（认不出时按 32K）。E2008 的处置会自动把学到的值写进 `model_context_windows`（见[自动处置](#自动处置)） |
| `model_context_windows` | object | 按模型名记录的上下文窗口，如 `{"qwen3.6-27b": 16384}`。E2008 处置从服务端报错学到真实窗口后自动写入，下次启动直接生效；按模型名区分，切换 profile 不会把一个模型的窗口套到另一个上。窗口已知且小于 48K 时，cove 只向模型发送核心工具（读写编辑、shell、搜索、待办、提问、repo_map、skill、webfetch），repo map 摘录、记忆注入和单个工具结果的上限也按窗口比例缩小 |
| `experimental_tools` | boolean | 默认 `false`。开启后才注册实验性协作工具 `task`、`task_*`、`team_*`、`send_message`、`brief`、`sleep`（见[工具注册条件](#工具注册条件)） |
| `web_search` | object | `{"provider": "tavily\|brave\|duckduckgo", "api_key": "..."}`。配置了 `provider` 时以它为准，`api_key` 留空时读对应环境变量（`TAVILY_API_KEY` / `BRAVE_API_KEY` / `BRAVE_SEARCH_API_KEY`）；未配置时仍按环境变量选择；都没有则抓取 DuckDuckGo。`/diagnose` 与 `/diagnose quick` 末尾会提示未配置或缺 key |
| `memory_embedding` | object | 可选：`{"base_url", "api_key", "model"}`，为记忆检索启用远程语义向量；留空的字段沿用主 provider 的值。不配置则只用关键词检索，不产生额外请求 |

> 已移除的字段：`telemetry`（11.1.0 起删除；旧 `config.json` 里的该键仍能正常加载，保存配置时原样保留但不生效，`COVE_TELEMETRY` 环境变量也不再有意义）。

### Hooks（hooks.json）

工具调用前后可以运行自定义命令。只读取**用户级** `~/.cove/hooks.json`（设置了 `COVE_CONFIG_DIR` 时位于该目录下；不读取项目目录里的 hooks 文件，避免克隆下来的仓库在本机执行命令）：

```json
{
  "hooks": {
    "BeforeTool": [
      {"matcher": "bash", "command": "echo %date% >> C:/logs/cove-bash.log"}
    ],
    "AfterTool": [
      {"matcher": "write|edit", "command": "gofmt -l .", "async": true, "timeout": 10}
    ]
  }
}
```

| 字段 | 说明 |
|------|------|
| 事件名 | `BeforeTool`、`AfterTool`、`SessionStart`、`SessionEnd`；也接受别名 `PreToolUse`/`PostToolUse`。`SessionEnd` 在退出时触发一次（`/exit`、`/restart`、Ctrl+D、headless 读完输入、`-p` 结束），cove 等它完成（含 `async` hook）再退出，总上限 30 秒；配置了 SessionEnd hook 时退出前在 stderr 提示“正在运行 SessionEnd hook…” |
| `matcher` | 对工具名的正则（Go RE2），匹配**完整**工具名：`bash` 不匹配 `bash_output`，`write\|edit` 只匹配这两个工具；写了 `^` 或 `$` 的按原样使用；空或 `*` 表示所有工具。**不区分大小写**，并接受 Claude Code 的工具名：`Bash`（也匹配 `powershell`）、`Edit`/`MultiEdit`、`Write`、`Read`、`Grep`、`Glob`、`WebFetch`、`WebSearch`，从 Claude Code 复制来的 hook 可直接用 |
| `command` | 一条命令行，用与 `bash` 工具相同的 shell 执行（Windows 上依次选 Git Bash → PowerShell → cmd；其他系统 bash/sh）。stdin 收到 JSON（`event`、`tool_name`、`tool_input`、`model`、`session_id`、`cwd`）；`BeforeTool` 钩子在 stdout 输出 `{"continue": false, "message": "..."}` 可阻止这次工具调用（非零退出码时同样有效），**退出码 2** 也表示阻止（原因取自 stderr，与 Claude Code 的 PreToolUse 约定相同）；其他非零退出码、非 JSON 输出视为不阻止；超时不阻止，记一条日志 |
| `timeout` | 秒，默认 60 |
| `async` | `true` 表示不等待结果（因此不能阻止工具调用） |

- 文件带 UTF-8 BOM（如 PowerShell 5.1 `Set-Content -Encoding utf8` 写出的）也能读取。
- 无效条目（未知事件、空 `command`、正则错误）会在启动日志里警告，其余有效条目照常生效；JSON 本身解析失败时全部忽略并警告。
- `/hooks` 列出已加载的钩子（事件、匹配工具、同步/异步、超时、命令）以及未加载条目的原因，便于确认配置是否生效。

### 配置迁移

配置系统支持自动迁移，升级版本时无需手动修改 config.json。

---

## 技能系统

Cove 内置 **12 个技能**，编译在二进制里，随 cove 版本一起更新。

### 技能加载机制

- **按需加载**：所有技能只把名称和一句话描述列在系统提示词里，模型判断任务需要时再用 `skill` 工具加载全文。内置技能都是工作流（写计划、TDD、调试等），不会因为读写了某类文件就被自动塞进对话
- **按文件类型注入（仅自定义技能）**：自己写的技能如果在 `paths` 里声明了 glob 模式，操作匹配文件时会自动注入，每个会话只注入一次。不含 `/` 的模式（`*.go`）只比较文件名；含 `/` 的模式（`src/api/*.go`、`internal/**/*.ts`）按相对项目根目录的路径匹配，`**` 匹配任意层目录。`paths`、`allowed_tools`、`steps` 支持逗号分隔、YAML 列表（`- item`）和 `[a, b]` 三种写法；文件为 CRLF 换行或带 BOM 也能正确解析。**项目技能**（仓库里的 `.cove/skills`、`.claude/skills`）与内置、插件或用户技能同名时仍按“越近越优先”覆盖，但加载时会提示一次；解析后指向项目目录之外的项目技能文件（符号链接、目录联接）不会加载
- **禁用**：在配置里写 `"disabled_skills": ["spike", "plan"]`，就不会加载这些技能（内置或自定义都可以）

### 技能来源与优先级

同名技能以更"近"的定义为准：**项目 > 用户 > 插件 > 内置**。

| 来源 | 位置 |
|------|------|
| 内置 | 编译在 cove 二进制中 |
| 插件 | `~/.cove/plugins/<插件名>/skills/` |
| 用户 | `~/.claude/skills/`，然后 `~/.cove/skills/`（后者优先）；后台回顾自动生成的技能也在这里（`auto-<slug>-<hash>/`，见[技能审查](#技能审查-background-review)） |
| 项目 | 从 git 仓库根目录到当前目录，每一级的 `.claude/skills/` 和 `.cove/skills/`，越靠近当前目录越优先 |

- 不读取仓库根目录以上的目录；不在 git 仓库里时，只读当前目录
- 不扫描当前目录下的子目录：克隆或 vendor 进来的第三方代码即使带着 `.claude/skills`，也不会被加载
- `/skills list` 会标注每个技能的来源（`[内置]` `[插件]` `[用户]` `[项目]`）

### 修改内置技能

`/skills export <名称>` 会把内置技能复制到 `~/.cove/skills/<名称>/SKILL.md`，修改后重启即生效。注意：导出的副本会一直覆盖内置版本，**不再随 cove 升级更新**；删除这个文件即可恢复内置版本。

### 内置技能

| 技能 | 说明 |
|------|------|
| `commit-messages` | 编写 Conventional Commits 提交信息 |
| `executing-plans` | 按已有的实现计划分步执行，设置检查点 |
| `github-code-review` | 在 GitHub 上审查 PR：读 diff、行内评论、批准或要求修改 |
| `github-pr-workflow` | GitHub PR 生命周期：建分支、提交、开 PR、盯 CI、合并 |
| `karpathy-guidelines` | 编码准则：先想后写、简单优先、改动精准、目标驱动 |
| `performance-optimization` | 基于测量的性能优化：先 profile，修真正的瓶颈，再验证效果 |
| `plan` | 实现前先写可执行的计划：小任务、精确路径、完整代码 |
| `requesting-code-review` | 提交前自检：安全扫描、质量门禁、自动修复 |
| `safe-refactoring` | 不改变行为的重构：小步走，每步测试保持通过 |
| `spike` | 用一次性实验先验证想法再动手 |
| `systematic-debugging` | 四阶段根因调试：先弄清原因再修，不靠猜 |
| `test-driven-development` | TDD：红-绿-重构，先写测试再写代码 |

### 技能文件格式

```markdown
---
name: my-skill
description: 我的自定义技能
paths: "*.go,*.py"
---

# 技能内容

技能的具体指令和提示词...
```

---

## MCP 协议支持

Cove 支持 **Model Context Protocol (MCP)**，可连接外部工具服务器。

### 传输类型

- **stdio**：本地子进程通信
- **SSE (Server-Sent Events)**：远程 HTTP 流
- **Streamable HTTP**：新版 HTTP 传输协议

### 配置示例

```json
{
  "mcp_servers": {
    "filesystem": {
      "type": "stdio",
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-filesystem", "/path/to/allowed"]
    },
    "atlassian": {
      "type": "sse",
      "url": "https://mcp.atlassian.com/v1/mcp"
    }
  }
}
```

### REPL 管理命令

```
/mcp list          # 列出已连接的 MCP 服务器
/mcp connect ...   # 连接新服务器
/mcp disconnect    # 断开连接
```

> **注意**：`mcp_servers` 里的服务器仅在 Cove **启动时**从配置加载。修改配置后需重启 Cove（或用 `/mcp connect` 手动连接）。

### 断线重连与工具列表刷新

- 服务器首次意外断线（不是 `/mcp disconnect`）时，2 秒后自动重连一次；再次断线需要 `/mcp connect`。
- 收到服务器的 `notifications/tools/list_changed` 通知时自动刷新工具列表。`/mcp connect`、断开、刷新之后，发给模型的工具定义缓存随之失效，模型下一次调用就能看到新工具。
- 单个工具的 schema 超过 4KB 时依次精简：先去掉各处 `description` 字段 → 只留属性类型 → 只列属性名，保证仍是合法 JSON（以前会在 JSON 中间截断）。

---

## 插件系统

插件提供可扩展的命令和工具，内置插件市场。

### REPL 命令

```
/plugin list       # 列出已安装插件
/plugin install    # 安装插件
/plugin remove     # 卸载插件
```

---

## 后台任务与异步执行

### 异步任务架构

Cove 的 REPL 支持异步任务执行：

- **主 REPL** 循环中，用户输入被转换为任务放入队列
- 后台 goroutine 取出任务异步执行
- 用户可以在当前任务执行时继续输入：输入的文本**作为指引送进当前任务**，下一次模型调用时生效，回车后提示 `[已插入] 已作为指引送入当前任务，下一步模型调用时生效`；连续输入多条会一起送入。若任务在用到指引之前就**正常完成**了，未生效的指引会自动作为新任务排到队首执行，并提示 `[已排队] 当前任务已结束，刚插入的指引将作为新任务执行`；若任务是因 `/stop`、Ctrl+C、撞上迭代/时间上限、出错或内部异常而结束，指引保留在原任务上，提示 `[已保留] 刚插入的指引未生效，将在 /continue 继续或下一次模型调用时送入`，`/continue`（或“继续”）恢复该任务时随第一次模型调用送入。带附件（`/attach`）的消息不能作为指引，仍排队等待。空闲时输入的指令直接开始执行，不打提示
- `/tasks` 查看运行中和排队任务（仅交互式 REPL 维护队列）
- `/stop` 取消当前任务（仅交互式 REPL；headless 为同步执行）

### 运行中插入指引

任务运行时直接输入并回车，就像在旁边给正在干活的助手递纸条：文本会在下一次模型调用前以 `[用户指引]` 消息追加进对话，模型据此调整后续步骤，不会打断正在执行的工具调用。典型用法：“别改测试文件”“先看 internal/api 目录”“用表驱动测试”。多条指引在同一次模型调用前会合并成一条消息，按输入顺序排列。

指引不会静默丢失：如果任务在下一次模型调用前就正常完成了，这些指引会作为一条新任务立刻开始执行——这时它是独立请求，模型看不到“上一任务的中途”这一语境，如有需要可再补一句说明；如果任务是被取消、撞上限、出错或内部异常结束的，指引留在该任务上，`/continue` 恢复时一起送入，模型能看到中断前的上下文。`exit` 退出时未生效的指引会直接丢弃。

`/tasks` 会在当前任务下方显示 `待生效指引: <预览>`；固定输入行的行末显示 `已插入 N 条指引`，模型消费后清零。以 `/` 开头的命令、授权提示与提问工具的回答不走这条路径，行为不变。

### 任务运行中的输入行

任务运行时，输入行固定在终端**最后一行**，上方一条暗色横线把它和输出流分开。输入行显示 `⚡ ❯` 与正在输入的内容（光标位置以反色标出），空白时提示“任务运行中，可直接输入指引，回车后送入当前任务”；有未生效指引时行末显示 `已插入 N 条指引`，有排队任务时显示 `已排队 N 条`，两者同时存在时用 ` · ` 连接；模型输出和工具结果只在横线上方滚动，不会把输入行刷走。回车后输入的内容会回显到上方的输出流里，便于在记录中看到插入了什么。

实现上用的是终端自己的滚动区域（DECSTBM）加一次光标位置查询（`ESC[6n`），不靠程序数行，所以折行、宽字符、spinner 都不影响定位。以下情况自动退回旧行为（运行期间不显示输入行，回车后只排队不回显）：终端不回应光标位置查询（超时 300 ms；一次未应答后暂停 30 秒再试，连续 3 次未应答才在本进程内停用，之后收到任何应答即恢复）、stdout 不是终端、窗口不足 8 行、设置了 `COVE_PIN_INPUT=0`。授权提示出现时输入行临时回到输出流中答题，答完再钉回底部。

长时间运行的 `bash`/`powershell` 命令的实时输出，会先打一行 `▸ bash <命令>  实时输出:`，输出内容缩进在它下面；命令结束后照常出现工具摘要行（`✓ Command: … · 共 N 行`）。

### 任务合并

当排队任务与新输入的内容相似或重叠时，系统会自动合并任务，避免重复执行。任务合并只作用于排队中的任务（例如带附件的消息、`/continue` 恢复的任务）；运行中输入的普通文本作为指引送入当前任务，不参与合并。

### 持久化任务队列

运行中任务和待执行消息会原子保存到配置目录的 `task-queues` 子目录，保留项目目录、会话 ID 和附件内容。存储失败时队列暂停，不启动后续任务，并显示错误。正常完成且队列为空后删除对应快照。headless 按行同步执行，不维护该队列。

重启仅提示未完成队列，不自动重放任务。恢复步骤：先 `/tasks saved` 获取队列 ID 和会话 ID，再 `/resume <会话ID>` 切回原会话，最后 `/tasks restore <队列ID>`。仍属于活跃进程的队列不能接管，其他项目或会话的队列也不能恢复到当前会话。恢复后保持暂停。

| 命令 | 说明 |
|------|------|
| `/tasks remove <序号>` | 删除待执行任务；序号来自 `/tasks`，不删除运行中的任务 |
| `/tasks move <序号> <目标序号>` | 调整待执行任务顺序，并保存新顺序 |
| `/tasks retry` | 明确确认重试状态不明的任务，并继续队列 |
| `/tasks skip` | 放弃状态不明任务，队列仍暂停 |
| `/tasks run` | 确认启动已暂停的待执行队列 |

崩溃或退出前的当前任务可能已修改文件或产生其他副作用，标记为状态不明。请检查已有结果后选择 `retry` 或 `skip`；Cove 不保证任意外部操作的恰好一次执行。切换会话不会把原队列带入新会话，原快照仍可切回原会话后恢复。

### 任务验收证据

`/acceptance` 展示当前会话最近一次任务的验收条件、命令退出码、耗时和输出尾部。报告原子保存在配置目录的 `acceptance` 子目录，切回原会话后可查看。每个会话只保留最新报告，后续任务不会继承前一任务的通过状态。

验收命令来自已有 `done_verify_commands` 配置和自动检测的构建/测试，沿用原有信任检查、超时和重试门禁，不会新增未经授权的命令执行。已通过的结果、失败的结果，以及因超时、取消、未信任项目或前序失败而未验证的项目分别展示。复用本轮最后一次文件修改后的成功证据会单独注明；校验后再次修改工作区会使旧通过结果失效。

模型回复“完成”不等于验收通过。没有执行证据时报告显示未验证；命令通过也不代表业务目标全部满足。本版只记录可执行命令证据，产品效果、视觉质量等主观条件仍需人工验收。

### 失败重试

任务因上限、Ctrl+C 或 API 错误中断后，提示“输入 /continue 可从中断处继续刚才的任务。”，输入 `/continue` 即可续跑（见[/continue](#continue)）；也可以像以前一样输入 `继续` 或 `continue` 来重试。

### 中断草稿保存

任务开始时即把输入保存为中断草稿，任务正常完成后自动清除。因此请求失败、被 Ctrl+C 中断或进程异常退出（崩溃、断电、被杀）后，下次在同一项目目录启动时会提示草稿，输入“继续”回到该草稿所在的会话接着做（请求不会重复写入会话）。草稿只在保存它的项目目录中提示和使用；在其他项目或其他会话中完成对话不会清除它；在同一项目开始新任务会替换它。任务运行期间 `/history detail interrupted` 显示的错误一栏为“任务未完成（cove 在任务运行中退出）”。`-p`/headless 模式不保存草稿。

---

## 单轮上限与 /continue

一轮（从你发出一条消息到模型给出最终回答）中模型会反复调用工具。为防止失控，每轮有三道上限。交互模式下到达上限时**暂停询问**，而不是立即中止本轮；`-p` 模式没有人回答，按硬上限处理。

| 上限 | 配置 | 默认 | 交互模式 | `-p` 模式 |
|------|------|------|---------|----------|
| 迭代上限 | `max_iterations` | 200 次模型调用 | 询问 `[c] 继续 N 次  [s] 停止` | 硬上限：`--max-turns N` 覆盖配置，`0` 不限制；到达即以退出码 1 结束 |
| 时间上限 | `max_turn_minutes` | 交互模式 60 分钟（`0` 关闭） | 询问 `[c] 继续 N 分钟  [s] 停止` | 默认不限时；只有配置（`config.json`、`.cove.json` 或 profile）显式写了 `max_turn_minutes` 才生效，到达即报错结束 |
| 停滞检测 | — | 连续 60 次迭代没有文件读写 | 询问一次 `[c] 继续  [s] 停止`，之后本轮不再问 | 只记日志，不中断 |
| 循环检测 | — | 本轮第 2 次命中 L1/L2 循环 | 询问 `[c] 本轮禁用循环检测并继续  [s] 停止`（见[循环检测第二次询问](#循环检测第二次询问)） | 注入引导，累计 5 次硬停 |

headless（`--no-tui`）模式与 `-p` 相同：迭代上限为硬上限，时间上限只在显式配置时生效，停滞只记日志。

询问示例：

```
  ⏸ 本轮已调用模型 200 次，达到单轮上限
  已调用模型 200 次 · 用时 12m30s · 本轮费用 $0.8421
  最近步骤: read → grep → edit → bash → read
  [c] 继续 200 次  [s] 停止
```

- 回答 `c`（或 `继续`、`y`）再给一个同样大小的窗口，本轮状态不丢；其他任何输入或 Ctrl+C 都视为停止。迭代或时间窗口用掉 80% 时，模型会收到一次剩余额度提醒（见[预算提醒](#预算提醒80)）。
- 15 分钟无人回答也按停止处理，并提示“等待超时，已按停止处理（此后单独输入 c 会作为新消息发送）”。
- 选择停止时，模型先在不调用工具的前提下给出一段收尾总结（见[收尾总结](#收尾总结)），然后本轮结束。
- 停止后已完成的工具步骤及其结果保留在会话里，交互模式只显示一行停止说明（不带 “Request failed” 前缀，也不再重复 /continue 提示）：

  ```
  本轮已停止：已达到单轮最大迭代次数 200。输入 /continue 可继续；可通过配置 max_iterations 调整
  本轮已停止：已达到单轮时间上限 60 分钟。输入 /continue 可继续；可通过配置 max_turn_minutes 调整（0 为不限制）
  ```

  输入 `/continue` 即可从中断处续跑。

`-p` 到达上限时 stderr 输出（退出码 1）：

```
Error: 已达到单轮最大迭代次数 200（可用 --max-turns 调整）
Error: 已达到单轮时间上限 30 分钟（可配置 max_turn_minutes 调整，0 为不限制）
```

headless 模式显示 `已达到单轮最大迭代次数 N（可配置 max_iterations 调整）`。

### /continue

- 上一轮因上限停止、Ctrl+C（提示“[已中断] 正在停止当前任务…输入 /continue 可继续”）或 API 错误等中断后，在交互模式输入 `/continue`：重发同一条用户消息，引擎从中断处续跑，已完成的工具步骤不会重做。
- 没有被中断的回合时提示“没有可继续的回合”；当前有任务在运行时提示等它结束。
- headless（`--no-tui`）模式不支持 `/continue`。

### 子智能体上限

`agent` 工具与 `execute_plan` 的每个子智能体最多调用模型 `subagent_max_iterations` 次（默认 60）。到达上限时不丢弃已做的工作，而是返回部分结果：以 `Sub-agent did not finish: 已达上限（N 次模型调用），以下为部分结果` 开头，随后是已完成的步骤列表（工具名 + 主要参数，失败的会标注）和最后一次模型输出。`execute_plan` 中的任务遇到这种情况时标记为失败，但保留部分结果并出现在计划汇总里（“部分结果:”），不再整体重试。

**结构化退出原因**：每个子智能体的结果带退出原因 `exit_reason` 与 `truncated`：

| `exit_reason` | 含义 |
|------|------|
| `completed` | 正常完成 |
| `max_iterations` | 到达 `subagent_max_iterations`；此时 `truncated` 为是（输出是部分结果） |
| `interrupted` | 用户取消、5 分钟超时或预算耗尽 |
| `loop` | 陷入循环被检测器停下 |
| `error` | 出错 |

- `agent` 工具结果的首行：`[exit: <reason>, steps: N, truncated: yes/no]`。
- `execute_plan` 汇总：每个未正常完成的任务行尾附 `[exit: <reason>]`，并在 `Total:` 行之前加一行汇总（只列非零项），如 `汇总：3 个任务完成，1 个到达上限（含部分结果），1 个陷入循环，1 个已中断，1 个失败，1 个跳过`。

### 回合结束摘要行

交互模式下，回合结束时如果后台工作有值得说的事，会打印一行淡色摘要，例如：

```
已提取 2 条记忆 · 新增技能 go-release · 已建检查点，/undo 可回退
```

只在以下情况出现：

- 本轮提取到记忆（“已提取 N 条记忆”）；
- 后台回顾学到了新技能或更新了自动技能（“新增技能 X、Y”、“更新技能 X”，见[技能审查](#技能审查-background-review)）；
- 本轮改了文件且检查点创建成功（“已建检查点，/undo 可回退”）；
- dream 为 `threshold` 模式时门槛有变化（开始整理或还差的会话数/小时数变化；启动后的第一个回合只记录基线，不显示）；默认的 `session_end` 模式不在这里显示；
- `max_sessions` 自动清理删除了旧会话（“已清理 N 个旧会话（max_sessions=200）”）；
- 会话保存失败（“会话保存失败（详见日志）”）。

显示时机：摘要在回合之间到达时立即显示；如果到达时下一轮已经开始，则等该轮输出结束后再显示，不会插进回答中间。`-p`、headless 以及输出不是终端时不显示。

---

## 完成判定与收尾

cove 与 gemini-cli、Codex CLI、OpenCode、goose 等开源 Agent 的基准判定相同：**模型这一轮回复里没有工具调用，就算准备结束**，不要求模型调用“完成”工具。区别在于结束前还要过几道门，以及被迫停下时怎样收场。这些机制主要是为 DeepSeek 等较弱模型准备的：它们常说“接下来我将修改 X”却没有真的去做，或在做完工作后只回一句话。

所有每轮上限集中定义在 `internal/engine/nudges.go`。

### 停后自检

模型给出不带工具调用的回复时，引擎按以下顺序检查；命中任何一项，就把对应提示作为一条合成用户消息写入历史，再调用一次模型：

| 顺序 | 检查 | 触发条件 | 注入给模型的文案 | 每轮上限 |
|------|------|---------|-----------------|---------|
| 1 | 空回复 | 没有可见文本、也没有工具调用（只有思考/推理内容也算） | `[system: Your response was empty. Provide the answer or call a tool.]` | 2 次（空回复本身不写入历史） |
| 2 | 宣告未做 | 最后一段里有一句以“宣告下一步”开头（见下） | `[system: You announced a next step but did not perform it. Continue now by calling the required tools, or state clearly that the task is complete.]` | 与退化结尾合计 2 次 |
| 3 | 退化结尾 | 本轮运行过非只读工具（写文件、执行命令等），最终文本少于 40 个字符（含中文时 20 个字），且不含完成词 | `[system: Your last message is too brief to be a final answer after doing work. Summarize what you changed for the latest request and what remains, or continue.]` | 与宣告未做合计 2 次 |
| 4 | 完成前自检 | 本轮写/改过文件，且 `done_check` 生效（见下节） | `[system: Before finishing, check whether the user's latest request has been fully met. If anything remains, continue working now; if everything is done, reply with the final answer about that request.]` | 1 次 |
| 5 | 验证门禁 | `done_verify_commands` 或自动推断的校验命令（见[配置字段说明](#配置字段说明)） | 校验失败时打回继续修改 | — |

- 超过上限后，模型的回复原样作为最终答复。
- 以下情况一律不注入：已取消、预算已用尽、下一次调用会撞上迭代上限或时间上限（提示本身绝不会导致回合被停）。
- 注入的提示都会走正常的下一次模型调用：计费、计入迭代数。

**“宣告下一步”如何判定**：只看最后一段，按换行和 `. 。 ! ！ ; ；` 切成句子，标记词必须出现在句首或行首才算：中文“接下来、下一步、现在我来、现在让我、让我、我这就、我现在就、我将、现在开始、然后我”，英文 `Let me`、`I'll`、`I will`、`Next,`、`Now I'll`、`I'm going to`、`Then I'll` 等（整词匹配）。以下情况不算：以问号结尾；出现第二人称（`you`/`your`、“你”）；含 `please`、`confirm`、`once`、`let me know`；含否定（`not`、`won't`、`don't`、`can't`、`cannot`、`never`，“我将不、不会、不再”）；含“建议、如需、如果需要、是否需要、如有问题、请、等待、确认”；含完成词（“完成、已提交、已修复”、`done`、`finished`、`complete`、`committed`）；最后一段超过 300 字。因此“需要我继续吗？”“建议下一步运行测试”“已完成全部修改。”都不会触发。

**“退化结尾”只看非只读工具**：只读了文件（`read`/`grep`/`glob` 等）之后简短作答（如“文件 a.go 第 12 行是 return nil。”）不算退化结尾。

### 完成前自检 (done_check)

配置 `done_check`：

| 值 | 行为 |
|----|------|
| `auto`（默认；空值或未知值也按 auto） | 本轮使用的模型属于快速档（名字含 flash/mini/lite/tiny/fast/haiku/nano），或 provider 不是 anthropic（DeepSeek 等）时启用 |
| `on` | 总是启用 |
| `off` | 关闭 |

启用时，只在本轮写/改过文件的情况下，模型第一次准备结束时注入一次自检提示（界面显示暗色“（自检中…）”，两段回复之间空一行）；模型第二次准备结束时不再自检，无论它回答了什么，直接进入验证门禁。`config.json` 与项目 `.cove.json` 都能设置，profile 暂不支持。

### 验证门禁跳过已通过的命令

如果门禁命令本轮已经由模型用 `bash`/`powershell` 成功跑过，而且之后没有可能改动文件的操作，门禁不再重复执行它，直接记为通过（摘要里标 `passed earlier this turn`）。

- 匹配：空白规范化后完全相等；`a && b` 这类组合命令只匹配它自己。
- 成功：结果里不含 `[exit code:`、`Error`、`[timed out`、`[cancelled]`，也不以 `BLOCKED` 开头。
- 证据失效：之后运行了任何非只读工具（`write`、`edit` 等），或一条既不是只读、也不是构建/测试类的 shell 命令。只读工具不影响。
- 证据只在当前回合内有效，`/continue` 续跑从零开始记。

### 收尾总结

回合不是因为模型自己说完而停下时，引擎再调用一次模型，要求它不调用工具、用用户的语言、只针对最近这条请求总结：已完成什么、还剩什么、建议的下一步，在 git 仓库里还要说明改动是否已提交、已推送。提示为：

```
[system: The run is stopping (<reason>). Without calling tools, summarize in the user's language, for the latest request only: what was completed, what remains, and the recommended next step. In a git repository, say whether the changes are committed and pushed.]
```

- 触发：迭代/时间上限（交互模式选 `[s]`，以及 `-p`/headless 撞硬上限）、停滞询问选停止、循环检测询问选停止、循环检测累计 5 次硬停。
- 这次调用不带工具定义（anthropic provider 例外：历史里有 tool_use 时 API 要求带 tools，提示照样要求不调工具，回复中的工具调用会被丢弃），`max_tokens` 1024，关闭思考，使用本轮实际使用的模型；计费。
- 总结作为一条助手消息写进历史（提示本身不保留）。交互模式流式显示；`-p` 与 headless 在打印错误前先把总结写到 stdout，退出码与错误不变（仍是上限错误，退出码 1）。
- 已取消、预算已用尽、回放模式（`--replay`）或调用失败时静默跳过。

### 只汇报当前请求

同一会话里连做几个互不相关的任务时，最终报告只讲最近这条请求：

- 系统提示词要求报告只针对最近的请求；之前的请求已经汇报过，只有本次接着做或改动了那部分工作时才提。上面各条补充提示（退化结尾、完成前自检、收尾总结）也都限定在“最近的请求”。
- **只有本回合写过的任务列表才会“追着”模型**：`todowrite` 的列表在整个会话里保留（你回答模型的提问、`/resume` 之后的第一句话都不会丢掉计划）。回合结束前“还有 N 项未完成，做完再完整写一遍报告”的提示，只在**本回合调用过 `todowrite`** 时出现；本回合没碰过的旧列表不会强迫模型接着做、也不会被写进新任务的报告。每 8 轮的提醒对这种旧列表会注明“这是对话前面留下的列表，只有最近的请求属于那项工作时才继续”。

### 提交与推送状态

- 系统提示词要求：在 git 仓库里改了文件时，报告必须说明改动是否已提交、已推送；只有真的执行且成功了才能说已提交/已推送，否则明确写“未提交”“未推送”；列给用户执行的命令要说明是给用户的步骤，不是已经做了的事。
- **引擎给出的状态行**：不依赖模型怎么说。本轮用 `write`/`edit` 改过文件，或运行过会改变仓库状态的 git 命令（`add`、`commit`、`push`、`pull`、`merge`、`rebase`、`reset`、`checkout`、`switch`、`restore`、`rm`、`mv`、`cherry-pick`、`revert`、`clone`、`init`、`am`、`apply`、`clean`，以及 `stash`/`tag`/`branch` 的修改用法；失败或超时的 `git push` 同样算），且工作目录在 git 仓库里时，最终报告（或收尾总结）下方显示一行暗色的 `git status` 结果，例如：
  - `git：3 个文件有未提交的改动`
  - `git：改动已提交，2 个提交未推送到 origin/main`
  - `git：改动已提交，分支 feature 没有上游，未推送`
  - `git：工作区干净，与 origin/main 同步（按本地记录）`
  - `git：工作区干净，没有未推送的提交，落后 origin/main N 个提交（按本地记录）`
- 只读的 `git log`、`git diff`、`git status`、`git show`、`git fetch` 不会触发这一行。`/usr/bin/git`、`"C:\Program Files\Git\bin\git.exe"` 这类带路径的写法也能识别。
- “未推送/落后的提交数”按本地记录的上游分支计算（上次 fetch/push 时的状态），不访问网络；`git status` 超过 3 秒或失败时不显示。

### 中断标记

回合被中断时（Ctrl+C 取消、预算耗尽、API 错误、迭代/时间上限、循环、停滞），在历史里写一条提示，让模型下次先核对现场：

```
[system: The previous turn was interrupted (<reason>). Commands may have partially executed and files may be half-edited; re-check state before repeating work.]
```

`<reason>` 为 `user cancel`、`budget exhausted`、`API error`、`iteration limit`、`time limit`、`loop detected`、`stagnation`，其余为 `stopped`。同一次中断只写一条：`/continue` 续跑后再次中断不会重复写；某一轮正常完成、或你发出不同的新请求之后，下次中断再写新的。原有的中文续跑提示（“上一次执行被中断…”）保留。

### 循环检测第二次询问

仅交互模式：一轮内第 2 次非致命的循环命中（L1 或 L2，见[工具循环检测](#工具循环检测)）时暂停询问：

```
  ⏸ 检测到重复操作，模型可能在原地打转
  已调用模型 37 次 · 用时 4m12s · 本轮费用 $0.1203
  最近步骤: bash → bash → bash → bash → bash
  <检测器给出的原因>
  [c] 本轮禁用循环检测并继续  [s] 停止
```

- `c`：本轮关闭 L1/L2 检测（L3 停滞询问照旧），这批工具调用照常执行；下一轮自动恢复。
- `s`：先生成收尾总结，再结束本轮（“检测到操作循环，已按你的选择停止本轮”），可以 `/continue`。
- `-p`/headless 不询问，行为不变：每次命中注入引导，累计 5 次硬停。

### 预算提醒（80%）

- **给模型的窗口提醒**：迭代窗口或时间窗口用掉 80% 时，在本批最后一条工具结果末尾追加一次 `[budget: about N model calls / M minutes remain in this window. Prioritize converging on a result; do not stop solely because of this notice.]`（只开了一种上限时只报一种）。每个窗口最多一次；选“继续”开新窗口后可以再提醒一次；正好用完的那一刻不提醒。`-p` 下同样生效。
- **给你的费用提醒**：会话费用达到 `max_budget_usd` 的 80% 时，终端提示一次 `费用已达预算的 80%：$x / $y（到达上限将停止；/budget <金额> 调整本会话上限，/budget off 取消上限）`；改预算后重新计算。

### 权限拒绝文案

工具调用被拒绝时，模型收到的工具结果在原因之后统一追加一句下一步指引，避免弱模型用同样的参数反复重试：

```
Error: permission denied for <tool> (<reason>). Do not call this tool again with the same input; explain the situation to the user or choose a different approach.
```

用户拒绝时 `<reason>` 为 `user rejected`；`policies.json` deny 规则拒绝（`denied by policy for <tool>.`）和“没有交互审批处理器”的拒绝也追加同一句。

---

## 计划执行器 (Plan Executor)

Plan Executor 是 Cove 的核心高级功能之一，支持**声明式多步骤任务执行**。

### 工作流程

1. Agent 使用 `todowrite` 工具创建结构化任务列表
2. 每个任务可声明依赖（`depends:task-1,task-2` 前缀）
3. Agent 调用 `execute_plan` 工具执行计划
4. Plan Executor 分析依赖关系，生成拓扑排序的执行级别
5. **同级别的独立任务并行执行**（默认最多 4 个并发子智能体，`max_agents` 可设 1–8）
6. 依赖任务失败时，下游任务自动标记为「跳过」
7. 失败的任务自动重试 1 次；子智能体因到达 `subagent_max_iterations` 而失败的任务不重试，保留部分结果并在计划汇总中显示“部分结果:”

### 依赖声明语法

在 `todowrite` 的 `content` 字段中使用 `depends:` 前缀：

```
depends:task-1,task-2 实现用户登录功能
```

### 并行执行

`execute_plan` 工具的 `parallel` 参数控制是否并行执行：
- `true`：同级别任务并发执行
- `false`：所有任务串行执行

---

## 子智能体与团队协作

### 子智能体 (Sub-Agent)

通过 `agent` 工具，Agent 可以生成子智能体处理独立的子任务：

- 支持的类型：`general`（通用）、`explore`（探索代码）、`plan`（计划）、`review`（审查）、`test`（测试）、`verify`（独立反向验证）
- 子智能体拥有受限的工具集
- 子智能体沿用当前会话的权限模式，每次工具调用都经过与主会话相同的授权检查
- 每个子智能体最多调用模型 `subagent_max_iterations` 次（默认 60），超时 5 分钟；到达上限时返回已完成步骤的部分结果并标注（见[子智能体上限](#子智能体上限)）
- 通过 `delegate.Delegator` 管理生命周期

### 独立回归验证

`verify` 子智能体使用独立对话上下文，沿用当前供应商、模型、预算和权限门。按需调用，不会默认在每轮结束时运行，也不改变 `done_self_review`。它只获得代码阅读工具和 `regression_verify`，不能改文件、写测试、执行通用 Shell、联网或再派生子智能体。

第一版支持 Go。先用 `/checkpoints` 找到修复前的本项目检查点，再对 Cove 说：

```text
用 verify 子 Agent 独立验证这次修复。
基线检查点：<修复前的 hash>
生产文件：internal/foo/value.go
测试包：./internal/foo
测试选择器：^TestValueRegression$
检查测试是否确实能检出旧实现的问题，不能只报告当前测试通过。
```

工具输入为 `baseline`（7–64 位十六进制检查点哈希）、`files`（1–32 个现存普通生产 `.go` 文件，不接受 `_test.go`）、`package`（项目相对包路径）和 `run`（Go 测试正则）。工具用旧源码构建 Go overlay，固定执行两次 `go test -json -count=1 -timeout=60s`，不把旧文件恢复到工作区。每次进程最多 2 分钟，子智能体仍受 5 分钟和模型调用次数上限约束。

仅当旧实现退出码为 1 且出现真正的具名测试失败事件，同一测试在当前实现上通过、当前进程退出码为 0，才记为通过。两边都通过表示测试不能区分这个修复；编译失败、没有跑到测试、跳过、超时、取消或执行期间检查点捕获的工作区状态变化都不能算通过。模型没有执行工具就宣称成功，同样返回 `unverified`。

基线哈希、当前状态哈希、选中文件、测试范围、退出结果和有界输出会加入 `/acceptance`，保存在现有会话验收报告中；后续文件修改或回滚使成功证据失效。通过只证明选定测试对选定旧源码有辨别力，不证明需求全部正确，也不能替代对断言、缺失测试和其他改动的代码审查。

**这不是沙箱**：Go 测试和构建可能写文件、读取环境或访问网络，必须信任项目并按权限模式授权。测试没有被复制或冻结，执行期间不要同时编辑项目；检查点忽略的文件、外部依赖和短暂后又撤回的修改不在状态比对保证范围内。Cgo 等未按 Go overlay 重定向的外部源码不支持完整反向还原。暂不支持 Python/.NET/JavaScript 的机械反向证明。

### 团队 (Team)

通过 `team_create` 创建智能体团队并行工作：

- 定义团队成员及其各自的任务
- 通过 `send_message` 在任务/团队间发送消息
- 消息支持定向投递（任务 ID / 团队 ID / 广播）

---

## 后台学习

Cove 在对话过程中自动提取记忆、学习技能，并在对话结束后整理记忆。`--no-auto` 关闭全部后台学习；所有后台模型调用都计费并受 `max_budget_usd` 约束。

**本地模型例外**：provider 的 `base_url` 指向本机（`127.0.0.1`、`localhost`、`[::1]`、`host.docker.internal`）时，每轮结束后的记忆提取、对话复盘和 dream 整理都不运行。本地服务一次只处理一个请求，这些调用会把你的下一轮排在后面，而且 30 秒超时在本地大模型上几乎必超。会话保存与自动清理照常。

### 记忆提取 (Extract)

**每个回合结束后都会运行**（不再有时间节流，快速会话的最后几个回合也不会被漏掉），用后台（fast）模型分析最近 20 条消息，把值得长期保存的事实写入记忆。保留的保护：

- 上一次提取还在运行时，本回合跳过；
- 自上次提取以来没有新消息时跳过（压缩使历史变短也算新历史）；
- 少于 4 条消息不提取。

每次提取（包括没找到值得保存内容的）都会记录时间和条数，可用 `/memory stats` 查看；交互模式下提取到记忆时会显示在[回合结束摘要行](#回合结束摘要行)里。`cove -p` 回答后最多等待 20 秒让本轮提取完成再退出。

- 新记忆写入**项目记忆目录**（见[记忆系统](#记忆系统)）。追加到已有记忆时，单个文件超过 10KB（或 200 行）就滚动写入 `name-2.md`、`name-3.md`……，不再截断丢掉新内容；与文件末尾完全相同的行不重复追加；只在全局目录存在的同名记忆，以全局内容为底写入项目目录，全局文件不动。
- 所有写入都经过统一检查（注入检测、单条 25KB、记忆总量上限 300KB）。

### 记忆溯源

手动 `/memory add` 和自动提取的新写入会保存来源记录，可用 `/memory source <名称>` 查看。记录包含写入类型、来源会话与项目；自动提取还记录当时实际使用的最近 20 条消息的位置、输入 SHA256 和最多 4KB 的依据片段。消息位置是该次提取输入中的编号，不是永久不变的会话文件行号；压缩或清理历史后，原始输入可能已无法完整读取。

元数据保存到记忆目录的隐藏 `.provenance` 子目录，按记忆名和正文 SHA256 绑定，不改正文、不计入记忆提示，也不会注入模型上下文。来源保存失败时不改正文。追加时保留已有来源；原正文没有记录时明确标为未知。每个正文版本最多保留最近 64 条来源记录，不提供逐字逐事实归因。删除记忆时同步清理它的来源历史。

溯源仅覆盖持久记忆文件，不为项目指令文件维护会话来源。旧记忆，以及被外部编辑或 Dream 改写成新内容的记忆不会自动继承旧来源，显示未知。来源片段可能含对话内容，请按敏感本地数据保管；来源只是写入依据，**不代表事实已经核实**。本版尚不提供记忆禁用、固定或来源修订界面。

### 本会话记忆即时生效

- 本回合提取出的新记忆（或旧记忆新追加的行），在**下一回合**以 `<session_memories>` 块（≤ 2KB，只出现一次）附在你的消息之后，不必等下一次启动；系统提示词不重建。
- 记忆总量超过 24KB（系统提示词只列索引，见[记忆注入](#记忆注入)）时，每回合还会按你的消息做 BM25 检索，附上 `<relevant_memories>` 块（≤ 4KB，按条目截断，同一记忆在会话内只注入一次）。
- 两者合并成一个 `<turn_memories>` 块，总长 ≤ 6KB。上下文压缩后清空重计（重建的系统提示词已包含全部记忆）。

### 技能审查 (Background Review)

后台回顾对话，把可复用的做法沉淀为技能（**只生成技能**，记忆由上面的提取负责）：

- 至少 6 条消息、距上次回顾有 4 条以上新消息、距上次回顾至少 3 个回合，且本回合用过非只读工具时触发，30 秒超时；预算用尽时跳过。`cove -p` 不做回顾。
- 技能写到 `~/.cove/skills/auto-<slug>-<名字哈希6位>/SKILL.md`（frontmatter：`name`、`description`、`generated`、`source_session`），下次启动自动加载。
- **不覆盖你的技能**：目标文件不是自动生成的（没有 `generated:`）或 name 不同时跳过；已有同名的非自动技能（用户、项目、插件、内置）时不注册；再次学到同名的自动技能则更新文件。
- 摘要行显示“新增技能 X、Y”或“更新技能 X”。

### 记忆整理 (Dream)

4 阶段记忆整合：**Orient**（收集记忆文件）→ **Gather**（分析关联和冗余）→ **Consolidate**（合并和重组）→ **Prune**（清理过时记忆）。全局记忆目录和当前项目的记忆目录都会被整理。

配置文件 `dream.json`（位于配置目录：设置了 `COVE_CONFIG_DIR` 时为该目录，否则 `~/.cove`）：

| 键 | 默认 | 说明 |
|----|------|------|
| `enabled` | `true` | 总开关 |
| `trigger` | `"session_end"` | `session_end`：对话结束时整理；`threshold`：旧行为，回合结束时检查时间与会话门槛。大小写不敏感，未知值警告并用默认 |
| `min_turns` | `2` | 仅 `session_end`：本次进程里成功完成的回合数至少为此值才整理 |
| `min_interval_minutes` | `0` | 仅 `session_end`：距上次整理至少这么多分钟才再次整理；`0` 不限 |
| `min_hours` | `12` | 仅 `threshold` |
| `min_sessions` | `3` | 仅 `threshold` |

**`session_end` 模式（默认）**：

- 回合结束时不再检查 dream。退出时（`/exit`、`/restart`、Ctrl+D、`-p` 结束、headless 结束，在 SessionEnd hook 跑完之后）检查：dream 已启用、本次进程完成的回合数 ≥ `min_turns`、上次整理后有会话被修改（含本会话）。`--no-auto` 与 `--replay` 时跳过。
- 满足条件时以**分离的后台进程**整理，stderr 显示“已在后台启动记忆整理（约 1–3 分钟，至多约 30 次后台模型调用，结果见 /dream）”，cove 立即退出。后台进程继承工作目录（项目 `.cove.json` 的模型配置同样生效），使用 `model_fast`（未设则 `model`），最长 5 分钟，调用计费并写入费用历史。
- 后台进程启动失败时在当前进程内整理，最多 60 秒，每秒打印一个进度点；超时则取消并回滚整理锁。
- 后台进程的输出追加到配置目录下的 `dream.log`（超过 1MB 时滚动为 `dream.log.1`）；结果写入 `dream-last.json`（`mode`、`pid`、`started_at`、`finished_at`、`result`=running/completed/failed/skipped、`error`、`sessions_reviewed`、`files_touched`、`input_tokens`、`output_tokens`、`cost_usd`）。进程崩溃或被强杀时，下一次检查会把记录改为失败并回滚锁。

**`threshold` 模式**：距上次整理至少 `min_hours` 小时，且之后至少有 `min_sessions` 个新会话（正在使用的会话不计入）；整理在回合结束的记忆提取之后进行；`cove -p` 中不自动整理；退出时正在进行的整理被取消并回滚锁。

```
/dream            # 同 /dream status
/dream status     # 是否启用、距上次整理、触发方式（session_end 显示“上次整理后的新会话: N 个”，threshold 显示门槛还差多少）、上次会话结束整理的结果、正在整理或上次运行结果、上次整理用量（输入/输出 token 与费用）
/dream run        # 忽略门槛，立即在后台整理
```

`/dream run` 仍要求 dream 已启用并拿到整理锁；锁被另一个 cove 进程或刚完成的整理占用时提示“整理锁被占用”，稍后再试。后台整理进程异常退出时，`/dream` 显示“异常退出…详见 dream.log”。

### 记忆去重

新记忆与现有记忆相似度 >80% 时自动合并。

### 会话笔记 (Session Notes)

基于正则识别对话中的决策和发现，保存到 `~/.cove/projects/<hash>/session_notes.md`（旧位置 `<项目>/.cove/session_notes.md` 自动迁移并删除，`.cove/` 为空时一并删除；新旧文件都在且内容不同时保留旧文件不动）。

- 决策：“决定、采用、改用、换成”，以及分句开头的“用”（“用户、用例、用法、用途、用于、用来、用以”不算）；发现：“发现、原因是”。关键词前两个字里有“没、未、不、无、你、由”时不记录；记录内容至少 4 个字；同类同文本去重。
- 不再记录 `File: x` 和工具错误条目。时间格式 `2006-01-02 15:04`，加载时保留。

---

## 上下文与提示词

### 系统提示词：项目轮廓

系统提示词里不再放整份代码库地图，只放一个 **≤ 4KB 的 `<project_outline>`**（本仓库实测系统提示词从 51.7KB 降到 13.1KB，纯聊天回合不再为代码地图付费）。轮廓内容全部排序、不含时间戳，同一棵目录树每次生成的字节完全相同，不破坏 prompt 缓存；只在构建系统提示词时（会话开始、压缩后）生成一次：

- 第一行固定是使用提示：符号请用 `repo_map` 工具、`grep`、`glob` 查询（仓库里没有 Go/Python/TS/JS 源码时改为提示用 `grep`/`glob`）
- `Languages:` 按文件数排序的语言统计（前 8 种）
- `Top-level dirs (files):` 顶层目录及文件数；`Top-level files:` 根目录文件
- `Source dirs (files):` 二级源码目录（如 `internal/engine (92)`）
- `Entry points:` `main.go`、`main.py`、`index.ts`、`main.rs`、`Program.cs` 等（深度 ≤ 4）
- `Build/test:` 由 `go.mod`、`package.json` scripts、`Cargo.toml`、`pyproject.toml`、`pom.xml`、gradle、`.sln`/`.csproj`、`CMakeLists.txt`、`Makefile` 目标推导出的命令

跳过点目录与 `node_modules`、`vendor`、`testdata`、`build`、`dist`、`target`、`bin`、`obj`、`__pycache__`、`venv`；超长列表以 `… +N more` 收尾，整体超过 4KB 时按行截断。启动时不再扫描文件树和代码地图。

### 代码大纲按需查询

**`repo_map` 工具**（只读、无需确认，所有模式都注册，含 `-p` 与 headless）：

| 参数 | 说明 |
|------|------|
| `query` | 可空。空格或逗号分隔的路径片段或标识符，如 `"engine turnContextNote"`；不区分大小写，匹配文件路径与符号名 |
| `path` | 可空。只查该目录（相对工作目录，不能越出工作目录） |

- 两者都空时列出被引用最多的文件。
- 输出 ≤ 12KB：每个文件一行 `路径 (package x):`，下面是类型/函数/方法签名与 `:行号`；匹配的符号排前，每文件最多 30 个；排序为匹配度 > 非测试文件优先（`query` 含 test/spec 时不降权）> 引用排名 > 路径。被截断时末尾一定有 `… N more matching files`。
- 只支持 Go、Python、TypeScript、JavaScript（`.go`、`.py`、`.ts`、`.tsx`、`.js`、`.jsx`、`.mjs`、`.cjs`）。其他语言请用 `grep`/`glob`；没有匹配时工具也会这样提示。

**首个任务回合自动附带一次摘录**：每个会话只有一次（`/cd` 切换目录后重新计数），在第一个“像任务”且能提取出检索词的回合，把 `repo_map` 的查询结果以 `<repo_map_excerpt>`（≤ 12KB）附在你的消息之后（不进入系统提示词）。

- “像任务”：含文件路径、函数调用 `name(`、camelCase/snake_case 标识符、反引号代码、关键词（修复、实现、重构、报错、测试；英文 fix、implement、refactor、error、bug、test 按整词匹配，`testimony` 不算），或长度 ≥ 200 字符。
- 检索词取自消息里的路径、反引号代码和 ASCII 标识符（≥ 3 字符，去掉常见英文停用词）。**纯中文消息提取不出检索词**，即使命中“测试”“实现”等关键词或很长，也不注入、不占名额；“你好”之类的闲聊同样不注入。
- 查询结果为空时不注入，也不计次。

### 指令文件

启动时从当前目录向上到 git 根目录，每一层按 `CLAUDE.md`、`.claude/CLAUDE.md`、`AGENTS.md`、`.cove.md` 的顺序加载（根目录在前，越靠近当前目录越靠后），按路径和内容去重后合并注入系统提示词。不在 git 仓库里时只看当前目录。合计上限 32KB，超出部分截断，末尾标注 `... [truncated: instruction files exceed 32KB]`，终端同时打一行淡色提示“项目指令文件超过 32KB，已截断（保留前 32KB）”（每会话一次）。`/memory list` 把它们标为 `(指令文件)`。

### 子目录提示

模型操作到某个子目录（读写该目录下的文件，或 `grep`/`glob` 的 `path` 参数指向它）时，该目录及其上层尚未见过的 `AGENTS.md`、`CLAUDE.md`、`.cursorrules`、`.cove.md` 会作为提示附在工具结果后面。单个文件上限 12KB，一次工具调用最多注入 24KB，放不下的留给下一次调用。上下文压缩后重置，会重新显示；按文件类型注入的自定义技能同样重置。

### 记忆注入

- 记忆总量 ≤ 24KB 时，全部记忆全文放进系统提示词。
- 超过 24KB 时，系统提示词只列记忆索引 `<memory_index>`，索引本身上限 4KB，超出部分以 `- … N more saved memories (list the memory directory to see them all)` 收尾（被截掉的记忆仍能被每回合的相关记忆检索找到）；每回合按你的消息用 BM25 检索相关记忆，以 `<relevant_memories>` 附在消息之后（见[本会话记忆即时生效](#本会话记忆即时生效)）。

### 上下文压缩

上下文大小以服务商返回的真实 `input_tokens` 为准（上次请求的输入 token + 之后新增消息的估算；没有可用数据时按 system prompt + 工具定义 + 全部消息估算）。达到触发点时自动压缩：触发点 = min(0.75 × 压缩预算, 窗口 − 回复预留 − 8K 安全余量)，其中压缩预算 = 模型上下文窗口 × 0.85，回复预留 = 窗口的 1/4（限制在 4K–64K 之间）。例如 200K 窗口的模型约在 127.5K token 时压缩，64K 窗口为 40K，1M 窗口为 637.5K。无法确定模型时使用固定阈值（64K token）。每次请求的输出上限（max_tokens）也按模型收紧为 min(64K, 模型输出上限, 窗口/4)，且不低于 4K（例如 200K 模型为 50K，deepseek-chat 为 8192）。

- **保留原始需求**：摘要输入中，第一条真实的用户消息保留 2000 字符，其余用户消息 600、助手 250、工具结果 100 字符；摘要提纲的第一项是“用户的原始需求”，压缩后模型不会忘了最初要做什么。
- 自动压缩时终端提示一行：`已压缩上下文：X → Y tokens（已摘要早期对话 / 已裁剪旧工具输出 / 原因）`。

手动压缩：

```
/compact           # 立即压缩（不看阈值，强制摘要早期对话；至少 4 条消息）
```

输出三种之一：

- `已压缩：压缩前 X tokens → 压缩后 Y tokens。`
- `仅部分压缩（原因）：压缩前 X tokens → 压缩后 Y tokens。`（只裁剪了旧工具输出，摘要失败）
- `未压缩：原因（当前 X tokens）。`（例如消息太少）

### 工具结果去重

同一个大文件被反复 `read`/`cat`、或输出相同的测试反复运行时，历史里会出现多份完全相同的工具结果。每次检查上下文时，第 2 份及以后的副本替换为存根：

```
[identical to earlier tool result for call <tool_call_id> (<n> bytes); content omitted]
```

- 条件：工具结果 ≥ 512 字节、内容完全相同；最近 4 条工具结果不动；`question`/`todowrite`/`plan_mode`/`exit_plan_mode` 与已落盘的占位不参与。消息条数和 tool_call_id 不变。
- 为了不频繁破坏 prompt 缓存，只有两种情况下才真正替换：本轮的旧输出落盘遮蔽本来就要改写历史；或者这批替换累计能省下至少 2000 token（估算）。
- 大工具输出落盘在 `~/.cove/tool-outputs`，超过 7 天的文件在启动时删除；因此恢复较早的会话后，其中引用的旧工具输出已无法再读取。
- 无配置开关。

---

## 护栏与安全

### 工具循环检测

系统内置**三层循环检测**机制，防止 AI 陷入无限循环浪费 Token：

| 层级 | 检测方式 | 窗口 | 阈值 | 说明 |
|------|---------|------|------|------|
| Layer 1a | 精确工具指纹匹配（工具名+参数哈希） | 14 轮 | 10 次 | 检测完全相同工具调用 |
| Layer 1b | 模糊工具名匹配 | 12 轮 | 10 次 | 检测同一工具不同参数 |
| Layer 2 | 输出内容哈希 | 40 轮 | 8 次 | 检测相同输出重复 |
| Layer 3 | 停滞检测（无文件读写） | 60 轮 | — | 交互模式询问一次是否继续（见[单轮上限](#单轮上限与-continue)），`-p` 与 headless 只记日志 |

**响应机制**：
- Layer 1/2 前 5 次检测到循环 → 注入引导消息，要求 AI 换思路，自动清空检测窗口
- 交互模式下，一轮内第 2 次命中时暂停询问 `[c] 本轮禁用循环检测并继续 / [s] 停止`（见[循环检测第二次询问](#循环检测第二次询问)）
- 超出 5 次 → 先生成收尾总结，再硬终止当前回合，返回错误
- 只读工具（`read`/`grep`/`glob`/`webfetch`/`task_list`/`skills_list`/`skill_view`）豁免检测（`browser` 的 `screenshot` 会写文件，不算只读工具）
- Flash 模型使用更敏感的阈值（8/12, 8/10, 8/30, 50）

### 幂等结果检测

检测重复的相同工具输出，防止无限循环。

### 并行执行保护

- 并行工具调用上限：8 个
- 工具 panic 时（无论并行还是串行执行）都转成该调用的 `Error: tool panicked: …` 结果，不会使进程崩溃
- 并行子智能体上限：4 个

### 路径安全

文件操作受路径安全检查，禁止访问系统敏感路径。

### URL 安全

`browser` 和 `webfetch` 工具会检查 URL 安全性，阻止访问私有/内部地址（含 `0.0.0.0/8`、组播 `224.0.0.0/4`、保留段 `240.0.0.0/4`、`ff00::/8`、NAT64 `64:ff9b::/96`）。`198.18.0.0/15` 只在 URL 里直接写 IP 时拒绝——Clash/Surge 等 TUN 模式的 fake-IP DNS 会把所有域名解析到这一段，按解析结果拒绝会让这类机器上的抓取全部失败。

使用 `-tags chromedp` 构建的无头 Chrome 会拦截页面发出的每个请求（子资源、重定向、fetch/XHR），私网或非 http(s) 目标一律失败；`data:`/`blob:`/`about:` 放行。已知限制：跨进程 iframe、Worker 与 WebSocket 的请求不经过拦截。

### 请求重试

API 请求失败重试时，退避时间为 `基准 × 2^n × [0.5, 1.5)` 的随机值；服务端要求的等待时间（依次读取 `retry-after-ms`、`Retry-After`（秒数或 HTTP 日期）；只有 429 响应才会再读取 `anthropic-ratelimit-*-reset`，且只看剩余额度为 0 的限额；上限 60 秒）不会被缩短，5xx 响应按正常退避处理。Anthropic 非流式请求超时为 300 秒；请求超时后不再重试（请求已发出、可能已计费），连接被拒绝、被重置等其他传输错误仍会重试。

### 速率限制

内置 API 速率限制追踪（`/ratelimit` 查看状态）。

### 工具失败熔断

同一个工具 30 秒内连续失败时快速失败，按工具名分别计数：某个工具成功只清除它自己的失败记录，提示文案带工具名。

---

## 检查点与回退

### 自动检查点

在执行 `write` 或 `edit` 操作前（整批工具调用开始之前），系统自动创建 Git 快照作为检查点。快照存放在 `~/.cove/checkpoints/store`，每个项目有独立的历史，不会写入项目自己的 Git 仓库；项目的 `.gitignore` 会被遵守。内容与上一个检查点相同时不会重复创建。

### 手动操作

```
/checkpoints       # 列出当前项目最近的检查点
/undo              # 回退到上一个与当前状态不同的检查点；连续执行会一步步往前回退
/undo <commit>     # 回退到指定检查点（只接受当前项目的检查点）
```

回退会恢复检查点里的文件内容，并删除检查点之后新建的文件。回退前的状态会先自动备份，输出里会给出撤销这次回退的命令（`/undo <备份>`）。

`bash`/`powershell` 命令执行前也会创建检查点，所以 `rm`、`sed -i`、代码生成器造成的改动同样可以回退；明确只读的命令（`ls`、`cat`、`git status`、`git diff` 等）不创建。

本轮改了文件且检查点创建成功时，[回合结束摘要行](#回合结束摘要行)显示“已建检查点，/undo 可回退”。

### 选择性文件回滚

先用 `/checkpoints` 选定当前项目的目标，再预览文件：

```text
/undo files <commit> src/main.go "path with spaces.txt"
/undo apply <预览ID>
/undo cancel
```

预览不会改工作区，只保存当前备份和选中文件的状态。确认令牌绑定本次预览、项目及会话，最多有效 10 分钟；切换会话或项目、重启进程、重新预览都会使旧确认不可用。预览只支持普通文件，不接受目录、符号链接、嵌套仓库、忽略/未纳入备份的文件或越界路径。

确认前会复核全部选中文件的内容、存在性和权限；与预览不一致则整批拒绝。执行时只恢复选中文件，目标中不存在的选中文件会删除，未选中文件不动；写入不扩大现有文件的读取权限。不要在执行过程中同时编辑这些文件：多个文件并非一个文件系统事务，途中失败可能部分生效，此时输出仍提供回滚前备份的 `/undo <备份>` 供恢复。整树 `/undo` 和文件级回滚都会使当前会话的旧验收通过证据失效。

这是文件级回滚，不是逐行接受/撤销修改；预览前已经存在的用户修改会被纳入预览与备份，明确确认后也可能被回滚。

### 保留策略

- 每个项目保留最近 **50** 个检查点：超过 60 个时在后台一次裁到 50 个，撤销备份链同样裁剪。裁剪在后台进行，不阻塞创建检查点。
- **被保留的检查点会重建在新的根上，哈希随之改变**。因此以前记下的哈希（包括回退输出里给出的 `/undo <备份>`）在下一次裁剪后可能失效，`/undo <旧哈希>` 会提示“不属于当前项目”；用 `/checkpoints` 查看当前哈希。
- 每创建 20 个检查点，在后台运行一次 `git gc --quiet`（不带 `--prune=now`，不会删掉正在写入的对象），超时 2 分钟；失败或超时只记警告日志。

---

## 会话管理

### 会话保存

会话自动保存到 `~/.cove/sessions/`，每个会话会记录启动 cove 时所在的项目目录（会话元数据中的 `cwd` 字段）。

| 文件 | 内容 |
|------|------|
| `<id>.jsonl` | 一个会话。第 1 行是元数据（`format`、`version`、`id`、`created_at`、`updated_at`、`title`、`model`、`tokens_in`、`tokens_out`、`cost`、`cwd`），之后每行一条消息（JSON） |
| `index.json` | 会话列表索引（标题、目录、轮数、消息数、预览、时间、模型、用量、文件大小与修改时间） |
| `<id>.json` | 旧版格式（整个会话一个 JSON）。仍可加载和列出；该会话下次保存时自动迁移为 `.jsonl` 并删除旧文件 |

- 每次保存只**追加**新增的消息并原子更新 `index.json`；历史被整体替换（压缩、文件被外部修改）时整文件原子重写。
- 列会话只读 `index.json`，不解析消息体；索引与文件大小/修改时间对不上（例如崩溃或外部编辑）时自动重扫该文件修复索引，`index.json` 丢失时自动重建。
- 追加中途崩溃留下的半行在加载时跳过，下次保存时被清掉。
- `cove -r <id>` / `--resume` 接受 `<id>`、`<id>.jsonl` 或 `<id>.json`。
- 首行元数据在标题、模型、目录变化时随下一次保存重写；token 与费用最多每 16 次保存刷新一次（`index.json` 始终是最新值，首行只是索引丢失时的后备）。
- **自动清理**：每个项目（按启动目录的 git 根分组；没有记录目录的旧会话自成一组）只保留最近 `max_sessions` 个会话（默认 200），更早的在回合结束时自动删除；当前会话永不删除，每个进程最多每 10 分钟清理一次。设为负数关闭自动清理。有删除时回合结束摘要行显示“已清理 N 个旧会话（max_sessions=200）”，进程内第一次删除还会记一条警告日志（含删除数量与配置键）。**升级注意**：默认值 200 在升级后第一个回合结束时就会生效，要保留全部历史请事先设置 `"max_sessions": -1`。

### 按项目区分的历史

`/history`、`/resume`、`cove --list-sessions` 以及单独输入“继续”（或 `continue`，可带句末标点）时自动恢复最近任务——“继续把 README 翻译成英文”这类更长的句子按普通消息发送，任务运行中则作为指引送入当前任务，默认**只列出当前目录（项目）的会话**，避免把其他代码库的对话恢复到当前项目、让模型混淆文件路径。目录比较前会规范化为绝对路径；在 Windows 上不区分大小写（`D:\Proj` 与 `d:\proj` 视为同一项目）。

需要查看所有项目的会话时加上 `all`：

```
/history all               # 列出所有项目的会话（每行标注所属目录）
/history all <编号>        # 按 all 列表的编号恢复
/history all detail <编号> # 按 all 列表的编号查看详情
/resume all                # 列出所有项目的会话 ID
cove --list-sessions all   # 命令行列出所有项目的会话
```

执行 `/history all` 后直接输入编号，按的是 all 列表的编号；执行 `/history` 后则按当前项目列表的编号。

旧版本 cove 保存的会话没有记录目录，无法判断属于哪个项目，因此不出现在按项目的列表中（否则每个项目都会看到它们），但文件不会被删除，仍可在 `all` 视图中看到（标注为“旧版会话，未记录目录”）并恢复。列表中有被隐藏的会话时，会提示隐藏的数量和 `all` 用法。

按会话 ID 恢复（`/resume <id>`、`/history <id>`）不受项目限制；如果该会话属于其他目录，恢复时会给出提示，显示会话目录和当前目录。

### 会话恢复

```
/resume            # 列出当前项目可恢复的会话
/resume <id>       # 恢复指定会话（可跨项目，会提示）
/history           # 查看当前项目的历史会话
/history <编号|id> # 恢复历史会话并美化显式
/history delete <编号|id>   # 删除一个会话
/history clear              # 清空当前项目历史（显示数量，需再输入 /history clear confirm）
/history clean              # 修复历史文件，不删除
```

#### 🛡️ 历史记录智能降噪
Cove 的会话管理具备低信噪比排除算法。当会自动为您保存的会话生成标题和摘要预览时，任何诸如单独的通用命令行启动指令（例如：`write`、`read file`、`grep`、`cd`、`git commit`等），都会被自动判定为“低信息噪音标题”而丢弃。系统会自动向后寻检并精确蒸馏首句真实的 User 提问语义作为替代标题，确保历史菜单一目了然。

#### 🎨 渐进式多轮色彩还原
在交互式 REPL 中加载历史会话时，系统不再以一两行简单的“已恢复”来掩盖状态。控制台会**无感温和重绘最近的 4 轮交互历史**：
- **用户（User）指令**：以高饱和彩色、富有留白的层次显示。在系统内置微调时生成的 `[system:` 前缀底层通知则自动低亮隐藏。
- **助手（Assistant）**：完美梳理出的逻辑行文直接打印。
- **核心工具（Tool）调用链**：树状追溯所有调用工具（如 `edit`、`bash`）时传入的具体参数与经过剪裁压缩处理的返回结果（拒绝直接刷屏 200 行日志，精准截断）。

极大地唤醒了开发者的短期记忆，确保从上次中断的地方无缝衔接。

### 会话导出

```
/export            # 导出当前对话为 Markdown
```

### 上下文压缩

见[上下文与提示词 · 上下文压缩](#上下文压缩)。`/compact` 可随时手动压缩。

---

## 记忆系统

### 持久记忆

记忆分两处存放，加载时合并（同名时项目优先）：

| 位置 | 内容 |
|------|------|
| `~/.cove/projects/<hash>/memory/` | **项目记忆**：新记忆（自动提取与 `/memory add`）写在这里。`<hash>` 由项目根（向上找到的 git 根，否则当前目录）计算，目录权限 0700 |
| `~/.cove/memory/` | **全局记忆**：只读合并；旧版本写在这里的记忆不迁移，照常生效 |

设置了 `COVE_CONFIG_DIR` 时以上目录都在该目录下。

```
/memory add <名称> <内容>   # 添加记忆（写入项目记忆；同名记忆只在全局存在时，以全局内容为底写入项目目录）
/memory list               # 列出所有记忆：共 N 条，每条附大小与首行摘要（60 字），标注 (项目) / (全局) / (指令文件)
/memory search <关键词>    # 关键词检索
/memory remove <名称>      # 删除一条记忆
/memory stats              # 条数、总大小、使用率、单条上限、其中指令文件，以及“上次提取: 时间，保存 N 条”（无记录时“尚无记录”）
```

自动提取的记录保存在 `~/.cove/memory/.last-extraction.json`（隐藏文件，不作为记忆加载）。

### 记忆特性

- 单条上限 25KB，总量上限 300KB；自动追加超过 10KB 时滚动写入 `name-2.md` 等（见[记忆提取](#记忆提取-extract)）
- 总量 ≤ 24KB 时全文注入系统提示词，超过时注入索引 + 每回合 BM25 检索（见[记忆注入](#记忆注入)）
- 嵌入向量存储（可选 `memory_embedding`）
- 自动提取和去重；对话结束时整理（见[记忆整理](#记忆整理-dream)）
- 跨会话持久化

---

## 费用追踪

### 实时追踪

- 每次 API 调用的 token 使用和费用实时计算
- 不同模型的计费标准不同；价目表未收录的 `claude-*` 型号按 Opus 单价（$15 / $75 每百万 token）估算，宁可高估也不少算，其他未知型号按默认单价（$0.435 / $0.87）估算
- Anthropic 的 prompt cache 写入（`cache_creation_input_tokens`）按输入价的 1.25 倍计费
- 达到预算上限时自动暂停并提示

### 查看费用

```
/cost               # 查看本次会话费用
```

显示信息包括：
- 本次会话 token 数和费用
- 近 1 天（24h）总费用
- 近 7 天总费用
- 历史总会话数和总费用

### 预算管理

```
/budget             # 查看当前预算（本会话）
/budget 5           # 本会话预算设为 $5（不写配置）
/budget auto        # 按历史使用自动调整本会话预算（不写配置）
/budget off         # 取消本会话的预算上限
/budget save        # 把当前会话预算写入 config.json
```

- `/budget` 的各种设置**只影响本会话**；要长期生效用 `/budget save`，或 `/config budget <n>`（直接写配置）。
- 费用达到预算的 80% 时提示一次：`费用已达预算的 80%：$x / $y（到达上限将停止；/budget <金额> 调整本会话上限，/budget off 取消上限）`；改预算后重新计算。
- 到达上限时暂停，所有模型调用（主循环、子智能体、记忆提取、压缩、收尾总结、记忆整理）都计入。

---

## 诊断系统

### 诊断码体系

30+ 诊断码，按子系统分组：

| 码段 | 类别 | 范围 |
|------|------|------|
| E1xxx | 配置 | API Key、配置文件、模型名、权限模式 |
| E2xxx | API / 网络 | 不可达、超时、限流、认证、400、5xx、流中断；**E2008 上下文超出模型窗口**、**E2009 供应商已被标记不可用** |
| E3xxx | 权限 | 拒绝、无授权回调、policies.json 无法加载、文件访问 |
| E4xxx | 工具 | 未注册、超时、崩溃、shell 缺失、Git Bash/WSL、git 缺失；**E4009 工具参数非法 JSON** |
| E5xxx | 引擎 | 迭代上限、取消、压缩、崩溃、后台整理失败；**E5007 模型调用无进展** |
| E6xxx | 会话 / 文件系统 | 会话损坏、保存失败、权限、磁盘空间 |

运行期错误在产生处**按类型归类**得到诊断码（HTTP 状态码、上下文超长、限流、传输错误、工具参数非法、卡住、供应商不可用），不是靠错误文本猜；归不了类的错误按原文记录、无码。

### 使用

```
/doctor             # 快速检查：git、ripgrep、供应商与 API key，外加“后台学习”“权限规则文件”
/diagnose           # 完整诊断（含网络检测）
/diagnose quick     # 快速检查（跳过网络）
/diagnose errors    # 查看运行时记录的错误/卡顿：按诊断码和模型聚合，显示次数、最近时间、建议、已执行的处置
/diagnose archive   # 处理完后归档错误日志，开始新的记录周期（处置器的计数一并清零）
/diagnose trace [N] # 查看最近 N 条交互轨迹（默认 30）：每次模型调用的消息数、估算 token、耗时、结束原因或错误类别，每次工具调用的耗时、结果大小、是否出错，每次压缩的前后 token 数，超长重试的处理
```

交互轨迹写在 `~/.cove/trace.jsonl`（超过 4MB 轮转为 `trace.jsonl.1`），只记录大小、名称和错误开头，不记录消息正文。任务"跑了很久没结果"时，先看它。

```
/diagnose codes     # 列出所有诊断码，带处置器的标注 (有处置器)
```

`/doctor` 与 `/diagnose` 都包含以下两项（读取当前会话的实际状态）：

- **后台学习**：dream 摘要（是否启用、门槛还差多少、上次运行结果）+ 记忆条数与大小 + 上次提取时间与条数；上次整理失败时警告 E5006。
- **权限规则文件**：`policies.json` 无法读取或解析时报错 E3004——此时文件中的规则（包括 `deny`）都不生效。

`/diagnose` 与 `/diagnose quick` 末尾还会提示网络搜索未配置 `web_search` 或缺少对应 API key。

### 自动处置

运行期错误归类后记入 `~/.cove/errors.log`；`/diagnose errors` 按诊断码和模型聚合显示。部分诊断码带**处置器**：错误发生时自动执行并提示一行（`⚙ …`），只改本会话的运行策略与参数，不改代码、不改 `config.json`，并且只有真的执行了才在记录里写一条「已处置」：

- **E2008 上下文超出模型窗口**：从服务端报错里解析真实窗口（llama.cpp 的 `n_ctx`、`context size (N tokens)`、OpenAI 的 `maximum context length is N`、Anthropic 的 `N maximum`），小于 cove 按模型名估算的值时当场改为该值，并写入 `config.json` 的 `model_context_windows`，下次启动直接生效；后续压缩按真实窗口触发。超长时的重试真正会变小：历史太短、压缩器无从摘要时，改为移除本轮附加的 repo map 摘录与记忆注入、裁剪最大的工具结果，再重试一次；重发同一请求时「上一次执行被中断」标记只保留一条，不再每次多一条。压缩（摘要或截断）后的第一条消息始终带 `<original_request>` 原始需求原文，模型不会在不知道任务是什么的状态下继续。
- **E4009 工具参数非法 JSON**：同一模型第 3 次出现时提示换模型或降低 temperature，之后每 5 次提示一次。

静态检查（`/diagnose`）里只有「会话完整性」会自动清理损坏的会话文件，其余检查给出建议。

### 启动时诊断

`diagnostic.QuickCheck()` 在启动时自动运行，检测常见配置问题；此外若错误日志里有带建议、且未被处置的错误，提示一行「错误日志里有 N 类未处理的问题，输入 /diagnose errors 查看建议」。

---

## 附件功能

### 在 REPL 中

```
/attach image.png           # 挂载图片
/attach config.json         # 挂载文件
/attach list                # 列出附件
/attach remove image.png    # 移除附件
/attach clear               # 清除所有附件
```

### 在 -p 模式

```bash
cove -p "分析这张图" --image screenshot.png
cove -p "审查配置" --file config.json
```

### 内联 @ 语法

在 REPL 或 `-p` 消息中使用 `@路径` 自动挂载：

```
解释这张图 @assets/screen.png
审查这个文件 @src/main.go
```

---

## Git 集成

### 提交

```
/commit "feat: add login feature"    # git add + commit
/commit                              # 自动生成 Conventional Commit 消息
```

### 审查

```
/review             # 审查未暂存的变更
/diff               # 显示 git diff
```

### 工作树

Agent 可通过 `worktree` 工具创建隔离的 Git 工作树，适合大规模重构。

---

## CovePhone (Android)

CovePhone 是 Cove 的 Android 手机伴侣应用。

### 要求

- Android 8.0 (API 26) 或更高
- 网络连接
- 支持的提供商 API Key（如 DeepSeek）

### 安装

1. 从 [Releases](https://github.com/liuzhixin405/cove-agent/releases) 下载 APK
2. 允许安装未知来源应用
3. 打开 APK 完成安装

### 设置

1. 启动 CovePhone
2. 进入设置（齿轮图标）
3. 输入 API Key
4. 选择模型和提供商
5. 返回聊天界面开始使用

### 特性

- **原生 Go 引擎**：与桌面版共用同一套 Go 模型接入层（`internal/api`：各提供商、流式解析、工具参数修复、连接重试），通过 `gomobile` 编译为 `cove-core.aar`；工具调用循环是移动端自己的轻量实现，不包含桌面版的工具系统和权限机制
- **Thinking 显示**：AI 思考过程带平滑滚动显示
- **持久化设置**：API Key、模型、提供商自动保存
- **多轮对话**：会话内完整聊天历史

### 问题排查

如果应用返回重复响应：
1. 检查 API Key 是否正确配置
2. 确保网络连接正常
3. 尝试切换模型
4. 重启应用

### 技术支持

- GitHub Issues: https://github.com/liuzhixin405/cove-agent/issues
- 邮箱: 164910441@qq.com

---

## 高级技巧

### 1. 利用计划模式

对于复杂变更，先输入要求进入计划模式 (`plan_mode`)，让 Agent 只读取和分析代码，生成完整计划后再执行。

### 2. 批量任务提高效率

利用 `todowrite` 一次性定义多个任务，然后 `execute_plan` 并行执行无依赖的任务。

### 3. 自定义技能

在 `~/.cove/skills/` 创建符合工作流的技能文件，让 Agent 在操作特定类型文件时自动加载。

### 4. 记忆管理

定期使用 `/memory list` 查看积累的记忆；记忆默认在对话结束时自动整理，`/dream` 查看结果，`/dream run` 立即整理。

### 5. 预算控制

设置合理的 `max_budget_usd`，或使用 `/budget auto` 让系统根据历史使用调整本会话预算（`/budget save` 写入配置）。

### 6. 附件而非复制

对于大型代码审查，使用 `--file` 参数或 `/attach` 命令而不是直接复制代码到对话中。

### 7. 浏览器工具

对于 JS 渲染的页面（如 Jira、Confluence），使用 `browser` 工具而非 `webfetch`；需要截图确认时使用 `screenshot` 动作。

### 8. Chrome Headless 模式

使用 `chromedp` 标签构建 Cove 可获得完整的 headless Chrome 支持：
```bash
go build -tags chromedp -o cove ./cli/cove
```

### 9. 🌲 代码大纲：轮廓常驻，符号按需

Cove 自带免 CGO、零外部依赖的代码大纲库（[internal/repomap/](../internal/repomap/)），解析 Go/Python/TS/JS 的类型、函数、方法签名，并按引用关系排名。它不再把整份大纲塞进系统提示词：

- 系统提示词只含 ≤ 4KB 的项目轮廓（语言、目录、入口、构建/测试命令）；
- 需要符号和行号时，模型调用 `repo_map` 工具按路径或标识符查询（≤ 12KB）；
- 第一个带路径、标识符或反引号代码的任务回合，自动附带一次 ≤ 12KB 的相关摘录。

详见[系统提示词：项目轮廓](#系统提示词项目轮廓)与[代码大纲按需查询](#代码大纲按需查询)。

### 10. ⚡ 本地文件改变热发现与 $mtime$ 动态防抖缓存

当外部（如 IDE、Git checkout 或编译器生成）或者 Cove 的辅助工具更改了工作区源代码时：
- Cove 将基于高并发 `RWMutex` 锁，自动追踪所有文件的绝对路径及最新修改时间戳（$mtime$）。
- 数据改变时感知线程无缝触发增量失效；在极短的时间窗口内对同一目标的连续改动做高效率增量防抖，无感通知大模型智能刷新或废弃过时提示词上下文。这极大节约了 API 资费。

### 11. 🔎 全网事实核查搜索引擎 Grounding 保护

Cove 具有双搜索引擎核查屏障：
- **搜索引擎联动检测**：当您配置了 `web_search`（见[配置字段说明](#配置字段说明)）或设置了环境变量 `TAVILY_API_KEY` / `BRAVE_API_KEY` 时，Cove 的网络搜索功能将并联激活对应 API，结合高保真 RAG 结果清洗，杜绝任何 SEO 引流垃圾数据。
- **动态兜底**：若缺失高级 API 密钥，系统将自动使用轻量且经过编码重构的 DuckDuckGo 作为防灾兜底抓取，始终向 AI 输送最干净、真实的联网第三方库与 API 信息，全时抗击大模型知识幻觉和死板记忆。
