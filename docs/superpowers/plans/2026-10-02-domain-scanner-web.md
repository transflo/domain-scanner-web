# Domain Scanner Web Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把上游 domain-scanner 做成 Docker 内常驻的未注册域名扫描服务:Web UI 管理任务、实时日志、Telegram 推送、强制口令保护。

**Architecture:** 上游 Go 代码放进同一 module(`domain_scanner`),新增 `cmd/server` + `internal/*` 服务层(SQLite、调度器、通知、日志总线、认证、HTTP API/SSE);前端是 Next.js(shadcn preset `b0`),通过 rewrites 代理 `/api/*`。两个容器用 docker compose 编排。

**Tech Stack:** Go(本机无 Go,**所有 go 命令都通过 `docker run golang`** 执行)、`modernc.org/sqlite`(纯 Go)、`likexian/whois`、`dlclark/regexp2`、Next.js、shadcn/ui、pnpm、Docker Compose。

**Spec:** `docs/superpowers/specs/2026-10-02-domain-scanner-web-design.md`

## Execution Notes(本次执行的实际约定)

- Go 命令包装:`docker run --rm -v "${PWD}:/src" -v gomod:/go/pkg/mod -v gobuild:/root/.cache/go-build -w /src golang:1.25 go <args>`(下文简写为 `GO <args>`)。
- 用户要求"通过后直接开发",因此本计划写完后不再等待审阅,由当前会话原生执行(superpowers:executing-plans 风格),最终再做一次整体代码审查。
- 上游文件(`internal/domain/checker.go`、`internal/reserved`、`internal/stats`、`internal/worker`、`main.go` 等)保持原样不改;所有新逻辑放新文件/新包,便于以后同步上游。
- 密钥只放 `.env`(gitignore)。测试用的 bot token / chat id 不得出现在任何被提交的文件中。

## Global Constraints

- 前端初始化命令必须是 `pnpm dlx shadcn@latest init --preset b0 --template next`,之后只用 `shadcn add` 加组件。
- `ADMIN_PASSWORD` 必填、≥8 位,否则 scanner 拒绝启动;compose 中 `${ADMIN_PASSWORD:?...}`。
- 除 `/api/health` 外所有 API(含 SSE)需要会话 cookie;scanner 的 8080 不映射到宿主机。
- 登录失败限流:同一 IP 连续 5 次失败锁定 5 分钟。
- 检测超时/出错/限流 ⇒ `unknown`,**绝不**推送、不计入可注册。
- Telegram 批量推送:每 10 秒或满 20 条;失败重试 3 次;日志与错误中不得出现 token。
- 默认公开仓库 `transflo/domain-scanner-web`,AGPL-3.0,保留上游 LICENSE。
- Go module 名保持 `domain_scanner`。

## Review Focus

1. WHOIS 返回限流/服务不可用文案(`too many requests`、`temporarily unavailable`)→ 必须判 `unknown`,不得判 available、不得推送。(Task 1 测试)
2. 非法/过于复杂的正则、空字典、非法 pattern → 创建任务返回 400,服务不得 `os.Exit`/panic。(Task 2、8 测试)
3. 超大搜索空间(如 8 位字母数字 ≈ 2.8×10¹²)→ 用 int64 计算并在超过阈值时要求 `force`,不得溢出。(Task 2 测试)
4. 字典词含大写/下划线/Unicode/过长(>63)→ 归一化为小写,非法标签跳过但仍推进游标。(Task 2 测试)
5. 续跑后不得重复写入或重复推送同一域名(`UNIQUE(job_id, domain)`;推送只针对新插入行)。(Task 3、7 测试)
6. Telegram 消息含 HTML 特殊字符/超过 4096 字符 → 转义并分片;Go http 错误含 URL(带 token)→ 必须脱敏。(Task 5 测试)
7. 删除运行中的任务 → 先取消再删,不得留下孤儿 goroutine 或写入已删任务。(Task 7 测试)

---

### Task 0: 项目脚手架与上游导入

**Files:**
- Create: `go.mod`, `go.sum`(由上游拷贝后 `go mod tidy`)、`internal/{cache,domain,generator,reserved,stats,types,worker}/*`(上游原样)、`main.go`(上游 CLI,原样)、`LICENSE`、`.gitignore`、`.env.example`、`.dockerignore`、`NOTICE.md`

**Produces:** 可编译的 module `domain_scanner`;`GO build ./...` 通过。

