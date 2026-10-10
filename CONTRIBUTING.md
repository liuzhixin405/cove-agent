# Contributing to cove

感谢您对 cove 的关注！我们欢迎各种形式的贡献。

## 行为准则

请参阅 [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md)。

## 如何贡献

### 报告 Bug

1. 在 [Issues](https://github.com/liuzhixin405/cove-agent/issues) 中搜索是否已有相同问题
2. 使用 **Bug Report** 模板创建新 issue
3. 提供详细的复现步骤、期望行为和实际行为
4. 附上 `cove --doctor` 的诊断输出（如适用）

### 功能请求

1. 在 [Issues](https://github.com/liuzhixin405/cove-agent/issues) 中搜索类似请求
2. 使用 **Feature Request** 模板
3. 描述使用场景和期望的解决方案

### 提交代码

1. **Fork** 本仓库
2. 创建 feature 分支: `git checkout -b feature/my-feature`
3. 提交更改: `git commit -m 'feat: add some feature'`
4. 推送到分支: `git push origin feature/my-feature`
5. 提交 Pull Request

### Commit 规范

我们使用 [Conventional Commits](https://www.conventionalcommits.org/zh-hans/v1.0.0/)：

- `feat:` 新功能
- `fix:` 修复 bug
- `docs:` 文档更新
- `style:` 代码格式（不影响功能）
- `refactor:` 重构
- `test:` 测试相关
- `chore:` 构建/工具相关

### 开发环境

```bash
# 克隆仓库
git clone https://github.com/liuzhixin405/cove-agent.git
cd cove-agent

# 运行测试
go test ./...

# 构建
go build -o cove ./cli/cove

# 带 Chrome headless 支持的构建
go build -tags chromedp -o cove ./cli/cove

# 运行 linter
golangci-lint run
```

### 项目架构

```
cove/
├── cli/cove/           # 入口点 (main.go)
│   ├── main.go         # 主程序：启动引擎、REPL、配置
│   ├── chat_interaction.go  # 聊天交互核心逻辑
│   ├── repl_config_commands.go  # 配置类 REPL 命令
│   ├── repl_help.go    # 帮助和诊断信息
│   ├── repl_history.go # 会话历史管理
│   ├── repl_loop.go    # 交互式 REPL 主循环
│   ├── interactive.go  # 交互终端判定
│   ├── headless.go     # 非交互前端
│   └── repl_session_commands.go  # 会话命令
├── internal/
│   ├── automation/     # 维护任务（worktree 隔离、事件去重）
│   ├── api/            # AI 提供商接口（Anthropic/OpenAI/DeepSeek 等）
│   ├── browser/        # Headless Chrome 浏览器集成 (chromedp)
│   ├── checkpoint/     # Git 检查点管理
│   ├── command/        # REPL 命令注册表
│   ├── config/         # 配置加载/保存/迁移
│   ├── context/        # 项目上下文收集
│   ├── cost/           # 费用追踪
│   ├── delegate/       # 子智能体委托管理
│   ├── diagnostic/     # 诊断系统 (30+ 错误码)
│   ├── dream/          # 记忆整合系统
│   ├── engine/         # 核心引擎（对话循环）
│   ├── extract/        # 自动记忆提取
│   ├── guardrail/      # 护栏（循环检测、断路器等）
│   ├── race/           # 双 worktree 竞跑
│   ├── remote/         # 认证远程监督（HTTP）
│   ├── render/         # Markdown/diff/工具块渲染
│   ├── repomap/        # 符号级代码库地图
│   ├── hooks/          # 事件钩子系统
│   ├── log/            # 日志
│   ├── mcp/            # MCP 协议客户端
│   ├── memory/         # 记忆存储 (BM25 + 向量)
│   ├── notes/          # 会话笔记
│   ├── onboarding/     # 新用户引导
│   ├── permission/     # 权限管理 + 智能分类器
│   ├── plan/           # 计划执行器（DAG + 并行）
│   ├── plugin/         # 插件系统 + 市场
│   ├── repl/           # REPL 终端 UI
│   ├── session/        # 会话管理
│   ├── skills/         # 技能系统
│   ├── safety/ safepath/ safeurl/  # 危险命令、路径与 URL 守卫
│   ├── shell/          # shell 命令解析与只读判定
│   ├── termui/ uiout/ textmode/ textutil/  # 终端输出与文本工具
│   ├── workspace/ filelock/ fsatomic/ proctree/  # worktree、文件锁、原子写、进程树
│   ├── token/          # Token 计数
│   └── tool/           # 工具注册与实现 (20+ 工具)
│       ├── advanced_tools_*.go  # 计划/任务/团队/Agent/Worktree 工具（按主题拆分）
│       ├── extra_tools.go     # WebSearch/Question/TodoWrite 工具
│       ├── browser_tools.go   # Headless 浏览器工具
│       ├── skill_tools.go     # 技能工具
│       ├── mcp_tool.go        # MCP 工具桥接
│       └── ...
├── docs/               # 文档（使用手册等）
└── scripts/            # 一键验证入口 check.ps1
```

### 代码风格

- 遵循 Go 标准代码风格 (`gofmt`, `go vet`)
- 为导出函数/类型添加文档注释
- 新功能需包含测试
- 保持向后兼容性

### Pull Request 检查清单

- [ ] 代码通过所有测试 (`go test ./...`)
- [ ] 通过 lint 检查 (`golangci-lint run`)
- [ ] 添加了必要的测试
- [ ] 更新了相关文档
- [ ] 遵循 commit 规范

## 业务流测试台

单元测试之外，`cli/cove/testpage_test.go` 提供一个本地网页：

```bash
COVE_TESTPAGE=1 go test ./cli/cove -run TestPage -timeout 0
```

它复用 e2e 夹具（假模型按脚本应答、管道喂输入），左侧选场景，输入框填输入或点"随机输入"从场景候选里抽一个，运行后右侧显示真实终端输出（ANSI 还原）和每项检查的通过与否，顶部是通过数。场景定义在同一文件的 `tpScenarios`，每个场景是一个 Go 函数：起 REPL、喂输入、返回检查项。加场景就是加一个结构体。

## 许可证

贡献的代码将在 [MIT License](LICENSE) 下发布。
