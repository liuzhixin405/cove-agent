# Security Policy

## Supported Versions

| Version | Supported          |
| ------- | ------------------ |
| 12.x    | :white_check_mark: |
| < 12    | :x:                |

## Reporting a Vulnerability

如果您发现安全漏洞，请**不要**在公开的 issue 中报告。

请发送邮件至：**164910441@qq.com**（或通过 GitHub Security Advisory 私下报告）。

我们将在 48 小时内确认收到报告，并在 7 天内提供初始评估。

### 流程

1. 您提交安全漏洞报告
2. 我们确认并评估严重程度
3. 我们开发修复方案
4. 我们发布安全公告和补丁
5. 公开披露（在补丁发布后进行）

## Security Best Practices

使用 cove 时请注意：

- **API Key 安全**: 不要在代码中硬编码 API key，使用环境变量或 `~/.cove/config.json`
- **权限模式**: 生产环境建议使用 `default` 或 `plan` 模式
- **MCP 服务器**: 仅连接到受信任的 MCP 服务器
- **会话文件**: 不要分享包含 API key 的会话导出文件

