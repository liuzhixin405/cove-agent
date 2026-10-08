<div align="center">

# 🤖 Cove-Agent: 终端里的 AI 自动化专家

**像专家一样在终端里写代码。不再是简单的 AI 聊天，而是你的自动化代码执行引擎。**

[![CI](https://github.com/liuzhixin405/cove-agent/actions/workflows/ci.yml/badge.svg)](https://github.com/liuzhixin405/cove-agent/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/liuzhixin405/cove-agent?include_prereleases)](https://github.com/liuzhixin405/cove-agent/releases)
[![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

[中文](#中文) | [English](#english)

</div>

---

### 🚀 核心价值主张 (Key Value Propositions)

Cove-Agent 重新定义了 **AI 辅助编程**，将开发效率提升至自动化流水线级别：

* **⚡️ 真正的一等公民模型支持**：针对 **DeepSeek V3/R1, Qwen 2.5, GLM-4, Kimi, 豆包** 进行了底层 token 和推理路径的深度适配，成本极低，性能极致。
* **🛠️ 深度自动化引擎 (Plan Mode)**：基于 `Plan` 模式的复杂任务编排，让 AI 不仅能改代码，更能**自主规划、执行、测试、诊断**整个开发周期。
* **🧠 智能语境管理 (RepoMap)**：内置高性能代码上下文索引，AI 对你的仓库结构、依赖关系、符号定义了如指掌。
* **🖥️ 终端即是全能工作台**：内置 Shell、浏览器、文件系统操作，支持 MCP 协议，无需离开终端即可完成全链路开发。
* **🛡️ 隐私与安全**：单文件 Go 二进制，无依赖，所有 API Key 本地管理，数据绝不外泄。

---

### 📽️ 自动化演示

下面两张图是 cove-agent 在真实仓库里的会话——本仓库的代码就是这么写出来的。

![cove-agent 在终端里规划并执行任务](images/README/1790699695911.png)

![cove-agent 执行工具调用并汇报结果](images/README/1790699714465.png)

<a name="中文"></a>

## 💡 为什么选择 Cove-Agent？

**Cove-Agent 是专门为需要终端自动化开发体验的工程师打造的工具。**

它不仅仅是 `Claude Code` 或 `Aider` 的替代品，更是国产模型在编程领域落地的最佳实践。我们深入调研了大量开发者工作流，专注于解决“如何更聪明地在终端内完成任务”的问题：

### 核心特性关键词

- **#终端编程** (Terminal-based IDE)
- **#AI自动化** (AI Agent Workflow)
- **#代码重构** (Refactoring with AI)
- **#国产模型优化** (DeepSeek/Qwen/Kimi/GLM/Doubao)
- **#MCP支持** (Model Context Protocol)
- **#低延迟开发** (Low-latency dev workflow)
- **#单元测试自动化** (TDD with AI)

---

### 快速安装与使用

```bash
# 1. 下载对应平台的二进制（Windows / macOS / Linux）
#    https://github.com/liuzhixin405/cove-agent/releases

# 2. 或者从源码构建（需要 Go 1.25+）
go build -o cove ./cli/cove

# 3. 启动任务
cove /new "添加一个新的 API 接口并更新 README"
```

首次运行请在 `~/.cove/config.json` 中配置模型与 API key（可直接复制 [docs/config.example.json](docs/config.example.json)）。

---

### ⌨️ 命令速查

| 命令                  | 说明                                                            |
| --------------------- | --------------------------------------------------------------- |
| `/help`             | 显示帮助                                                        |
| `/keys`             | 查看输入快捷键（别名 `/shortcuts`）                           |
| `/model <名称>`     | 切换 AI 模型                                                    |
| `/provider <名称>`  | 切换提供商（anthropic/deepseek/openai/glm/kimi/qwen/doubao 等） |
| `/api-key <密钥>`   | 保存 API 密钥                                                   |
| `/base-url <地址>`  | 设置自定义接口地址                                              |
| `/mode <模式>`      | 设置权限模式（`default`/`plan`/`auto`/`bypass`）        |
| `/permissions`      | 查看当前权限模式                                                |
| `/profile [list       | switch                                                          |
| `/config`           | 查看完整配置                                                    |
| `/system <提示词>`  | 设置自定义系统提示词                                            |
| `/budget <金额        | auto                                                            |
| `/cost`             | 查看用量和费用                                                  |
| `/ratelimit`        | 查看 API 速率限制状态                                           |
| `/stats`            | 查看消息数与费用统计                                            |
| `/status`           | 查看代理状态与会话信息                                          |
| `/context`          | 查看当前上下文                                                  |
| `/compact`          | 立即压缩对话历史（打印压缩前后的 token 数）                     |
| `/new`              | 保存当前会话并开始新会话（清空对话上下文）                      |
| `/clear`            | 清屏并清空回滚区（别名 `/cls`，快捷键 Ctrl+L）                |
| `/history`          | 查看和恢复历史会话                                              |
| `/resume [id]`      | 恢复已保存的会话                                                |
| `/continue`         | 从中断处继续上一轮（已完成的工具步骤不会重做）                  |
| `/export`           | 导出当前对话                                                    |
| `/undo`             | 回退到上一个检查点                                              |
| 独立回归验证         | 要求使用 `verify` 子 Agent：Go 同一测试必须旧实现失败、新实现通过，证据见 `/acceptance` |
| `/undo files <检查点> <文件>...` | 预览文件级回滚，`/undo apply <预览ID>` 确认；执行前检测选中文件漂移 |
| `/checkpoints`      | 列出所有检查点                                                  |
| `/diff`             | 显示 git diff                                                   |
| `/commit [msg]`     | Git add + commit                                                |
| `/review`           | 审查工作区变更                                                  |
| `/init [apply         | discard]`                                                       |
| `/cd <路径>`        | 切换工作目录（按新目录重新加载 `policies.json` 规则）         |
| `/attach <文件...>` | 挂载图片或文件（支持 `list`/`remove`/`clear` 子命令）     |
| `/memory [list        | add                                                             |
| `/memory source <名称>` | 查看当前正文版本的来源会话、消息位置与提取依据                 |
| `/dream [status       | run]`                                                           |
| `/hooks`            | 列出从 `hooks.json` 加载的钩子                                |
| `/mcp`              | MCP 服务器管理                                                  |
| `/plugin`           | 插件管理                                                        |
| `/skill <名称>`     | 查看或调用一个技能（别名 `/skills`）                          |
| `/tools`            | 列出可用工具                                                    |
| `/tasks`            | 查看运行中/排队任务（TUI）；headless 显示同步执行状态           |
| `/tasks saved`      | 列出当前项目持久化队列；显式恢复、删除和排序，重启不自动执行 |
| `/acceptance`       | 查看最新任务的通过、失败、未验证项与命令执行证据               |
| `/automations`      | 显式维护任务：定时扫描、去重事件、Git worktree 隔离执行 |
| `/inbox`            | 查看维护结果、验证输出和补丁；接受只记录决定，不自动合并 |
| `/browser-verify`   | Chrome 实操断言、桌面/移动截图与验收证据；未启用 Chrome 时记为未验证 |
| `/race`             | 两个 worktree 竞跑、同一验证器比较；显式 select 应用通过方案 |
| `/remote`           | 显式启动认证远程监督；状态、引导、取消、队列暂停及一次性工具审批 |
| `/stop`             | 取消当前任务（别名 `/cancel`）                                |
| `/record [status      | start                                                           |
| `/x [编号] [all]`   | 展开工具块折叠的输出（别名 `/expand`）                        |
| `/doctor`           | 快速检查 git、ripgrep、供应商与 API key                         |
| `/diagnose [quick     | errors                                                          |
| `/trust`            | 信任当前项目的 `.cove.json`（启用其中的 MCP、provider 等设置）      |
| `/restart`          | 保存会话并重启 cove，重启后接着当前会话                         |
| `/exit`             | 退出 REPL                                                       |

*更多信息请查看 [贡献指南](CONTRIBUTING.md) 和 [开发文档](docs/README.md)。*

四项工作流的 JSON 示例、调度入口和安全边界见 [工作流指南](docs/guide/workflows.md)。默认不启动调度、浏览器或远程监听；维护与竞跑需要有提交记录的 Git 项目，worktree 不是安全沙箱。

<a name="english"></a>

## English

Cove-Agent is an AI-native Terminal CLI designed for high-performance development. It treats your local repository as its playground, leveraging advanced models like DeepSeek to provide deep, contextual, and autonomous coding assistance.
