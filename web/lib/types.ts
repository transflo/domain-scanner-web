export type JobStatus =
  | "queued"
  | "running"
  | "paused"
  | "done"
  | "failed"
  | "cancelled"

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
}

export type ResultStatus = "available" | "unknown"

export interface ResultRow {
  id: number
  job_id: number
  domain: string
  status: ResultStatus
  signatures: string
  created_at: string
}

export type LogLevel = "debug" | "info" | "warn" | "error"

export interface LogEntry {
  id: number
  job_id: number
  level: LogLevel
  message: string
  time: string
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
}

export interface ResultFilter {
  job_id?: number
  status?: string
  q?: string
  limit?: number
  offset?: number
}