- [ ] 将 scratchpad 中 `upstream/` 的 `internal/`、`main.go`、`go.mod`、`go.sum`、`LICENSE` 拷贝到仓库根。
- [ ] 写 `.gitignore`(`.env`、`data/`、`node_modules`、`.next`、`*.db`、`bin/`)与 `.env.example`(`TELEGRAM_BOT_TOKEN=`、`TELEGRAM_CHAT_ID=`、`ADMIN_PASSWORD=`、`DATA_DIR=/data`)。
- [ ] 写 `NOTICE.md` 注明上游来源、许可证与改动范围。
- [ ] Run: `GO build ./...` → Expected: 成功。
- [ ] Commit: `chore: import upstream domain-scanner`

### Task 1: 带 unknown 语义的检测器

**Files:**
- Create: `internal/domain/detail.go`, `internal/domain/detail_test.go`

**Interfaces:**
- Produces:
```go
type Status string
const (StatusAvailable Status = "available"; StatusRegistered Status = "registered"; StatusReserved Status = "reserved"; StatusUnknown Status = "unknown")
type Verdict struct { Domain string; Status Status; Signatures []string; Reason string }
type Checker struct {
    LookupNS  func(string) ([]*net.NS, error)
    LookupIP  func(string) ([]net.IP, error)
    LookupMX  func(string) ([]*net.MX, error)
    Whois     func(domain string, servers ...string) (string, error)
    HasTLS    func(domain string) bool
    Retries   int           // 默认 2
    Backoff   time.Duration // 默认 1s,测试里设为 0
    Fallbacks []string      // 默认 whois.verisign-grs.com:43 等
}
func NewChecker() *Checker                       // 填充真实实现
func (c *Checker) Check(ctx context.Context, domain string) Verdict
```
- Consumes: 同包的 `isAvailableFromWHOIS`、`isUnavailableFromWHOIS`、`isServiceError`、`registeredIndicators`、`reservedIndicators`(上游未导出,同包可用)、`reserved.IsReservedDomain`。

判定顺序:reserved 规则 → 任一 DNS(NS/A/MX)命中 ⇒ registered → WHOIS(默认流程,出错则依次试 Fallbacks,各自带 Retries 与 Backoff)→ service error ⇒ unknown;registered 指示词 ⇒ registered;reserved 指示词 ⇒ reserved;available ⇒ 再做 TLS 兜底(有证书 ⇒ registered)否则 available;unavailable 指示词 ⇒ registered;其余 ⇒ unknown。全部 WHOIS 失败 ⇒ unknown。ctx 取消 ⇒ unknown(Reason="cancelled")。

- [ ] **Step 1: 写失败测试** `detail_test.go`,用注入的假函数覆盖(表驱动):
  1. DNS NS 命中 ⇒ registered,Signatures 含 `DNS_NS`,且 Whois 未被调用。
  2. WHOIS 含 `No match for "X"` + 无 DNS ⇒ available。
  3. WHOIS 含 `Registrar:` ⇒ registered。
  4. WHOIS 含 `too many requests` ⇒ **unknown**(Review Focus 1)。
  5. WHOIS 含 `temporarily unavailable` 同上。
  6. WHOIS 全部返回 error ⇒ unknown,Whois 调用次数 = (1+len(Fallbacks))×Retries。
  7. WHOIS 说 available 但 HasTLS=true ⇒ registered(Signatures 含 `SSL`)。
  8. reserved 词(如 `www.li`)⇒ reserved,不做任何网络调用。
  9. WHOIS 含 `status: reserved` ⇒ reserved。
  10. ctx 已取消 ⇒ unknown,Reason 含 `cancelled`。
  11. WHOIS 返回无法识别的文本 ⇒ unknown。
- [ ] **Step 2:** Run `GO test ./internal/domain/ -run TestChecker -v` → Expected: FAIL(未定义)。
- [ ] **Step 3:** 实现 `detail.go`。
- [ ] **Step 4:** Run 同上 → Expected: PASS。
- [ ] **Step 5:** Commit: `feat(domain): detailed checker with unknown status`

### Task 2: 可索引、可续跑的候选生成器

**Files:**
- Create: `internal/enumerate/enumerate.go`, `internal/enumerate/enumerate_test.go`

