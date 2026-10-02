# Domain Scanner Web

在 Docker 里长期后台运行的**未注册域名扫描服务**:用 Web UI 创建/暂停/继续任务,实时查看日志,发现可注册域名后经 **Cloudflare Registrar 最终核查**,再通过 **Telegram 机器人**推送,消息上带**一键注册**按钮。

基于 [xuemian168/domain-scanner](https://github.com/xuemian168/domain-scanner)(Go CLI,AGPL-3.0)。本项目保留其检测规则与指示词,并在外面加了服务层、Web UI、通知与部署,见 [NOTICE.md](NOTICE.md)。

- **后台挂起**:任务在服务端运行,关页面不受影响;容器重启后未完成的任务**自动从断点续跑**(游标与计数一起持久化,不重复计数)
- **多数据源检测**:DNS → **RDAP**(IANA bootstrap + 内置已验证的 ccTLD 服务器)→ WHOIS 兜底 → TLS 证书交叉验证;**不确定就是 unknown,绝不会误报成可注册**
- **Cloudflare 最终核查**:检测命中后用 Registrar `domain-check` 确认「真的能注册」及价格;不可注册的(保留名、溢价、已被占用)不推送
- **一键注册**:Telegram 消息带「注册」按钮 → 看到域名和价格 → 再点「确认注册」才扣费;有单价上限和每日上限
- **出站代理(Xray)**:内置 Xray 内核,支持 VLESS / VMess / Trojan / Shadowsocks / SOCKS / HTTP;可用表单、JSON 或分享链接/订阅导入,支持测活;**每个任务可单独选择出站**(直连 / 指定代理 / 代理池)
- **直连不限并发,出错自动退避并切换**:直连任务数量不设上限;检测到大量错误(限流、超时、断连)时主动退避,并切换到测活通过的代理,恢复后自动切回
- **详细日志**:每个检查步骤(DNS、RDAP、WHOIS、TLS、出站切换、Cloudflare、Telegram)都有结构化日志,可按组件/域名/出站/关键字过滤,导出 JSONL,「诊断统计」按出站和后缀汇总失败原因,便于运行一段时间后迭代
- **限流友好**:同一注册局的所有任务共享请求间隔;收到 429 时全体暂停并指数退避(5 秒 → 5 分钟)
- **复用高星开源项目**:检测规则来自上游 domain-scanner;WHOIS 用 [likexian/whois](https://github.com/likexian/whois);正则用 [dlclark/regexp2](https://github.com/dlclark/regexp2);出站代理用 [XTLS/Xray-core](https://github.com/XTLS/Xray-core);内置词库 [first20hours/google-10000-english](https://github.com/first20hours/google-10000-english) 与 [dwyl/english-words](https://github.com/dwyl/english-words);界面用 [shadcn/ui](https://ui.shadcn.com)(`preset b0`)+ Next.js
- **强制口令保护**:`ADMIN_PASSWORD` 不设置就无法启动
- **多设备**:桌面、平板、手机布局都经过 Playwright 在多种设备尺寸和三种浏览器内核上的自动化检查

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
| `CLOUDFLARE_ACCOUNT_ID` / `CLOUDFLARE_API_TOKEN` | 否 | Cloudflare Registrar 核查与注册;也可在「设置」页填写(优先) |
| `WEB_PORT` / `WEB_BIND` | 否 | Web 对外端口(默认 3000)与绑定地址 |
| `MAX_PARALLEL_JOBS` | 否 | **走代理**的任务同时运行上限,默认 0(不限制);直连任务永远不受限 |
| `RDAP_SERVERS` | 否 | 额外 RDAP 服务器,`tld=https://rdap.example/,tld2=…` |
| `XRAY_BIN` | 否 | xray 可执行文件路径,镜像内已内置(`/usr/local/bin/xray`) |
| `LOG_STDOUT_LEVEL` | 否 | `docker compose logs` 的最低级别,默认 `info` |

### 设置 Telegram 通知

1. 在 [@BotFather](https://t.me/BotFather) 创建机器人,拿到 token。
2. **用你自己的 Telegram 账号打开这个机器人并点击 Start(或发送 `/start`)。** 机器人不能主动联系从没跟它说过话的人,否则会报 `chat not found`。
3. 获得自己的 Chat ID(例如给 [@userinfobot](https://t.me/userinfobot) 发消息)。
4. 在「设置」页填写 Token 和 Chat ID,保存后点「发送测试消息」。

命中的域名会按任务合并,每 10 秒或满 20 个发送一条;发送失败自动重试 3 次,最终失败只写日志、不影响扫描。

### Cloudflare 核查与一键注册

1. 在 Cloudflare 创建 API Token,**权限只给 Registrar**(它具有注册、扣费能力);在「设置 → Cloudflare Registrar」填入账户 ID 和 Token,点「校验凭据」。
2. 扫描命中后,每个域名会调用 `POST /accounts/{id}/registrar/domain-check`:
   - **可注册**(`registrable=true`、非溢价)→ 推送,消息带价格和「注册」按钮;
   - **不可注册 / 溢价** → 不推送(这就是用来消除误报的最后一道检查);
   - **Cloudflare 不支持的后缀**(如 `.li` `.ch` `.sh` `.cc`)→ 仍推送(可在设置里关闭),标注「未经 Cloudflare 确认」,**不带注册按钮**;
   - Cloudflare 暂时出错 → 按「未经确认」处理,不带按钮。
3. 点「注册」→ 机器人回复域名与价格 → 再点「确认注册」才会真正调用 `POST /accounts/{id}/registrar/registrations`。**这一步会真实扣费且不可退款。** 可在设置里关闭二次确认(不推荐)。
4. 保护措施:单价上限(默认 30 美元)、每日注册上限(默认 5 个);数据库层面的原子占位保证同一个域名不会被重复扣费;确认按钮 15 分钟后失效;只接受来自你配置的私聊的按钮点击。
5. 只有**私聊**(数字 Chat ID)才会出现按钮;频道/群组只收通知。

### 出站代理(Xray)

「出站代理」页(参考 3x-ui 的出站管理):

- **基础**:选协议、填地址/端口/凭据,选传输(TCP / WS / gRPC / HTTPUpgrade / XHTTP / mKCP)和 TLS / Reality;
- **JSON**:直接编辑 Xray outbound JSON(基础表单表示不了的配置会自动打开这个标签);
- **导入**:粘贴 `vless://` `vmess://` `trojan://` `ss://` `socks5://` `http://` 链接、base64 订阅内容或 Xray 配置,先预览(逐行报错)再导入;
- **测活**:经该代理访问测活地址(默认 `https://cp.cloudflare.com/generate_204`),显示延迟(冷启动与复用连接各一次)、出口 IP 与国家;每 5 分钟自动复测,结果决定能否作为故障切换目标;
- 配置被 Xray 拒绝时(例如缺 Reality 公钥)会直接显示原因,且不影响其它代理。

创建任务时选择**出站方式**:直连、指定某个代理、或代理池;「出错时自动切换」默认开启。

退避与切换:每个任务观察最近 20 次检查,失败率 ≥ 60%(限流、超时、断连)就退避 30 秒(之后翻倍,最多 10 分钟),同时给该出站记一次惩罚,并切换到测活通过的出站;直连被切走后,约 5 分钟后试探性切回。

## 怎么判断一个域名"可注册"

1. 保留名规则(可选,默认关闭):上游把 1–2 位字母、2–3 位数字一律当保留名,会让短域名扫描什么都扫不出,所以默认交给注册局判断。
2. DNS 有 NS/A/MX 记录 → 已注册。
3. **RDAP**:HTTP 200 = 已注册,404 = 未注册。内置 `li ch de nl fr cz pl io ai sh ac cc` 的服务器(它们不在 IANA bootstrap 里),其余 TLD 走 IANA bootstrap。
4. RDAP 出错或该 TLD 不支持 → **WHOIS** 兜底(含备用服务器与退避)。
5. 判为未注册后再探测 443 端口,有 TLS 证书则改判已注册。
6. 任何一步超时/被限流/无法解析 → **unknown**:不推送、不计入"可注册",在结果页可单独查看。
7. 可注册的结果再交给 **Cloudflare** 做最终确认(见上)。

### 已知局限

- 个别注册局对"注册局保留名/禁止注册的短名"也返回 404(例如 `.li` 的 2 位数字几乎全是"可注册"),这类域名实际注册时可能被拒;Cloudflare 不支持的后缀无法做最终确认,所以消息里会标注。
- 注册局对单个 IP 的 RDAP/WHOIS 额度很有限(如 `rdap.nic.ch`)。限流时扫描会自动变慢而不是出错;配几个代理并选「代理池」能分摊请求。
- 部分注册局(如 `.li` 的 SWITCH)的 WHOIS 直接拒绝自动化客户端,这类后缀只能依赖 RDAP。
- Xray 新版不再支持 h2 / quic 传输,这类链接会在导入时提示改用 xhttp / grpc / ws。
- 带 `plugin=` 的 Shadowsocks 链接暂不支持(需要额外的插件进程)。

## 日志与诊断

「运行日志」页:

- **实时日志**:SSE 推送,按级别、任务、组件、域名、出站、关键字过滤(服务端过滤,历史与实时一致),点一行展开结构化字段(耗时、出站、各步骤结果),可导出 JSONL;
- **诊断统计**:按出站(检查数、成功率、限流/超时/网络错误、平均与最大耗时、退避次数)和按后缀汇总,用来判断下一步该调什么;
- 「设置 → 日志与代理」可把记录级别调到 `debug`,此时每个检查步骤(每次 DNS 查询、RDAP/WHOIS 请求、TLS 探测的结果与耗时)都会入库。日志按级别分层保留(debug 保留最少,error 保留最多),数据库不会无限增长。
- API:`GET /api/logs`、`/api/logs/export`(JSONL)、`/api/logs/stream`、`/api/diagnostics`。Token、口令等敏感值在写入前统一脱敏。

## 安全

- 所有接口(包括日志流)都需要登录;唯一公开的是 `/api/health`。
- 会话 cookie 为 `HttpOnly` + `SameSite=Strict`,7 天有效;退出登录会在服务端吊销会话;修改口令后所有旧会话失效。
- 连续输错 5 次,按来源 IP 锁定 5 分钟。
- scanner 的 8080 端口**不映射到宿主机**,只能经 web 容器访问。
- Telegram token、Cloudflare token 不会出现在日志和 API 响应里(只返回脱敏值)。
- **Cloudflare Token 能花钱**:请只授予 Registrar 权限,并设好单价/每日上限;如果 Token 或 Bot Token 曾经出现在聊天记录、截图或终端里,请到 Cloudflare / @BotFather 轮换。
- 若放在公网,请务必套 HTTPS 反向代理(代理需传 `X-Forwarded-Proto: https`,cookie 会自动带 `Secure`)。

## 开发

本机不需要安装 Go:

```bash
# 后端测试(Docker 里的 golang 镜像)
docker run --rm -v "$PWD:/src" -w /src golang:1.26 go test -race ./...

# 用真实 xray 校验解析器生成的全部配置(需要 xray 二进制)
docker run --rm -v "$PWD:/src" -w /src -e XRAY_BIN=/src/.tmp/xray golang:1.26 go test -run RealXray ./internal/proxy/

# 前端
cd web && pnpm install && pnpm lint && pnpm typecheck && pnpm build

# 前端热更新:把 scanner 端口开到本机回环,再起 dev server
docker compose -f docker-compose.yml -f docker-compose.dev.yml up -d scanner
cd web && pnpm dev
```

### 多设备界面测试

`web/e2e` 是 Playwright 用例,对运行中的栈做检查:每个页面在 15 种设备/浏览器配置下无横向溢出、无控制台错误、控件都在屏幕内、触控目标 ≥ 24px,并覆盖代理增删测活导入、任务创建、日志展开、设置脱敏等流程。

```bash
cd web
pnpm exec playwright install chromium firefox webkit
E2E_PASSWORD=<管理口令> pnpm e2e            # 全部配置
E2E_PASSWORD=<管理口令> pnpm e2e --project=iphone-se
```

手机/平板为 Chromium 设备模拟(iPhone SE、iPhone 14 Pro Max、Pixel 7、Galaxy S9+、320px 小屏、iPad 竖/横);另有真实 Firefox 与 WebKit 内核(桌面与手机尺寸)。截图输出到 `web/e2e-screenshots/`(已 gitignore)。

`scripts/smoke*.sh` 是在 compose 网络里跑的真实环境冒烟脚本(用法见各脚本头部注释);`scripts/smoke-proxy.sh` 验证经 Xray 出站的完整链路。

### 目录

```
cmd/server/          服务入口与配置
internal/domain/     检测器:上游 checker.go + detail.go(unknown 语义)+ rdap.go + trace.go(逐步骤记录)
internal/egress/     出站(直连/SOCKS5)、健康与惩罚注册表
internal/proxy/      Xray:分享链接解析、配置生成、进程管理、测活
internal/scheduler/  任务状态机、断点续跑、错误风暴退避与出站切换
internal/cloudflare/ Registrar API 客户端(domain-check / registrations)
internal/verify/     命中后的 Cloudflare 核查流水线
internal/register/   一键注册(二次确认、价格与次数上限、防重复扣费)
internal/notifier/   Telegram 批量通知与按钮回调
internal/appsettings/ 设置项(密钥、注册策略、日志级别)
internal/store/      SQLite
internal/logbus/     日志总线(内存环 + SSE + 持久化 + 脱敏)
internal/enumerate/  可按下标取值、可续跑的候选生成器
internal/wordlists/  内置/上传词库
internal/auth/       口令登录、限流、会话
internal/server/     REST + SSE API
web/                 Next.js + shadcn/ui(b0);web/e2e 为多设备测试
```

## 常见问题

- **大量 unknown**:到「运行日志」看原因,或看「诊断统计」。`HTTP 429` 表示被注册局限流(会自动退避);`whois ... not permitted` 表示该注册局拒绝 WHOIS,请确认该 TLD 有 RDAP(可用 `RDAP_SERVERS` 补充)。
- **Telegram 报 `chat not found`**:先在 Telegram 里对机器人点 Start,并核对 Chat ID。
- **没有出现「注册」按钮**:确认 Cloudflare 已配置、该后缀被 Cloudflare 支持、Chat ID 是私聊的数字 ID,且价格没超过上限。
- **代理显示不可用**:列表里会显示 Xray 或探测给出的原因(DNS 解析失败、连接被拒、握手失败等);先「测活」,再检查 JSON。
- **想让容器日志也能看**:`docker compose logs -f scanner`,和日志页内容一致。

## 许可证

[AGPL-3.0](LICENSE),与上游一致。若你修改并通过网络向他人提供服务,需按 AGPL 公开对应源码。Xray-core 为 MPL-2.0,以独立二进制随镜像分发。
