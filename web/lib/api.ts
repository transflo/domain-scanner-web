import type {
  Diagnostics,
  EgressStatus,
  ImportResult,
  Job,
  JobParams,
  LogEntry,
  LogFilter,
  Outbound,
  OutboundDetail,
  ProbeResult,
  ProxyStatus,
  ResultFilter,
  ResultRow,
  Settings,
  SettingsUpdate,
  Stats,
  WordlistInfo,
} from "@/lib/types"

export class ApiError extends Error {
  status: number
  constructor(message: string, status: number) {
    super(message)
    this.name = "ApiError"
    this.status = status
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  let res: Response
  try {
    res = await fetch(`/api${path}`, { credentials: "same-origin", ...init })
  } catch {
    throw new ApiError("无法连接到服务,请检查网络或后端是否运行", 0)
  }
  // Any 401 outside the auth endpoints means the session is gone: back to the login page.
  if (res.status === 401 && !path.startsWith("/auth/") && typeof window !== "undefined") {
    // Full page load on purpose: it drops all client state of the dead session.
    // eslint-disable-next-line @next/next/no-location-assign-relative-destination
    window.location.href = "/login"
    throw new ApiError("登录已失效", 401)
  }
  if (!res.ok) {
    const body = (await res.json().catch(() => null)) as { error?: string } | null
    throw new ApiError(body?.error ?? `请求失败(HTTP ${res.status})`, res.status)
  }
  if (res.status === 204) return undefined as T
  return (await res.json()) as T
}

const json = (method: string, body?: unknown): RequestInit => ({
  method,
  headers: body === undefined ? undefined : { "Content-Type": "application/json" },
  body: body === undefined ? undefined : JSON.stringify(body),
})

function query(params: Record<string, string | number | undefined>): string {
  const q = new URLSearchParams()
  for (const [k, v] of Object.entries(params)) {
    if (v !== undefined && v !== "" && v !== 0) q.set(k, String(v))
  }
  const s = q.toString()
  return s ? `?${s}` : ""
}

export const api = {
  // auth
  login: (password: string) => request<{ ok: true }>("/auth/login", json("POST", { password })),
  logout: () => request<{ ok: true }>("/auth/logout", json("POST")),
  me: () => request<{ authenticated: boolean }>("/auth/me"),
  health: () => request<{ ok: boolean }>("/health"),

  // dashboard
  stats: () => request<Stats>("/stats"),

  // jobs
  jobs: () => request<{ items: Job[] }>("/jobs").then((r) => r.items),
  createJob: (p: JobParams) => request<Job>("/jobs", json("POST", p)),
  jobAction: (id: number, action: "pause" | "resume" | "cancel") =>
    request<Job>(`/jobs/${id}/${action}`, json("POST")),
  deleteJob: (id: number) => request<void>(`/jobs/${id}`, json("DELETE")),

  // results
  results: (f: ResultFilter) =>
    request<{ items: ResultRow[]; total: number }>(`/results${query({ ...f })}`),
  exportUrl: (f: ResultFilter) => `/api/results/export${query({ ...f, limit: undefined, offset: undefined })}`,

  // logs
  logs: (p: LogFilter) => request<{ items: LogEntry[] }>(`/logs${query({ ...p })}`).then((r) => r.items),
  logComponents: () => request<{ items: string[] }>("/logs/components").then((r) => r.items),
  streamUrl: (p: LogFilter) => `/api/logs/stream${query({ ...p, limit: undefined, before_id: undefined })}`,
  logExportUrl: (p: LogFilter) => `/api/logs/export${query({ ...p, limit: undefined, before_id: undefined })}`,
  diagnostics: (hours: number) => request<Diagnostics>(`/diagnostics${query({ hours })}`),

  // outbound proxies
  outbounds: () => request<{ items: Outbound[]; status: ProxyStatus }>("/outbounds"),
  outbound: (id: number) => request<OutboundDetail>(`/outbounds/${id}`),
  createOutbound: (b: { name?: string; config: string; enabled?: boolean }) =>
    request<OutboundDetail>("/outbounds", json("POST", b)),
  updateOutbound: (id: number, b: { name?: string; config?: string; enabled?: boolean }) =>
    request<OutboundDetail>(`/outbounds/${id}`, json("PUT", b)),
  deleteOutbound: (id: number) => request<void>(`/outbounds/${id}`, json("DELETE")),
  importOutbounds: (text: string, save: boolean) =>
    request<ImportResult>("/outbounds/import", json("POST", { text, save })),
  testOutbound: (id: number) => request<ProbeResult>(`/outbounds/${id}/test`, json("POST")),
  testAllOutbounds: () =>
    request<{ results: Record<string, ProbeResult> }>("/outbounds/test-all", json("POST")).then((r) => r.results),
  testOutboundConfig: (config: string) => request<ProbeResult>("/outbounds/test-config", json("POST", { config })),
  reloadOutbounds: () => request<ProxyStatus>("/outbounds/reload", json("POST")),
  egresses: () => request<{ items: EgressStatus[] }>("/egresses").then((r) => r.items),

  // wordlists
  wordlists: () => request<{ items: WordlistInfo[] }>("/wordlists").then((r) => r.items),
  uploadWordlist: (name: string, file: File) => {
    const form = new FormData()
    form.set("name", name)
    form.set("file", file)
    return request<WordlistInfo>("/wordlists", { method: "POST", body: form })
  },

  // settings
  settings: () => request<Settings>("/settings"),
  saveSettings: (s: SettingsUpdate) => request<Settings>("/settings", json("PUT", s)),
  testTelegram: () => request<{ ok: true }>("/settings/telegram/test", json("POST")),
  testCloudflare: () => request<{ ok: true }>("/settings/cloudflare/test", json("POST")),
}