**Interfaces:**
- Produces:
```go
type Spec struct { Length int; Suffix string; Pattern string /* d|D|a */; Regex string; Words []string }
type Plan struct { /* unexported */ }
func Compile(s Spec, maxSpace int64) (*Plan, error)   // 校验;空间 > maxSpace 返回 ErrTooLarge
func (p *Plan) Total() int64                            // 原始候选总数(过滤前)
func (p *Plan) At(i int64) (domain string, ok bool)     // ok=false 表示被正则或合法性过滤,仍应推进游标
var ErrTooLarge error
```
- 规则:suffix 必须以 `.` 开头(缺失则补);Pattern 仅 `d/D/a`;字典模式下 Length/Pattern 忽略;字典词转小写,仅允许 `[a-z0-9-]`、长度 1..63、不以 `-` 开头结尾,否则 `ok=false`;正则沿用上游 `validateRegexComplexity` 同款规则(长度≤200、禁用危险模式、量词≤5)并设 100ms 超时;空间用 int64 并检测溢出。

- [ ] **Step 1: 写失败测试:**
  1. `d` 长度 2 ⇒ Total=100,`At(0)=="00.li"`,`At(99)=="99.li"`。
  2. `D` 长度 1 ⇒ Total=26;`a` 长度 2 ⇒ Total=1296。
  3. 确定性:两次 Compile 同参数,`At(i)` 全序列一致。
  4. 正则 `^1` + `d` 长度 2 ⇒ `At(10)` ok、`At(20)` !ok。
  5. 危险正则 `(a+)+` / 超长 / 量词过多 ⇒ Compile 返回错误(不 panic、不退出)。
  6. 非法 pattern `x`、Length ≤0、空 suffix ⇒ 错误。
  7. 8 位 `a`(≈2.8e12)在 maxSpace=1e9 时 ⇒ `ErrTooLarge`;30 位 `a` 溢出 ⇒ 也是 `ErrTooLarge`(不得溢出为负数)(Review Focus 3)。
  8. 字典 `["Hello","wo_rld","ok","-bad","Ünï", strings.Repeat("a",64)]`:`At` 结果依次为 `hello.li` ok、`wo_rld` !ok、`ok.li` ok、`-bad` !ok、`Ünï` !ok、64 字符 !ok;Total=6(Review Focus 4)。
  9. 空字典 ⇒ 错误。
- [ ] **Step 2:** Run `GO test ./internal/enumerate/ -v` → FAIL。
- [ ] **Step 3:** 实现。
- [ ] **Step 4:** Run → PASS。
- [ ] **Step 5:** Commit: `feat(enumerate): indexable candidate generator`

### Task 3: SQLite 存储层

**Files:**
- Create: `internal/store/store.go`, `internal/store/migrate.go`, `internal/store/store_test.go`

**Interfaces:**
- Produces:
```go
type Job struct { ID int64; Name, Suffix, Pattern, Regex, Wordlist string; Length int; DelayMS, Workers int; Status string; Cursor, Total, Checked, Available, Unknown, Registered int64; Error string; CreatedAt, UpdatedAt time.Time }
type Result struct { ID, JobID int64; Domain, Status, Signatures string; CreatedAt time.Time }
type LogEntry struct { ID, JobID int64; Level, Message string; Time time.Time }
func Open(path string) (*Store, error)  // WAL, busy_timeout, 自动迁移
func (s *Store) Close() error
// jobs
func (s *Store) CreateJob(ctx, j *Job) (int64, error)
func (s *Store) GetJob(ctx, id int64) (*Job, error)
func (s *Store) ListJobs(ctx) ([]Job, error)
func (s *Store) UpdateJobProgress(ctx, id int64, cursor, checked, available, unknown, registered int64) error
func (s *Store) SetJobStatus(ctx, id int64, status, errMsg string) error
func (s *Store) DeleteJob(ctx, id int64) error            // 级联删除 results/logs
func (s *Store) RecoverableJobs(ctx) ([]Job, error)       // status in (queued, running)
// results
func (s *Store) InsertResult(ctx, r *Result) (inserted bool, err error)   // UNIQUE(job_id,domain),重复返回 false
func (s *Store) ListResults(ctx, f ResultFilter) ([]Result, int64, error) // JobID,Status,Q,Limit,Offset
// logs
func (s *Store) InsertLogs(ctx, []LogEntry) error
func (s *Store) ListLogs(ctx, f LogFilter) ([]LogEntry, error)           // Level(最小级别),JobID,Limit,BeforeID
func (s *Store) PruneLogs(ctx, keep int) error
// settings
func (s *Store) GetSetting(ctx, key string) (string, bool, error)
func (s *Store) SetSetting(ctx, key, value string) error
func (s *Store) Stats(ctx) (Stats, error)
```

