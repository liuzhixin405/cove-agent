# Cove 文档目录

- [使用手册 (User Manual)](USER_MANUAL.md) — 完整的中文使用手册，覆盖所有功能
- [cove vs Claude Code](COMPARISON.md) — 诚实对比：什么时候选 cove，什么时候不选
- [为什么 cove 用 Go](WHY_GO.md) — 语言选型的全面分析（Go vs Python/TS/Rust/Java/C++）
- [需求说明书](需求说明书.md) — 从现有实现反向推导的产品需求说明
- [全流程图（端到端）](全流程图-端到端.md) — 总流程 + 各模块内部流程图
- [工作流指南](guide/workflows.md) — 维护任务、竞跑、远程监督、浏览器验收的 JSON 示例与安全边界
- 面向贡献者的架构说明以 [贡献指南](../CONTRIBUTING.md) 的目录树和源码注释为准（旧的三份开发文档因与代码脱节已删除）
- [终端交互实现与迁移边界](bubbletea-migration-plan.md) — 为什么不换 UI 框架
- [配置示例](config.example.json)
- [项目 README](../README.md) — 中英双语项目总览
- [更新日志](../CHANGELOG.md) — 版本发布历史
- [贡献指南](../CONTRIBUTING.md) — 如何参与贡献
- [行为准则](../CODE_OF_CONDUCT.md) — 社区行为准则
- [安全策略](../SECURITY.md) — 安全问题报告流程

## 维护约定

当前功能以使用手册、工作流指南和源码为准；已完成的删除与行为变更记录在更新日志，不再保留单独的精简分析。

`superpowers/` 下的方案和设计文档是按日期保存的历史记录，不代表当前实现。更新功能时同步现行文档、配置示例和文档一致性测试，不改写历史方案。
