# 出站代理 / Cloudflare 终检 / 可观测性 — 设计与实施计划

日期:2026-10-03  状态:用户已确认关键决策(见 §0),直接实施

## 0. 已确认的决策

| 问题 | 决定 |
|---|---|
| Telegram 注册按钮 | 点「注册」→ 弹出二次确认(域名+价格)→ 点「确认注册」才扣费。设置页可关闭二次确认 |
| Cloudflare 不支持的后缀(.li/.ch/.sh/.cc…) | 仍推送,标注「未经 Cloudflare 确认」,**不带注册按钮** |
| UI 测试范围 | Chromium 多设备模拟 + 尝试安装 Firefox/WebKit 跑同一套断言 |
| 其余默认 | 直连任务并行数不限;DNS 预检直连;注册防护:单价上限 30 USD、每日 5 个 |

## 1. 出站代理(Xray)

- scanner 镜像内置 `xray-core v26.3.27`(构建时下载,校验官方 `.dgst` 的 sha256,按 `TARGETARCH` 选 amd64/arm64)。
- `internal/proxy`:
  - `link.go`:解析 `vless:// vmess:// trojan:// ss:// socks:// socks5:// http(s)://` 分享链接(支持 tcp/ws/grpc/httpupgrade/xhttp,tls/reality)→ xray outbound JSON。
  - `outbound.go`:模型与校验(协议白名单、地址、端口)。
  - `xrayconf.go`:把所有启用的出站生成为一份 xray 配置:每个出站一个 `127.0.0.1:<port>` 的 SOCKS 入站 + `inboundTag→outboundTag` 路由。
  - `runtime.go`:长驻 xray 进程管理(启动、配置变更重启、崩溃退避重启、日志转入日志总线、端口就绪检测)。
  - `probe.go`:测活。临时 xray 实例或运行中实例的 SOCKS 端口,对测试地址(默认 `https://cp.cloudflare.com/generate_204`)做冷/热两次计时请求,并读取 `cloudflare.com/cdn-cgi/trace` 得到出口 IP/国家。
- 存储:`outbounds` 表(name、protocol、address、port、config JSON、enabled、最近测活结果)。
- API:`GET/POST /api/outbounds`、`PUT/DELETE /api/outbounds/{id}`、`POST /api/outbounds/{id}/test`、`POST /api/outbounds/test-all`、`POST /api/outbounds/test-config`(未保存的 JSON 也能测)、`POST /api/outbounds/import`(解析链接/JSON,返回预览)。
- 前端「出站代理」页:列表(协议、地址、延迟、出口 IP/国家、状态)、测活/全部测活、启停、删除;弹窗三 tab:**基础**(表单)/ **JSON** / **导入**。
- 后台每 5 分钟自动测活,结果决定"可用代理"。

## 2. 任务出口、退避与故障切换

- `internal/egress`:`Egress{ID,Name,Direct,SocksAddr}`、`Pool`(按 ID 取、可用列表、冷却/惩罚)。HTTP、WHOIS、TLS 探测都通过 Egress 拨号;DNS 预检保持直连。
- 每个出口一个独立的 `domain.Checker`(RDAP 限速/退避状态按"出口+注册局"分开,因为限流按源 IP)。
- 任务参数:`egress_mode ∈ direct|proxy|pool`、`proxy_id`、`failover`(默认开)。
- **并行数**:直连任务不受 `MAX_PARALLEL_JOBS` 限制;走代理的任务仍受限。
- **健康检测**:每个任务滑动窗口(最近 20 次):`rate_limited/network/timeout` 型 unknown 占比 ≥ 60% 且样本 ≥ 10 ⇒ 触发:
  1. 全体 worker 暂停退避(30s 起翻倍,上限 10 分钟,窗口恢复健康 2 分钟后复位);
  2. 对当前出口施加冷却(5 分钟起翻倍,上限 30 分钟),所有任务选出口时跳过;
  3. failover 开启时切换到下一个"可用且未冷却"的代理(direct 模式从直连切到代理;proxy/pool 模式换另一个代理;都不可用则原地退避)。
- failover 开启时 RDAP 对 429 的最长等待缩短(默认 5 秒),使故障尽快显现并切换;关闭时保持长时间等待。

## 3. Cloudflare 终检与一键注册