- [ ] **Step 1: 写失败测试**(`t.TempDir()` 里的真实 SQLite):CRUD 往返;`InsertResult` 同 (job,domain) 第二次返回 `inserted=false`(Review Focus 5);删除 job 级联清理 results/logs;`RecoverableJobs` 只含 queued/running;`ListResults` 的 status 与 `q` 子串过滤、分页与总数;`ListLogs` 级别过滤(info 不含 debug)与 `BeforeID` 翻页;`PruneLogs` 保留最新 N 条;Settings 读写覆盖;并发 50 个 goroutine 同时 `InsertResult` 无 `database is locked`。
- [ ] **Step 2:** Run `GO test ./internal/store/ -v` → FAIL。
- [ ] **Step 3:** 实现(`modernc.org/sqlite`,`PRAGMA journal_mode=WAL; foreign_keys=ON; busy_timeout=5000`,`SetMaxOpenConns(1)` 写串行化以保证简单正确)。
- [ ] **Step 4:** Run → PASS。
- [ ] **Step 5:** Commit: `feat(store): sqlite persistence`

### Task 4: 日志总线

**Files:**
- Create: `internal/logbus/logbus.go`, `internal/logbus/logbus_test.go`

**Interfaces:**
- Produces:
```go
type Bus struct{}
func New(sink func([]store.LogEntry), ringSize int) *Bus   // sink 可为 nil
func (b *Bus) Log(level string, jobID int64, format string, args ...any)
func (b *Bus) Subscribe() (ch <-chan store.LogEntry, cancel func())  // 带缓冲,慢订阅者丢弃而非阻塞
func (b *Bus) Recent(n int, minLevel string, jobID int64) []store.LogEntry
func (b *Bus) Close()   // flush sink
func RedactSecrets(s string, secrets ...string) string
```

- [ ] **Step 1: 写失败测试:** ring 超出容量只保留最新;Subscribe 收到之后的日志、cancel 后 channel 关闭且不泄漏;慢订阅者(不读取)不阻塞 `Log`;`Recent` 级别/任务过滤;sink 批量被调用(Close 后全部刷出);`RedactSecrets` 把 token 替换为 `***`;并发 Log+Subscribe 在 `-race` 下无竞争。
- [ ] **Step 2:** Run `GO test -race ./internal/logbus/ -v` → FAIL。
- [ ] **Step 3:** 实现。
- [ ] **Step 4:** Run → PASS。
- [ ] **Step 5:** Commit: `feat(logbus): in-memory ring with SSE fan-out`

### Task 5: Telegram 通知器

**Files:**
- Create: `internal/notifier/notifier.go`, `internal/notifier/notifier_test.go`

**Interfaces:**
- Produces:
```go
type Config struct { Token, ChatID string }
type ConfigFunc func() Config                 // 每次发送时读取,支持设置页热更新
type Notifier struct{}
func New(cfg ConfigFunc, log *logbus.Bus, opts Options) *Notifier  // Options{BaseURL string; FlushEvery time.Duration; MaxBatch int; Backoff time.Duration}
func (n *Notifier) Start(ctx context.Context)    // 后台 flush 循环
func (n *Notifier) Notify(jobName string, domains []string)  // 入队,非阻塞
func (n *Notifier) SendTest(ctx context.Context) error
func (n *Notifier) Stop()                         // flush 剩余
```
- 行为:Notify 入队;每 `FlushEvery`(默认 10s)或积累 `MaxBatch`(默认 20)触发发送,按任务名分组;文本用 HTML parse_mode 并 `html.EscapeString`;单条超 4096 字符分片;HTTP 失败重试 3 次(`Backoff`),最终失败仅 `log.Log("error",…)`;所有错误/日志经 `RedactSecrets` 清除 token;未配置 token/chat 时丢弃并 warn 一次(不阻塞)。

- [ ] **Step 1: 写失败测试**(`httptest.Server` 模拟 `https://api.telegram.org/bot<token>/sendMessage`):满 20 条立即发送一次;未满时按 FlushEvery 发送;两个任务分两条消息;含 `<script>&` 的域名/任务名被转义;5000 字符内容分成多条且每条 ≤4096;服务器前 2 次 500 后成功 ⇒ 共 3 次请求且最终成功;一直 500 ⇒ 3 次后放弃,日志含 error 且**不含 token**(Review Focus 6);未配置时不发请求;`SendTest` 成功/失败返回值;`Stop` 会 flush 剩余。
- [ ] **Step 2:** Run `GO test -race ./internal/notifier/ -v` → FAIL。
- [ ] **Step 3:** 实现。
- [ ] **Step 4:** Run → PASS。
- [ ] **Step 5:** Commit: `feat(notifier): telegram batching notifier`

