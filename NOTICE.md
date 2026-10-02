# NOTICE

本项目基于 [xuemian168/domain-scanner](https://github.com/xuemian168/domain-scanner)(AGPL-3.0)。

- `internal/{cache,domain,generator,reserved,stats,types,worker}`、`main.go` 来自上游,保持原样。
- 新增(本项目):`internal/domain/detail.go`(带 unknown 语义的检测器,复用上游指示词与保留规则)、
  `internal/enumerate`、`internal/store`、`internal/scheduler`、`internal/notifier`、`internal/logbus`、
  `internal/wordlists`、`internal/auth`、`internal/server`、`cmd/server`、`web/`。
- 本项目整体以 AGPL-3.0 发布,见 `LICENSE`。