- `internal/cloudflare`:`Check(domains ≤20)`(`POST /accounts/{id}/registrar/domain-check`)、`Register(domain)`(`POST …/registrations`)、`Verify()`;统一错误分类(认证、限流、5xx)。
- 结果表新增:`cf_status`(`pending|confirmed|rejected|unsupported|error`)、`cf_reason`、`cf_price`、`cf_currency`、`cf_checked_at`、`register_status`(`''|registering|succeeded|failed`)、`register_note`。
- 流程:RDAP 判可注册 → 落库 `cf_status=pending` → 校验队列(≤20/批,2 秒或满批触发,429/5xx 退避重试)→ 更新结果 → 推送:
  - `confirmed`:带价格与「注册」按钮;
  - `unsupported`(`extension_not_supported*`):推送,标「未经 Cloudflare 确认」,无按钮;
  - `rejected`(`domain_unavailable`/`domain_premium`/`extension_disallows_registration`):**不推送**,结果页显示原因;
  - `error`(校验失败多次):推送并标注「校验失败」,无按钮;
  - 未配置 Cloudflare:沿用旧行为,标注「未配置 Cloudflare 校验」。
- Telegram 回调:notifier 长轮询 `getUpdates(allowed_updates=[callback_query])`;只接受来自已配置会话的点击;`reg:<id>` → 预览(重新 `domain-check` 取最新价格,校验单价上限与每日上限)→ 二次确认消息 `ok:<id>`/`no:<id>` → 数据库原子认领(`register_status=registering`,防双击)→ `Register` → 回报结果;202 异步时轮询 `links.self` 至多 2 分钟。
- 设置:Cloudflare 账户 ID/Token(脱敏)、测试连接、二次确认开关、单价上限、每日上限。

## 4. 日志重做

- 日志条目:`component`(scheduler/check/dns/rdap/whois/tls/cloudflare/notifier/xray/egress/auth/http/system)、`event`、`domain`、`egress`、`duration_ms`、`fields`(JSON)。
- Checker 通过 context 注入的 trace 回调实时上报每一步(`check.step`:DNS、RDAP 请求、WHOIS 重试、TLS、判定),最终 `check.done` 汇总(判定、依据、总耗时、出口)。
- 调度器:任务生命周期、窗口统计(速率/ETA 每 30 秒)、退避/切换/冷却决策、续跑游标;Cloudflare 批次请求与响应摘要;Telegram 每次发送尝试;xray 启停/重启/测活;登录与设置变更;API 访问日志(GET 轮询为 debug,变更类为 info,不含密钥)。
- 存储:持久化最低级别可设置(默认 debug);保留策略按级别分开(debug ≤ 200k 条,其余 ≤ 500k 条),每 10 分钟裁剪。stdout 只打 info+(`LOG_STDOUT_LEVEL`)。
- API/UI:按组件、级别、任务、出口、域名、全文搜索筛选;字段可展开;`/api/logs/export` 下载 JSONL;`/api/diagnostics` 汇总各出口成功率/429 次数、各后缀判定分布、平均耗时。

## 5. 多设备 UI 测试

- `web/e2e`:Playwright 脚本,登录后遍历全部页面,断言:无横向溢出(`scrollWidth ≤ innerWidth`)、无控制台错误、关键元素可见,并截图。
- 设备矩阵:iPhone SE(375×667)、iPhone 14 Pro Max(430×932)、Pixel 7(412×915)、Galaxy Fold 折叠态(280×653)、iPad Mini 竖/横、iPad Pro 横、笔记本 1366×768、1080p、2560×1440;浅色/深色;触控模拟。
- 引擎:Chromium + 尝试 Firefox/WebKit(下载失败则如实报告)。

## 6. 实施任务(顺序)

1. 日志:logbus/store 结构化 + 迁移(`ensureColumn`)+ 保留策略 + 过滤/搜索/导出 API。
2. Checker:trace、错误类型、Egress 注入、ctx 选项(MaxWait)。
3. egress 包 + 调度器(出口模式、健康窗口、退避、切换、直连不限并行)。
4. proxy 包(链接解析、xray 配置、运行时、测活)+ outbounds 存储/API。
5. Cloudflare 客户端 + 校验队列 + 注册服务 + Telegram 按钮/回调 + 设置 API。
6. 前端:出站代理页、任务表单出口选择、结果页 CF 列、日志页增强、设置页(CF/注册防护/日志级别)、诊断。
7. Docker:xray 内置;集成冒烟(xray 作为 SOCKS 服务端容器 → 加为出站 → 任务走代理)。
8. 多设备 Playwright 测试与修复。
9. README、全量测试、推送。

## 7. 安全与风险

- Cloudflare Token 具备扣费能力:只存 `settings` 表与 `.env`,API 只返回脱敏值,日志中脱敏;注册受单价/每日上限与(默认)二次确认保护;数据库原子认领防重复注册。
- 注册不可退款;Telegram 回调必须校验来源会话。
- 代理凭据(uuid/密码)存数据库,API 列表返回脱敏摘要,完整配置仅在编辑时返回给已登录用户。
- 个别注册局对保留名返回 404 的局限仍存在,Cloudflare 终检正是为此;`.li/.ch` 等不支持后缀只能标注未确认。