### Task 6: 词库管理

**Files:**
- Create: `internal/wordlists/wordlists.go`, `internal/wordlists/wordlists_test.go`, `scripts/fetch-wordlists.sh`

**Interfaces:**
- Produces:
```go
type Info struct { ID, Name, Source string; Count int; Builtin bool }
type Manager struct{}
func NewManager(builtinDir, userDir string) *Manager
func (m *Manager) List() ([]Info, error)
func (m *Manager) Words(id string) ([]string, error)       // id 校验,防路径穿越
func (m *Manager) Save(name string, r io.Reader, maxBytes int64) (Info, error)  // 上传自定义,限制大小,清洗行
```
- 内置词表(构建期由 `fetch-wordlists.sh` 下载进 `/app/wordlists`):`google-10000-english`(first20hours/google-10000-english)、`english-words-alpha`(dwyl/english-words 的 `words_alpha.txt`)。

- [ ] **Step 1: 写失败测试:** List 同时返回内置与用户词表及计数;Words 返回去重、去空白、小写后的词;id 含 `../`、绝对路径、不存在 ⇒ 错误(路径穿越);Save 超过 maxBytes ⇒ 错误且不落盘;Save 名称被清洗为安全文件名;空文件 ⇒ 错误。
- [ ] **Step 2:** Run → FAIL。 **Step 3:** 实现并写下载脚本。 **Step 4:** Run → PASS。
- [ ] **Step 5:** Commit: `feat(wordlists): builtin and uploaded wordlists`

### Task 7: 调度器与扫描引擎

**Files:**
- Create: `internal/scheduler/scheduler.go`, `internal/scheduler/runner.go`, `internal/scheduler/scheduler_test.go`

**Interfaces:**
- Consumes: `store.*`、`logbus.Bus`、`notifier.Notifier`(通过接口 `Notifier{ Notify(job string, domains []string) }`)、`enumerate.Compile/At`、`domain.Checker`(通过接口 `Checker{ Check(ctx, domain) domain.Verdict }`)、`wordlists.Manager`(接口 `Words(id) ([]string, error)`)。
- Produces:
```go
type Params struct { Name, Suffix, Pattern, Regex, Wordlist string; Length, DelayMS, Workers int; Force bool }
type Scheduler struct{}
func New(st *store.Store, log *logbus.Bus, nf Notifier, ck Checker, wl Words, opts Options) *Scheduler  // Options{MaxParallelJobs int; MaxSpace int64; FlushEvery time.Duration}
func (s *Scheduler) Start(ctx context.Context) error     // 恢复 queued/running 任务
func (s *Scheduler) Create(ctx context.Context, p Params) (*store.Job, error)  // 校验失败返回 ErrInvalid(包装)
func (s *Scheduler) Pause(id int64) error
func (s *Scheduler) Resume(id int64) error
func (s *Scheduler) Cancel(id int64) error
func (s *Scheduler) Delete(ctx context.Context, id int64) error   // 先停再删
func (s *Scheduler) Shutdown(ctx context.Context)                 // 等待 runner 保存游标后退出
```
- 状态机:`queued→running→(paused|done|failed|cancelled)`,`paused→queued`(Resume);非法转换返回 `ErrState`。
- Runner:每个任务一个 goroutine,`Workers` 个 worker 并发检测;按序提交 `At(i)`,**游标取已连续完成的最小下标**以保证续跑不漏;`!ok` 的候选也推进游标;`available` ⇒ `InsertResult`,仅 `inserted==true` 时 `Notify`;`unknown/registered/reserved` 只计数(registered 不入 results 表,以免暴涨;unknown 入表便于排查,status=`unknown`);每 5 秒或每 50 条 `UpdateJobProgress`;`DelayMS` 为每个 worker 检测间隔;任务结束 ⇒ done 并日志汇总。

