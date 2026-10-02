# NOTICE

本项目基于 [xuemian168/domain-scanner](https://github.com/xuemian168/domain-scanner)(AGPL-3.0)。

- `internal/{cache,domain,generator,reserved,stats,types,worker}`、`main.go` 来自上游,保持原样。
- 新增(本项目):`internal/domain/detail.go`(带 unknown 语义的检测器,复用上游指示词与保留规则)、
  `internal/enumerate`、`internal/store`、`internal/scheduler`、`internal/notifier`、`internal/logbus`、
  `internal/wordlists`、`internal/auth`、`internal/server`、`internal/egress`、`internal/proxy`、
  `internal/cloudflare`、`internal/verify`、`internal/register`、`internal/appsettings`、`cmd/server`、`web/`。
- 出站代理使用 [XTLS/Xray-core](https://github.com/XTLS/Xray-core)(MPL-2.0)的官方发布二进制,Docker 构建时下载并校验 sha256,不修改其源码。
- 出站代理管理页的交互(基础 / JSON / 导入、测活)参考 [MHSanaei/3x-ui](https://github.com/MHSanaei/3x-ui) 的出站管理界面,仅参考设计,未复制其代码。
- 本项目整体以 AGPL-3.0 发布,见 `LICENSE`。
