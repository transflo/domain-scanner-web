export type JobStatus =
  | "queued"
  | "running"
  | "paused"
  | "done"
  | "failed"
  | "cancelled"

export type EgressMode = "direct" | "proxy" | "pool"

export interface Job {
  id: number
  name: string
  suffix: string
  pattern: string
  regex: string
  wordlist: string
  length: number
  delay_ms: number
  workers: number
  use_reserved: boolean
  egress_mode: EgressMode
  proxy_id: number
  failover: boolean
  status: JobStatus
  cursor: number
  total: number
  checked: number
  available: number
  unknown: number
  registered: number
  error: string
  created_at: string
  updated_at: string
}

export interface JobParams {
  name?: string
  suffix: string
  pattern?: string
  length?: number
  regex?: string
  wordlist?: string
  delay_ms?: number
  workers?: number
  use_reserved?: boolean
  force?: boolean
  egress_mode?: EgressMode
  proxy_id?: number
  failover?: boolean
}

export type ResultStatus = "available" | "unknown"
export type CFStatus = "" | "pending" | "confirmed" | "rejected" | "unsupported" | "error"
export type RegisterStatus = "" | "registering" | "succeeded" | "failed"

export interface ResultRow {
  id: number
  job_id: number
  domain: string
  status: ResultStatus
  signatures: string
  created_at: string
  cf_status: CFStatus
  cf_reason: string
  cf_price: string
  cf_currency: string
  cf_checked_at?: string
  register_status: RegisterStatus
  register_note: string
}

export type LogLevel = "debug" | "info" | "warn" | "error"

export interface LogEntry {
  id: number
  job_id: number
  level: LogLevel
  component: string
  event: string
  message: string
  domain?: string
  egress?: string
  duration_ms?: number
  fields?: Record<string, unknown>
  time: string
}

export interface LogFilter {
  level?: string
  job_id?: number
  component?: string
  event?: string
  domain?: string
  egress?: string
  q?: string
  limit?: number
  before_id?: number
}

export interface Stats {
  jobs: number
  running_jobs: number
  checked: number
  available: number
  unknown: number
  registered: number
}

export interface WordlistInfo {
  id: string
  name: string
  source: string
  count: number
  builtin: boolean
}

export interface Settings {
  telegram_token: string
  telegram_chat_id: string
  telegram_configured: boolean
  telegram_source: "" | "env" | "settings"
  cloudflare_account_id: string
  cloudflare_token: string
  cloudflare_configured: boolean
  register_confirm: boolean
  register_max_price: string
  register_daily_cap: number
  push_unconfirmed: boolean
  log_level: LogLevel
  proxy_test_url: string
}

export type SettingsUpdate = Partial<{
  telegram_token: string
  telegram_chat_id: string
  cloudflare_account_id: string
  cloudflare_token: string
  register_confirm: boolean
  register_max_price: string
  register_daily_cap: number
  push_unconfirmed: boolean
  log_level: LogLevel
  proxy_test_url: string
}>

export interface ResultFilter {
  job_id?: number
  status?: string
  cf_status?: string
  q?: string
  limit?: number
  offset?: number
}

export interface EgressStatus {
  id: string
  name: string
  direct: boolean
  enabled: boolean
  healthy: boolean
  tested: boolean
  strikes: number
  penalized_until?: string
}

export interface ProxyStatus {
  xray_available: boolean
  running: boolean
  error: string
  egresses: EgressStatus[]
}

export interface Outbound {
  id: number
  name: string
  protocol: string
  address: string
  port: number
  transport: string
  security: string
  enabled: boolean
  last_test_at?: string
  last_ok: boolean
  last_delay_ms: number
  last_error: string
  last_ip: string
  last_country: string
  created_at: string
}

export interface OutboundDetail extends Outbound {
  config: Record<string, unknown>
  reload_error?: string
}

export interface ProbeResult {
  ok: boolean
  delay_ms: number
  cold_ms: number
  status: number
  ip: string
  country: string
  error: string
}

export interface Imported {
  name: string
  protocol: string
  address: string
  port: number
  config: Record<string, unknown>
}

export interface ImportError {
  line: number
  input: string
  message: string
}

export interface ImportResult {
  items: Imported[]
  errors: ImportError[]
  saved: number[]
  skipped: number
  reload_error?: string
}

export interface EgressDiag {
  egress: string
  checks: number
  available: number
  registered: number
  unknown: number
  rate_limited: number
  timeouts: number
  network_errors: number
  throttles: number
  storms: number
  avg_ms: number
  max_ms: number
  success_rate: number
}

export interface TLDDiag {
  tld: string
  checks: number
  available: number
  registered: number
  unknown: number
}

export interface Diagnostics {
  hours: number
  diagnostics: { since: string; checks: number; by_egress: EgressDiag[] | null; by_tld: TLDDiag[] | null }
  logs: { count: number; oldest: string; newest: string }
  egresses: EgressStatus[]
}

export interface StoragePolicy {
  DebugDays: number
  InfoDays: number
  WarnDays: number
  MaxDebugRows: number
  MaxOtherRows: number
  MaxDBBytes: number
  UnknownResultDays: number
  MinFreeBytes: number
}

export interface StorageReport {
  at: string
  state: "" | "ok" | "low" | "critical"
  level_forced: boolean
  disk_free_bytes: number
  disk_total_bytes: number
  db_bytes: number
  db_used_bytes: number
  wal_bytes: number
  auto_vacuum: number
  logs: Record<string, number> | null
  results: Record<string, number> | null
  jobs: number
  deleted_logs: number
  deleted_unknown: number
  policy: StoragePolicy
  error?: string
}