- [ ] **Step 1: 写失败测试**(假 Checker/Notifier/Words + 真 store):
  1. 小任务(`d` 长度 2,假 Checker 对偶数返回 available)跑完 ⇒ status=done、Checked=100、Available=50、results 50 条、Notify 共收到 50 个域名。
  2. Pause 后 Cursor 稳定、不再有新 Check 调用;Resume 后跑完且最终结果与不暂停一致,**无重复**(Review Focus 5)。
  3. 模拟崩溃:跑到中途 `Shutdown`,新建 Scheduler `Start` ⇒ 从保存的 cursor 续跑完成,总 Check 次数 ≤ 100+Workers,results 无重复,Notify 无重复域名。
  4. unknown 不入推送、不计 Available(Review Focus 1 的调度层面)。
  5. Create:非法正则/空字典/非法 pattern ⇒ ErrInvalid,**无 job 写入**;超大空间无 Force ⇒ ErrInvalid,Force=true 且仍超硬上限时仍拒绝(Review Focus 2、3)。
  6. Cancel ⇒ 终态 cancelled;对 done 任务 Pause ⇒ `ErrState`。
  7. Delete 运行中任务 ⇒ runner 退出后才删,之后无写入、`go test -race` 无竞争(Review Focus 7)。
  8. Checker panic 不拖垮服务:该域名记 unknown + error 日志。
- [ ] **Step 2:** Run `GO test -race ./internal/scheduler/ -v` → FAIL。
- [ ] **Step 3:** 实现。
- [ ] **Step 4:** Run → PASS。
- [ ] **Step 5:** Commit: `feat(scheduler): resumable job runner`

### Task 8: 认证与 HTTP API

**Files:**
- Create: `internal/auth/auth.go`, `internal/auth/auth_test.go`, `internal/server/server.go`, `internal/server/handlers_*.go`, `internal/server/server_test.go`

**Interfaces:**
- auth:
```go
func New(password string, secret []byte, ttl time.Duration, now func() time.Time) (*Auth, error) // 密码<8 ⇒ ErrWeakPassword
func (a *Auth) Login(ip, password string) (cookieValue string, err error)  // ErrBadPassword / ErrLocked
func (a *Auth) Verify(cookieValue string) bool
func (a *Auth) Middleware(next http.Handler, public ...string) http.Handler
func DeriveSecret(stored []byte, password string) []byte   // 口令变更使旧会话失效
```
  限流:同一 IP 5 次失败锁 5 分钟(成功清零);比较用 `subtle.ConstantTimeCompare`;cookie 为 `base64(expiry).HMAC`,`HttpOnly; SameSite=Strict; Path=/`(HTTPS 反代时 Secure 由 `X-Forwarded-Proto` 决定)。
- server:`New(deps Deps) http.Handler`,路由见规格 §4;JSON 错误格式 `{"error":"..."}`;SSE `/api/logs/stream` 发送 `event: log` 与 15 秒心跳;设置接口返回脱敏 token(`123456:****abcd`);PUT 设置时若 token 为脱敏占位符则保持原值;请求体大小限制。

- [ ] **Step 1: 写失败测试:**
  - auth:弱口令 ⇒ ErrWeakPassword;正确口令 ⇒ cookie 验证通过;错口令 ⇒ ErrBadPassword;连续 5 次失败后正确口令也返回 ErrLocked,时间前进 5 分钟后恢复;篡改 cookie/过期 cookie ⇒ Verify 失败;口令变更后旧 cookie 失效;Middleware 对 public 路径放行、其余 401。
  - server(`httptest` + 真 store + 假 scheduler 依赖):未登录访问 `/api/jobs`、`/api/logs/stream` ⇒ 401;`/api/health` ⇒ 200;登录后创建任务 ⇒ 201;非法正则 ⇒ 400;`/api/results/export` 返回 CSV 头与内容;`GET /api/settings` 响应中不含完整 token;`PUT` 脱敏占位符不覆盖原 token;SSE 能收到新日志;404/405 为 JSON。
- [ ] **Step 2:** Run `GO test -race ./internal/auth/ ./internal/server/ -v` → FAIL。
- [ ] **Step 3:** 实现。
- [ ] **Step 4:** Run → PASS。
- [ ] **Step 5:** Commit: `feat(server): auth and REST/SSE api`

### Task 9: 服务入口与配置

**Files:**
- Create: `cmd/server/main.go`, `cmd/server/config.go`, `cmd/server/config_test.go`

