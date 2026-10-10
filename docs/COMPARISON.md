# cove vs Claude Code（及其他）诚实对比

> 这里不写「cove 完胜」的营销话术。Claude Code 是当前最成熟、最稳定的终端编程 agent，这是事实。
> 本文要回答的是：**什么情况下你该选 Claude Code，什么情况下该选 cove**。

---

## 一句话定位

| | cove | Claude Code |
|---|---|---|
| 本质 | 单文件 Go 二进制，**多模型、国内模型优先**的终端代码助手——本质是写代码，顺带用文件/shell/浏览器等真实工具做电脑自动化 | Anthropic 官方出品，绑 Claude 模型的终端 agent |
| 谁在用 | 中文开发者、想用 DeepSeek/GLM/Kimi/Qwen 等国内模型、想要单文件零依赖的人 | 英文开发者、Anthropic 生态用户、要最成熟稳定方案的人 |

---

## 逐维度对比

| 维度 | cove | Claude Code | 说明 |
|------|------|-------------|------|
| **模型绑定** | 多提供商：Anthropic/OpenAI/DeepSeek 原生，GLM/Kimi/Qwen/Doubao/OpenRouter/SiliconFlow/Groq/Together/Fireworks/xAI/Mistral 等 12 个兼容 | 绑定 Claude（Anthropic），可用代理接第三方但非一等公民 | cove 的核心差异：国内模型是一等公民，不是「兼容接口凑合」 |
| **成本** | 用你自己的 key；可走 DeepSeek 等低价模型 | 用你自己的 key 或 Anthropic 订阅 | 用国内模型跑 agent 的单轮成本通常低一个数量级 |
| **分发** | 单文件静态二进制，零依赖，下载即用 | npm 安装，需要 Node 运行时 | cove 在「裸机/受限环境/容器」里更省心 |
| **权限模型** | 4 档（default/auto/bypass/plan）+ 项目级永久规则 | 细粒度权限 + 设置 | 两者都成熟，cove 对「只读默认放行」做得更开箱即用 |
| **工具集** | 文件/shell/PowerShell/grep/glob/web/browser（需 `-tags chromedp` 构建）/repo_map/MCP/skills/hooks | 文件/shell/grep/glob/web/MCP/skills/hooks | 能力相近，cove 多了 PowerShell、headless 浏览器、repo_map |
| **通用自动化** | 顺带的能力：因为是本地代码助手，自带文件、shell/PowerShell、headless 浏览器、网页抓取/搜索、MCP，所以也能操作电脑 | 有 Bash 等工具，定位以「写代码」为主 | 两者都能操作电脑；cove 的定位是「代码助手 + 顺带自动化」 |
| **多 agent** | 子 agent + 计划执行器（DAG）；团队协作为实验开关，默认关闭 | subagents（Claude 生态） | 都有，实现思路不同 |
| **记忆/学习** | **Dream 自学习**：自动提取记忆、跨会话整合、自动生成技能 | CLAUDE.md + 记忆（偏手动） | cove 的差异化亮点，但也因此更「玄」，需要用户信任 |
| **中文/国内体验** | 中文文档、国内模型适配、中文社区 | 英文为主 | 对中文开发者 cove 更顺手 |
| **成熟度/稳定性** | 单人维护，v12 但用户基础小 | 大厂持续迭代，社区反馈海量 | **这是 cove 最大的短板，不回避** |
| **生态** | MCP/skills/plugin 都有，但社区小、教程少 | 海量社区方案、教程、集成 | Claude Code 明显领先 |
| **数据自主** | 都本地跑、都用自己的 key | 同左 | 平手 |

---

## 什么时候选 Claude Code

- 你在 **Anthropic/Claude 生态**里，或公司已经统一采购。
- 你要的是 **最成熟、最少踩坑、社区方案最多** 的选择，不想折腾。
- 你需要大量 **英文社区** 的教程、插件、集成。
- 你能接受 npm 安装 + Node 运行时。

**Claude Code 是「风险最低」的选择，这点 cove 承认。**

---

## 什么时候选 cove

- 你想用 **DeepSeek / GLM / Kimi / Qwen / Doubao** 等国内模型跑 agent，而不是被绑在 Claude 上。
- 你要 **单文件、零依赖、交叉编译**，放进容器、受限服务器、U 盘里都能跑。
- 你在意 **成本**：低价国内模型能把单轮 agent 成本压到很低。
- 你对 **自学习记忆（Dream）** 这个方向感兴趣，愿意陪一个 solo 项目成长。
- 你是 **中文开发者**，想要中文文档和国内模型的开箱即用。
- 你在写代码之余，还想让它**顺带替你操作电脑**：跑 shell/PowerShell、抓网页、驱动浏览器、接 MCP 做点自动化。

**cove 是「更便宜、更自主、更中文」的代码助手（顺带还能操作电脑），但「更年轻、更不成熟」的选择。**

---

## 附带：cove vs Codex CLI / Aider

| | cove | Codex CLI (OpenAI) | Aider |
|---|---|---|---|
| 出品方 | 个人 | OpenAI | 社区（Paul Gauthier） |
| 模型绑定 | 多模型 + 国内模型 | 绑 OpenAI（Codex/GPT） | 多模型，接各家 |
| 形态 | 单文件 Go 二进制 | npm/单二进制 | Python（pip） |
| 特色 | Dream 自学习记忆、国内模型 | 强 agent 循环、OpenAI 深度集成 | 结对编程、git 原生、成熟稳定 |
| 适合 | 中文开发者、国内模型、零依赖 | OpenAI 生态重度用户 | 想要「可预测、可审查」diff 的结对体验 |

Aider 是另一个值得尊重的项目：它专注「结对 + git diff 可审查」，定位极清晰，社区成熟。**cove 的教训恰恰来自这里——定位太全反而模糊。** 这也是本文要把「什么时候不选 cove」写清楚的原因。

---

## 底线

- 拿 cove 和 Claude Code 比「成熟度」，是必输的——所以 **cove 不该在这条赛道上竞争**。
- cove 的立身之本是：**一个终端代码助手（本质写代码，顺带操作电脑）+ 国内模型一等公民 + 单文件零依赖 + 数据自主 + 中文体验 + Dream 自学习**。
- 如果你恰好命中这些需求，cove 是少数几个认真做的选项之一；如果命中不了，坦率地说，Claude Code 更好。

相关阅读：

- [为什么 cove 用 Go](WHY_GO.md)
