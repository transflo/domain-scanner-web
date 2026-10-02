# Domain Scanner Web

在 Docker 里长期后台运行的**未注册域名扫描服务**:用 Web UI 创建/暂停/继续任务,实时查看日志,发现可注册域名后通过 **Telegram 机器人**推送。

基于 [xuemian168/domain-scanner](https://github.com/xuemian168/domain-scanner)(Go CLI,AGPL-3.0)。本项目保留其检测规则与指示词,并在外面加了服务层、Web UI、通知与部署,见 [NOTICE.md](NOTICE.md)。

- **后台挂起**:任务在服务端运行,关页面不受影响;容器重启后未完成的任务**自动从断点续跑**(游标与计数一起持久化,不重复计数)
- **多数据源检测**:DNS → **RDAP**(IANA bootstrap + 内置已验证的 ccTLD 服务器)→ WHOIS 兜底 → TLS 证书交叉验证;**不确定就是 unknown,绝不会误报成可注册**
- **限流友好**:同一注册局的所有任务共享请求间隔;收到 429 时全体暂停并指数退避(5 秒 → 5 分钟),日志里能看到原因
- **复用高星开源项目**:检测规则来自上游 domain-scanner;WHOIS 用 [likexian/whois](https://github.com/likexian/whois);正则用 [dlclark/regexp2](https://github.com/dlclark/regexp2);内置词库 [first20hours/google-10000-english](https://github.com/first20hours/google-10000-english) 与 [dwyl/english-words](https://github.com/dwyl/english-words);界面用 [shadcn/ui](https://ui.shadcn.com)(`preset b0`)+ Next.js
- **日志窗口**:实时(SSE)、按级别/任务过滤、暂停滚动、下载,专门用来排查限流、超时和通知失败
- **强制口令保护**:`ADMIN_PASSWORD` 不设置就无法启动

## 快速开始

需要 Docker 与 Docker Compose。

```bash
git clone https://github.com/transflo/domain-scanner-web.git
cd domain-scanner-web
cp .env.example .env        # 然后编辑 .env,至少填写 ADMIN_PASSWORD(≥ 8 位)
docker compose up -d --build
```

打开 <http://localhost:3000>,用 `ADMIN_PASSWORD` 登录。数据保存在名为 `scanner-data` 的 Docker volume 里。

### 配置

| 变量 | 必填 | 说明 |
|---|---|---|
| `ADMIN_PASSWORD` | **是** | 登录口令,至少 8 位 |
| `TELEGRAM_BOT_TOKEN` / `TELEGRAM_CHAT_ID` | 否 | Telegram 通知;也可在「设置」页填写(优先) |
| `WEB_PORT` | 否 | Web 对外端口,默认 3000 |
| `MAX_PARALLEL_JOBS` | 否 | 同时运行的任务数,默认 2 |
| `RDAP_SERVERS` | 否 | 额外 RDAP 服务器,`tld=https://rdap.example/,tld2=…` |

### 设置 Telegram 通知

1. 在 [@BotFather](https://t.me/BotFather) 创建机器人,拿到 token。
2. **用你自己的 Telegram 账号打开这个机器人并点击 Start(或发送 `/start`)。** 机器人不能主动联系从没跟它说过话的人,否则会报 `chat not found`。
3. 获得自己的 Chat ID(例如给 [@userinfobot](https://t.me/userinfobot) 发消息)。
4. 在「设置」页填写 Token 和 Chat ID,保存后点「发送测试消息」。

命中的域名会按任务合并,每 10 秒或满 20 个发送一条;发送失败自动重试 3 次,最终失败只写日志、不影响扫描。

## 怎么判断一个域名"可注册"

1. 保留名规则(可选,默认关闭):上游把 1–2 位字母、2–3 位数字一律当保留名,会让短域名扫描什么都扫不出,所以默认交给注册局判断。
2. DNS 有 NS/A/MX 记录 → 已注册。
3. **RDAP**:HTTP 200 = 已注册,404 = 未注册。内置 `li ch de nl fr cz pl io ai sh ac cc` 的服务器(它们不在 IANA bootstrap 里),其余 TLD 走 IANA bootstrap。
4. RDAP 出错或该 TLD 不支持 → **WHOIS** 兜底(含备用服务器与退避)。
5. 判为未注册后再探测 443 端口,有 TLS 证书则改判已注册。
6. 任何一步超时/被限流/无法解析 → **unknown**:不推送、不计入"可注册",在结果页可单独查看。

### 已知局限

- 个别注册局对"注册局保留名/禁止注册的短名"也返回 404(例如 `.li` 的 2 位数字几乎全是"可注册"),这类域名实际注册时可能被拒。建议扫描 4 位以上的名字,并用「启用上游保留名启发式」选项过滤。
- 注册局对单个 IP 的 RDAP/WHOIS 额度很有限(如 `rdap.nic.ch`)。限流时扫描会自动变慢而不是出错;大范围扫描需要耐心,或降低并发、加大间隔。
- 部分注册局(如 `.li` 的 SWITCH)的 WHOIS 直接拒绝自动化客户端,这类后缀只能依赖 RDAP。

## 安全

- 所有接口(包括日志流)都需要登录;唯一公开的是 `/api/health`。
- 会话 cookie 为 `HttpOnly` + `SameSite=Strict`,7 天有效;退出登录会在服务端吊销会话;修改口令后所有旧会话失效。
- 连续输错 5 次,按来源 IP 锁定 5 分钟。
- scanner 的 8080 端口**不映射到宿主机**,只能经 web 容器访问。
- Telegram token 不会出现在日志和 API 响应里(只返回脱敏值)。
- 若放在公网,请务必套 HTTPS 反向代理(代理需传 `X-Forwarded-Proto: https`,cookie 会自动带 `Secure`)。

## 开发

本机不需要安装 Go:

```bash
# 后端测试(Docker 里的 golang 镜像)
docker run --rm -v "$PWD:/src" -w /src golang:1.26 go test -race ./...

# 前端
cd web && pnpm install && pnpm lint && pnpm typecheck && pnpm build

# 前端热更新:把 scanner 端口开到本机回环,再起 dev server
docker compose -f docker-compose.yml -f docker-compose.dev.yml up -d scanner
cd web && pnpm dev
```

`scripts/smoke.sh`、`smoke-dict.sh`、`smoke-resume.sh` 是在 compose 网络里跑的真实环境冒烟脚本(用法见各脚本头部注释)。

### 目录

```
cmd/server/          服务入口与配置
internal/domain/     检测器:上游 checker.go + detail.go(unknown 语义)+ rdap.go
internal/enumerate/  可按下标取值、可续跑的候选生成器
internal/scheduler/  任务状态机与断点续跑
internal/store/      SQLite
internal/notifier/   Telegram 批量通知
internal/logbus/     日志总线(内存环 + SSE + 持久化)
internal/wordlists/  内置/上传词库
internal/auth/       口令登录、限流、会话
internal/server/     REST + SSE API
web/                 Next.js + shadcn/ui(b0)
```

## 常见问题

- **大量 unknown**:到「运行日志」看原因。`HTTP 429` 表示被注册局限流(会自动退避);`whois ... not permitted` 表示该注册局拒绝 WHOIS,请确认该 TLD 有 RDAP(可用 `RDAP_SERVERS` 补充)。
- **Telegram 报 `chat not found`**:先在 Telegram 里对机器人点 Start,并核对 Chat ID。
- **想让容器日志也能看**:`docker compose logs -f scanner`,和日志页内容一致。

## 许可证

[AGPL-3.0](LICENSE),与上游一致。若你修改并通过网络向他人提供服务,需按 AGPL 公开对应源码。