- 配置:`LISTEN_ADDR`(默认 `:8080`)、`DATA_DIR`(默认 `/data`)、`ADMIN_PASSWORD`、`TELEGRAM_BOT_TOKEN`、`TELEGRAM_CHAT_ID`、`WORDLIST_DIR`(默认 `/app/wordlists`)、`MAX_PARALLEL_JOBS`(默认 2)。
- 启动序:加载配置(`ADMIN_PASSWORD` 校验失败 ⇒ 打印明确错误并 `exit 1`)→ 打开 store → 读/生成 secret(口令派生)→ 组装 logbus/notifier/scheduler/server → `scheduler.Start`(恢复任务)→ 监听;SIGTERM 优雅关停(`Shutdown` 保存游标、flush 通知)。
- Telegram 配置优先级:settings 表 > 环境变量;首次启动若 settings 为空则不写入(仅运行时回落到 env)。
- `/api/health` 的 `{"ok":true}`;Docker healthcheck 用它。

- [ ] **Step 1: 写失败测试**(`config_test.go`):空口令/7 位口令 ⇒ 错误含 `ADMIN_PASSWORD`;8 位通过;默认值;env 解析错误的 `MAX_PARALLEL_JOBS`(如 `abc`)⇒ 错误。
- [ ] **Step 2:** Run `GO test ./cmd/server/ -v` → FAIL。 **Step 3:** 实现 `config.go` 与 `main.go`。 **Step 4:** Run → PASS 并 `GO build ./...`。
- [ ] **Step 5:** Commit: `feat(server): entrypoint and config`

### Task 10: scanner 镜像与后端冒烟

**Files:**
- Create: `Dockerfile.scanner`, `docker-compose.yml`(先含 scanner,web 在 Task 13 加入)、`.dockerignore`

- `Dockerfile.scanner`:`golang:1.25` 构建 `CGO_ENABLED=0` 的 `/out/scanner`;运行镜像 `alpine`(含 `ca-certificates`、`curl`、`wget`),`RUN scripts/fetch-wordlists.sh`(构建期下载词表),`/data` 为 VOLUME,非 root 用户运行,HEALTHCHECK 调 `/api/health`。
- compose:`ADMIN_PASSWORD: ${ADMIN_PASSWORD:?请在 .env 中设置 ADMIN_PASSWORD}`、`env_file: .env`、named volume `scanner-data`、`restart: unless-stopped`。

- [ ] **Step 1:** `GO test -race ./...` → 全部 PASS。
- [ ] **Step 2:** 不带 `ADMIN_PASSWORD` 运行 `docker compose config` / `up` ⇒ 失败并提示(验证强制口令)。
- [ ] **Step 3:** 写临时 `.env`(`ADMIN_PASSWORD` 随机、Telegram 值来自用户消息),`docker compose up -d --build scanner`,等待 healthy。
- [ ] **Step 4:** curl 冒烟:未登录 401;登录;创建 `.li` `d` 长度 2 的任务;轮询到 done;`/api/results`、`/api/logs` 有数据;`docker compose logs scanner` 无 panic。
- [ ] **Step 5:** Commit: `feat(docker): scanner image and compose`

### Task 11: 前端脚手架(shadcn b0)

**Files:**
- Create: `web/`(由命令生成)

- [ ] **Step 1:** 在仓库根运行 `pnpm dlx shadcn@latest init --preset b0 --template next`(目标目录 `web`;若命令交互则按其提示选择,不得改动 preset),确认生成 `components.json`、`app/globals.css` 主题变量与 preset 一致。
- [ ] **Step 2:** `pnpm dlx shadcn@latest add button card input label table badge tabs sheet dialog select switch sonner progress separator scroll-area dropdown-menu sidebar skeleton tooltip textarea checkbox alert`。
- [ ] **Step 3:** `pnpm build` 成功。
- [ ] **Step 4:** 配置 `next.config`:`output: "standalone"`,`rewrites` 把 `/api/:path*` 代理到 `process.env.SCANNER_URL ?? "http://localhost:8080"`。
- [ ] **Step 5:** Commit: `feat(web): scaffold next + shadcn b0`

### Task 12: 前端页面

**Files:**
- Create: `web/middleware.ts`、`web/lib/api.ts`(类型化 fetch + 401 跳转)、`web/lib/types.ts`、`web/app/login/page.tsx`、`web/app/(app)/layout.tsx`(侧边栏)、`web/app/(app)/page.tsx`(仪表盘)、`web/app/(app)/jobs/page.tsx`、`web/app/(app)/results/page.tsx`、`web/app/(app)/logs/page.tsx`、`web/app/(app)/settings/page.tsx`,以及 `web/components/*`(job-form、log-viewer 等)。

