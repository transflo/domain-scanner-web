# Domain Scanner Web — 设计规格

日期:2026-10-02  状态:待用户审阅

## 1. 目标

把上游 [xuemian168/domain-scanner](https://github.com/xuemian168/domain-scanner)(Go CLI,AGPL-3.0)做成可在 Docker 中长期后台运行的"未注册域名扫描服务":

- 通过 Web UI 创建、暂停、继续、取消扫描任务。
- 发现可注册域名后通过 Telegram 机器人推送。
- 内置日志窗口,便于排查错误。
- `docker compose up -d` 一条命令部署;容器重启后未完成任务自动续跑。
- 前端严格使用 `pnpm dlx shadcn@latest init --preset b0 --template next` 生成的主题与组件。
- 复用高星开源库/词库:`likexian/whois`(上游已用)、`dlclark/regexp2`(上游已用),内置 google-10000-english 等词表。

### 非目标(YAGNI)

- 多用户/角色体系(单用户自托管;用**强制**的 `ADMIN_PASSWORD` 口令保护全部功能,见 §6)。
- 域名注册/购买功能。
- 除 Telegram 外的其他通知渠道。

## 2. 架构

```
浏览器 ──► web (Next.js, shadcn b0) :3000 ── rewrites /api/* ──► scanner (Go) :8080
                                                                   ├ REST API + SSE 日志流
                                                                   ├ scheduler(任务队列/续跑)
                                                                   ├ scanner(复用 internal/domain、generator、reserved)
                                                                   ├ wordlists(内置+上传)
                                                                   ├ notifier(Telegram)
                                                                   ├ logbus(环形缓冲 + 持久化)
                                                                   └ store(SQLite, /data volume)
```

仓库布局(Go module 沿用上游 `domain_scanner`,以便 import `internal/*`):

```
cmd/server/            服务入口
internal/domain|generator|reserved|...   上游代码(保留,尽量少改)
internal/server/       HTTP 路由、SSE
internal/store/        SQLite 访问层
internal/scheduler/    任务调度与续跑
internal/notifier/     Telegram 通知
internal/logbus/       日志总线
internal/wordlists/    词库管理
web/                   Next.js 前端
Dockerfile.scanner, web/Dockerfile, docker-compose.yml
docs/
```

## 3. 后端组件

| 组件 | 职责 | 依赖 |
|---|---|---|
| store | SQLite(纯 Go 驱动 `modernc.org/sqlite`,无需 CGO);表:jobs、results、logs、settings | — |
| scheduler | 任务状态机 `queued→running→paused→done/failed/cancelled`;启动时将 queued/running 任务重新入队;按游标(已处理索引)续跑;并发数受全局 worker 上限约束 | store、scanner、logbus、notifier |
| scanner | 封装上游生成器与检测器,输出 `(domain, status, signature)`;支持长度/后缀/模式/正则/字典/词库来源、delay、workers | 上游 internal/* |
| wordlists | 构建期下载高星词表入镜像;API 列出可用词表;支持上传自定义字典 | — |
| notifier | 可注册结果批量合并(默认每 10 秒或满 20 条)发送;测试消息;失败重试 3 次(退避),最终失败记日志不阻塞扫描 | store(读取设置)、logbus |
| logbus | 内存环形缓冲(最近 2000 条)+ 异步落库;SSE 订阅;日志级别 debug/info/warn/error;带 job_id 字段 | store |

### 检测语义(防误报)

- 仅当 DNS(NS/A/MX)、WHOIS、SSL 均判定"未注册"才记为 `available`。
- 任一检测超时/出错且无其他证据 → 记为 `unknown` 并写 warn 日志,**不推送、不计入可注册**。
- 保留上游 reserved 规则与重试/指数退避。

### 续跑

任务保存 `cursor`(已完成的候选序号)。生成器是确定性的(同参数同顺序),续跑时跳到 `cursor`。每处理 N 条或每 5 秒刷新一次 cursor。

## 4. REST API(前缀 `/api`)

| 方法 路径 | 说明 |
|---|---|
| GET `/health` | 健康检查(**唯一免认证接口**,只返回 `{"ok":true}`) |
| POST `/auth/login` | 提交口令,成功后下发会话 cookie;失败返回 401 |
| POST `/auth/logout` | 清除会话 cookie |
| GET `/auth/me` | 返回当前是否已登录(前端据此跳转登录页) |

以下所有接口均要求有效会话 cookie,否则返回 401(含 SSE `/logs/stream`)。
| GET/POST `/jobs` | 列表 / 创建(参数:suffix、length、pattern、regex、wordlist_id|dict、delay_ms、workers) |
| GET `/jobs/{id}` | 详情与进度 |
| POST `/jobs/{id}/pause` `/resume` `/cancel` | 控制 |
| DELETE `/jobs/{id}` | 删除(含结果) |
| GET `/results?job_id=&status=&q=&limit=&offset=` | 结果查询 |
| GET `/results/export?job_id=` | CSV 导出 |
| GET `/logs?level=&job_id=&limit=` | 历史日志 |
| GET `/logs/stream` | SSE 实时日志 |
| GET `/wordlists` / POST `/wordlists` | 词库列表 / 上传 |
| GET/PUT `/settings` | 设置(token 在响应中脱敏) |
| POST `/settings/telegram/test` | 发送测试消息 |
| GET `/stats` | 仪表盘汇总 |

## 5. 前端(Next.js + shadcn `b0` preset)

- 初始化严格使用 `pnpm dlx shadcn@latest init --preset b0 --template next`,其后仅通过 `shadcn add` 添加组件,不手写与主题冲突的样式。
- 页面:
  - **登录页**:口令输入框(shadcn Card + Input + Button),错误提示;未登录访问任何页面由 Next `middleware` 重定向到此;侧边栏提供退出登录。
  - **仪表盘**:运行中任务、进度、速率、累计可注册数、后端连接状态。
  - **任务**:列表 + 新建任务表单(Sheet/Dialog)+ 暂停/继续/取消/删除。
  - **结果**:Table,搜索/筛选、复制、导出 CSV。
  - **日志**:实时流;级别过滤、按任务过滤、暂停滚动、下载、清屏。
  - **设置**:Telegram token/chat id、测试按钮、默认扫描参数。
- 数据获取:fetch + SSE(EventSource);后端不可达时显示明确提示。
- 通过 `next.config` rewrites 把 `/api/*` 代理到 `SCANNER_URL`(compose 内为 `http://scanner:8080`)。

## 6. 配置与密钥

- `.env`(已 gitignore)与 `.env.example`:`TELEGRAM_BOT_TOKEN`、`TELEGRAM_CHAT_ID`、`ADMIN_PASSWORD`(**必填**)、`DATA_DIR`。
- **口令保护强制开启,无法关闭:**
  - `ADMIN_PASSWORD` 为空或未设置时,scanner 拒绝启动并输出明确错误;`docker-compose.yml` 使用 `${ADMIN_PASSWORD:?请在 .env 中设置 ADMIN_PASSWORD}`,缺失时 `compose up` 直接失败。
  - 最短长度 8 位,不足同样拒绝启动。
  - 登录成功后下发 `HttpOnly`、`SameSite=Strict` 的会话 cookie(HMAC 签名,含过期时间,默认 7 天);签名密钥首次启动随机生成并保存在 SQLite,轮换口令时自动使旧会话失效(密钥由口令派生加盐)。
  - 口令比较使用常量时间比较;登录接口按来源 IP 限流(连续 5 次失败锁定 5 分钟),并写 warn 日志(不记录口令)。
  - scanner 的 8080 端口默认**不**映射到宿主机,所有访问必须经 web 的 3000 端口;即便直连 8080,除 `/health` 外也都需要认证。
- 设置页写入的 Telegram 配置存入 SQLite settings,优先级高于环境变量;读取接口只返回脱敏 token。
- 日志中不得输出 token(notifier 统一脱敏)。
- 提交前对仓库全文扫描 token 与 chat id,确认无泄漏。

## 7. 部署

- `scanner`:多阶段构建(golang → distroless/alpine),词库在构建期下载,`/data` 挂 named volume。
- `web`:Next.js `output: "standalone"` 多阶段构建,pnpm。
- `docker-compose.yml`:两个服务、healthcheck、`restart: unless-stopped`、仅暴露 web 的 3000 端口(scanner 8080 不映射到宿主机)。

## 8. 测试与验证(完成前逐条执行)

1. Go 单元测试:生成器确定性、状态机、续跑、notifier 批处理与重试(mock Telegram HTTP 服务)、检测语义(unknown 不得当 available)。
2. 认证验证:未设置/过短的 `ADMIN_PASSWORD` 时服务拒绝启动;未登录访问任一 `/api/*`(含 SSE)返回 401、访问页面被重定向到登录页;错误口令登录失败且连续 5 次后被限流;正确口令登录后全部功能可用;退出后会话失效。
3. `docker compose up -d --build`,健康检查通过。
4. 容器内真实小规模扫描(如 `.li` 3 位数字),确认结果入库、日志流正常。
5. 浏览器自动化遍历所有页面,检查控制台与网络请求无错误;截图确认 shadcn 主题生效。
6. 向会话 id <CHAT_ID> 真实发送测试消息,并确认扫描命中时收到推送。
7. `docker compose restart scanner`,确认运行中任务自动续跑。
8. 密钥泄漏扫描通过后才允许 push。

## 9. 仓库与许可

- GitHub 新建 `domain-scanner-web`(账号 `transflo`),默认公开,AGPL-3.0。
- README 注明上游来源与修改说明,保留上游 LICENSE 与版权声明。
- 不提交 `.env`、数据库、`node_modules`。

## 10. 风险

- 公共 WHOIS 服务器有限流,大范围扫描会产生大量 unknown;UI 给出速率建议并默认保守(delay 1000ms)。
- 上游 internal 包 API 可能不适合直接复用(如耦合控制台输出),必要时做最小改动并在 README 记录。