- 只使用 shadcn 组件与其 token(`bg-background`、`text-muted-foreground` 等),不自写与主题冲突的颜色。
- `middleware.ts`:无会话 cookie 访问非 `/login` 页面 ⇒ 重定向 `/login`;API 401 时客户端同样跳转。
- 登录页:Card + Input(type=password) + Button,错误与锁定提示。
- 仪表盘:统计卡片(运行中任务、累计可注册、已检查、速率)、后端连接状态徽章、最近任务。
- 任务页:列表(状态 Badge、Progress、操作按钮)+ 新建 Sheet 表单(后缀、模式、长度、正则、词库选择/上传、delay、workers、force);暂停/继续/取消/删除(删除带确认 Dialog)。
- 结果页:Table + 搜索 + 状态/任务筛选 + 复制 + 导出 CSV + 分页。
- 日志页:EventSource 实时流;级别 Select、任务 Select、暂停滚动 Switch、清屏、下载;ScrollArea 自动滚底;断线自动重连并提示。
- 设置页:Telegram token(脱敏显示)/chat id、保存、"发送测试消息"按钮与结果 toast。
- 所有请求失败 ⇒ sonner toast 显示后端 `error`。

- [ ] **Step 1:** 先实现 `lib/api.ts`、类型与 `middleware.ts`;`pnpm build` 通过。
- [ ] **Step 2:** 逐页实现,每页完成后 `pnpm build` 与 `pnpm lint` 通过。
- [ ] **Step 3:** Commit(按页面分多次提交)。

### Task 13: web 镜像与整体联调

**Files:**
- Create: `web/Dockerfile`、`web/.dockerignore`;Modify: `docker-compose.yml`

- `web/Dockerfile`:`node` 多阶段 + pnpm,standalone 输出,非 root。
- compose:`web` 服务映射 `3000:3000`,`SCANNER_URL=http://scanner:8080`,`depends_on` scanner healthy;scanner **不**映射端口。

- [ ] **Step 1:** `docker compose up -d --build`,两个服务 healthy。
- [ ] **Step 2(浏览器自动化,Playwright/Chrome MCP):** 未登录访问 `/` ⇒ 重定向 `/login`;错误口令提示;登录后遍历仪表盘/任务/结果/日志/设置;新建任务(`.li`、`d`、2 位)并看到进度、日志实时滚动、结果出现;检查浏览器 console 无报错、network 无 4xx/5xx(401 之外);截图确认 shadcn b0 主题生效。
- [ ] **Step 3:** 修复发现的所有问题并重复验证。
- [ ] **Step 4:** Commit: `feat: web image and compose wiring`

### Task 14: Telegram 实测、续跑验证、收尾与上传

**Files:**
- Create: `README.md`、`README.zh.md`(合并说明,保留上游署名)、`LICENSE`(已有)

- [ ] **Step 1:** 在设置页保存 token/chat id,点"发送测试消息",**真实**发送到会话 <CHAT_ID>,确认 API 返回 ok(无法在聊天端亲眼看到时,以 Telegram API 的 `ok:true` 与 message_id 为证据并如实说明)。
- [ ] **Step 2:** 创建一个会命中的小任务(用字典或随机 `.li` 短 + 实际存在可注册结果的范围,必要时临时用 `unknown`→校验链路),确认命中后收到 Telegram 推送并在日志中可见。
- [ ] **Step 3:** 任务运行中 `docker compose restart scanner`,确认任务自动续跑且结果无重复。
- [ ] **Step 4:** 密钥泄漏扫描:`git grep` 与 `git log -p` 搜索 token 片段(`<BOT_ID>`、`<TOKEN_PREFIX>`)、chat id `<CHAT_ID>`、`.env`;任何命中都必须清除后再继续。
- [ ] **Step 5:** 写 README:快速开始(`cp .env.example .env`、填写必填项、`docker compose up -d`)、配置表、安全说明(口令保护、不暴露 8080)、与上游的差异、AGPL 说明、故障排查(日志页用法)。
- [ ] **Step 6:** `gh repo create transflo/domain-scanner-web --public --source . --remote origin --push`(公开仓库,已获用户确认默认公开),推送后用 `gh repo view` 与克隆检查 `.env` 未被提交。
- [ ] **Step 7:** 整体代码审查(`superpowers:requesting-code-review` 或 `code-review` skill),修复发现的问题后再推送最终提交。
