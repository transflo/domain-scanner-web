"use client"

import { Suspense, useEffect, useMemo, useRef, useState } from "react"
import { useSearchParams } from "next/navigation"
import {
  ArrowDownToLineIcon,
  ChevronDownIcon,
  ChevronRightIcon,
  DownloadIcon,
  SearchIcon,
  Trash2Icon,
} from "lucide-react"

import { DiagnosticsPanel } from "@/components/diagnostics-panel"
import { SelectField } from "@/components/field"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card, CardContent } from "@/components/ui/card"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Skeleton } from "@/components/ui/skeleton"
import { Switch } from "@/components/ui/switch"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { usePoll } from "@/hooks/use-poll"
import { api } from "@/lib/api"
import { fmtClock, levelLabel } from "@/lib/format"
import type { LogEntry, LogFilter, LogLevel } from "@/lib/types"
import { cn } from "@/lib/utils"

const MAX_ENTRIES = 2000
const HISTORY = 300

const levelItems = (["debug", "info", "warn", "error"] as LogLevel[]).map((l) => ({
  value: l,
  label: `${levelLabel[l]}及以上`,
}))

const levelClass: Record<LogLevel, string> = {
  debug: "text-muted-foreground",
  info: "text-foreground",
  warn: "text-amber-600 dark:text-amber-400",
  error: "text-destructive",
}

type Conn = "connecting" | "live" | "retrying"

/** Merge by id, keep chronological order, cap the size. */
function merge(prev: LogEntry[], incoming: LogEntry[]): LogEntry[] {
  if (incoming.length === 0) return prev
  const seen = new Set(prev.map((e) => e.id))
  const fresh = incoming.filter((e) => !seen.has(e.id))
  if (fresh.length === 0) return prev
  const all = [...prev, ...fresh].sort((a, b) => a.id - b.id)
  return all.length > MAX_ENTRIES ? all.slice(all.length - MAX_ENTRIES) : all
}

function useDebounced(value: string, ms = 350): string {
  const [v, setV] = useState(value)
  useEffect(() => {
    const t = setTimeout(() => setV(value), ms)
    return () => clearTimeout(t)
  }, [value, ms])
  return v
}

/** One log line; clicking it shows the structured details. */
function LogLine({ e, open, onToggle }: { e: LogEntry; open: boolean; onToggle: () => void }) {
  const hasDetail = !!(e.fields && Object.keys(e.fields).length) || !!e.domain || !!e.egress || !!e.duration_ms || !!e.event
  return (
    <div data-testid="log-line" className={cn("rounded-sm", open && "bg-muted")}>
      <button
        type="button"
        onClick={onToggle}
        aria-expanded={open}
        className={cn(
          "flex w-full min-w-0 items-start gap-2 rounded-sm px-1 py-0.5 text-left hover:bg-muted/70 pointer-coarse:py-2",
          levelClass[e.level],
        )}
      >
        <ChevronRightIcon
          className={cn("mt-0.5 size-3 shrink-0 text-muted-foreground transition-transform", open && "rotate-90", !hasDetail && "opacity-30")}
        />
        <span className="shrink-0 text-muted-foreground">{fmtClock(e.time)}</span>
        <span className="w-10 shrink-0 font-semibold uppercase">{e.level}</span>
        {e.component && <span className="hidden shrink-0 text-muted-foreground sm:inline">[{e.component}]</span>}
        {e.job_id > 0 && <span className="shrink-0 text-muted-foreground">#{e.job_id}</span>}
        <span className="min-w-0 whitespace-pre-wrap break-all">{e.message}</span>
      </button>
      {open && (
        <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-0.5 px-6 pb-2 text-[11px] text-muted-foreground" data-testid="log-detail">
          <dt>时间</dt>
          <dd className="break-all text-foreground">{e.time}</dd>
          <dt>组件 / 事件</dt>
          <dd className="break-all text-foreground">
            {e.component || "—"} / {e.event || "—"}
          </dd>
          {e.domain && (
            <>
              <dt>域名</dt>
              <dd className="break-all text-foreground">{e.domain}</dd>
            </>
          )}
          {e.egress && (
            <>
              <dt>出站</dt>
              <dd className="break-all text-foreground">{e.egress}</dd>
            </>
          )}
          {e.duration_ms ? (
            <>
              <dt>耗时</dt>
              <dd className="text-foreground">{e.duration_ms} ms</dd>
            </>
          ) : null}
          {e.fields && Object.keys(e.fields).length > 0 && (
            <>
              <dt>字段</dt>
              <dd>
                <pre className="overflow-x-auto whitespace-pre-wrap break-all text-foreground">{JSON.stringify(e.fields, null, 2)}</pre>
              </dd>
            </>
          )}
        </dl>
      )}
    </div>
  )
}

function LiveLogs() {
  const params = useSearchParams()
  const [level, setLevel] = useState<LogLevel>("info")
  const [jobId, setJobId] = useState(params.get("job") ?? "all")
  const [component, setComponent] = useState("all")
  const [domain, setDomain] = useState("")
  const [egress, setEgress] = useState("")
  const [text, setText] = useState("")
  const [entries, setEntries] = useState<LogEntry[]>([])
  const [paused, setPaused] = useState(false)
  const [autoScroll, setAutoScroll] = useState(true)
  const [conn, setConn] = useState<Conn>("connecting")
  const [buffered, setBuffered] = useState(0)
  const [openIds, setOpenIds] = useState<Set<number>>(new Set())

  const dDomain = useDebounced(domain.trim().toLowerCase())
  const dEgress = useDebounced(egress.trim())
  const dText = useDebounced(text.trim())

  const pausedRef = useRef(false)
  const bufferRef = useRef<LogEntry[]>([])
  const boxRef = useRef<HTMLDivElement>(null)

  const jobs = usePoll(api.jobs, 15_000)
  const components = usePoll(api.logComponents, 30_000)
  const jobItems = [
    { value: "all", label: "全部任务" },
    ...(jobs.data ?? []).map((j) => ({ value: String(j.id), label: `#${j.id} ${j.name}` })),
  ]
  if (jobId !== "all" && !jobItems.some((i) => i.value === jobId)) {
    jobItems.push({ value: jobId, label: `#${jobId}` })
  }
  const componentItems = [{ value: "all", label: "全部组件" }, ...(components.data ?? []).map((c) => ({ value: c, label: c }))]

  const filter: LogFilter = useMemo(
    () => ({
      level,
      job_id: jobId === "all" ? undefined : Number(jobId),
      component: component === "all" ? undefined : component,
      domain: dDomain || undefined,
      egress: dEgress || undefined,
      q: dText || undefined,
    }),
    [level, jobId, component, dDomain, dEgress, dText],
  )
  const filterKey = JSON.stringify(filter)

  // History first, then the live stream. Re-runs whenever a filter changes.
  useEffect(() => {
    let cancelled = false
    let source: EventSource | undefined
    const f = JSON.parse(filterKey) as LogFilter

    async function init() {
      let history: LogEntry[] = []
      try {
        history = await api.logs({ ...f, limit: HISTORY })
      } catch {
        // the stream below still works; the retry badge shows connectivity problems
      }
      if (cancelled) return
      bufferRef.current = []
      setBuffered(0)
      setOpenIds(new Set())
      setEntries(merge([], history))

      source = new EventSource(api.streamUrl(f))
      source.onopen = () => {
        if (!cancelled) setConn("live")
      }
      source.onerror = () => {
        if (!cancelled) setConn("retrying")
      }
      source.addEventListener("log", (ev) => {
        const entry = JSON.parse((ev as MessageEvent<string>).data) as LogEntry
        if (pausedRef.current) {
          bufferRef.current.push(entry)
          setBuffered(bufferRef.current.length)
        } else {
          setEntries((prev) => merge(prev, [entry]))
        }
      })
    }
    void init()
    return () => {
      cancelled = true
      source?.close()
      setConn("connecting")
    }
  }, [filterKey])

  useEffect(() => {
    if (autoScroll && boxRef.current) boxRef.current.scrollTop = boxRef.current.scrollHeight
  }, [entries, autoScroll])

  function togglePause(next: boolean) {
    pausedRef.current = next
    setPaused(next)
    if (!next && bufferRef.current.length > 0) {
      const pending = bufferRef.current
      bufferRef.current = []
      setBuffered(0)
      setEntries((prev) => merge(prev, pending))
    }
  }

  function toggleOpen(id: number) {
    setOpenIds((s) => {
      const n = new Set(s)
      if (n.has(id)) n.delete(id)
      else n.add(id)
      return n
    })
  }

  return (
    <>
      <div className="flex flex-wrap items-center gap-2" data-testid="log-filters">
        <div className="w-full min-w-0 sm:w-40">
          <SelectField label="日志级别" value={level} onChange={(v) => setLevel(v as LogLevel)} options={levelItems} />
        </div>
        <div className="w-full min-w-0 sm:w-48">
          <SelectField label="任务" value={jobId} onChange={setJobId} options={jobItems} />
        </div>
        <div className="w-full min-w-0 sm:w-40">
          <SelectField label="组件" value={component} onChange={setComponent} options={componentItems} />
        </div>
        <div className="flex w-full min-w-0 flex-col gap-2 sm:w-44">
          <Label htmlFor="log-domain">域名</Label>
          <Input id="log-domain" value={domain} onChange={(e) => setDomain(e.target.value)} placeholder="完整域名" className="font-mono" />
        </div>
        <div className="flex w-full min-w-0 flex-col gap-2 sm:w-36">
          <Label htmlFor="log-egress">出站</Label>
          <Input id="log-egress" value={egress} onChange={(e) => setEgress(e.target.value)} placeholder="direct / proxy-1" className="font-mono" />
        </div>
        <div className="flex w-full min-w-0 flex-col gap-2 sm:w-52">
          <Label htmlFor="log-q">关键字</Label>
          <div className="relative">
            <SearchIcon className="pointer-events-none absolute top-1/2 left-2.5 size-4 -translate-y-1/2 text-muted-foreground" />
            <Input id="log-q" value={text} onChange={(e) => setText(e.target.value)} placeholder="搜索消息 / 事件" className="pl-8" />
          </div>
        </div>
      </div>

      <div className="flex flex-wrap items-center gap-3">
        <Badge variant={conn === "live" ? "secondary" : conn === "retrying" ? "destructive" : "outline"} data-testid="log-conn">
          {conn === "live" ? "已连接" : conn === "retrying" ? "重连中…" : "连接中…"}
        </Badge>
        <div className="flex items-center gap-2">
          <Switch id="log-pause" checked={paused} onCheckedChange={togglePause} />
          <Label htmlFor="log-pause">暂停{paused && buffered > 0 ? `(${buffered} 条待显示)` : ""}</Label>
        </div>
        <div className="flex items-center gap-2">
          <Switch id="log-follow" checked={autoScroll} onCheckedChange={setAutoScroll} />
          <Label htmlFor="log-follow">自动滚动</Label>
        </div>
        <div className="flex flex-wrap gap-2 sm:ml-auto">
          <Button variant="outline" size="sm" onClick={() => setAutoScroll(true)}>
            <ArrowDownToLineIcon data-icon="inline-start" />
            跳到最新
          </Button>
          <Button variant="outline" size="sm" onClick={() => setEntries([])}>
            <Trash2Icon data-icon="inline-start" />
            清屏
          </Button>
          <DropdownMenu>
            <DropdownMenuTrigger render={<Button variant="outline" size="sm" />}>
              <DownloadIcon data-icon="inline-start" />
              导出
              <ChevronDownIcon data-icon="inline-end" />
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end" className="w-64">
              <DropdownMenuGroup>
                <DropdownMenuLabel>服务器上保存的全部历史(所有级别)</DropdownMenuLabel>
                <DropdownMenuItem render={<a href={api.logExportUrl({}, "text")} download />}>全部日志 · 文本 (.log)</DropdownMenuItem>
                <DropdownMenuItem render={<a href={api.logExportUrl({}, "jsonl")} download />}>全部日志 · JSONL(含结构化字段)</DropdownMenuItem>
              </DropdownMenuGroup>
              <DropdownMenuSeparator />
              <DropdownMenuGroup>
                <DropdownMenuLabel>仅当前筛选条件</DropdownMenuLabel>
                <DropdownMenuItem render={<a href={api.logExportUrl(filter, "text")} download />}>当前筛选 · 文本 (.log)</DropdownMenuItem>
                <DropdownMenuItem render={<a href={api.logExportUrl(filter, "jsonl")} download />}>当前筛选 · JSONL</DropdownMenuItem>
              </DropdownMenuGroup>
            </DropdownMenuContent>
          </DropdownMenu>
        </div>
      </div>

      <Card className="min-h-0 flex-1">
        <CardContent className="min-h-0 flex-1">
          <div
            ref={boxRef}
            data-testid="log-box"
            className="h-[calc(100svh-22rem)] min-h-64 overflow-auto rounded-md bg-muted/40 p-2 font-mono text-xs leading-relaxed"
          >
            {conn === "connecting" && entries.length === 0 ? (
              <Skeleton className="h-full w-full" />
            ) : entries.length === 0 ? (
              <p className="p-1 text-muted-foreground">暂无日志。</p>
            ) : (
              entries.map((e) => <LogLine key={e.id} e={e} open={openIds.has(e.id)} onToggle={() => toggleOpen(e.id)} />)
            )}
          </div>
        </CardContent>
      </Card>
    </>
  )
}

function LogView() {
  return (
    <>
      <div>
        <h1 className="text-xl font-semibold">运行日志</h1>
        <p className="text-sm text-muted-foreground">
          每个检查步骤都会记录(DNS、RDAP、WHOIS、出站切换、Cloudflare 核查、Telegram 推送)。点击一行展开结构化字段。
        </p>
      </div>
      <Tabs defaultValue="live" className="min-w-0">
        <TabsList>
          <TabsTrigger value="live">实时日志</TabsTrigger>
          <TabsTrigger value="diag">诊断统计</TabsTrigger>
        </TabsList>
        <TabsContent value="live" className="mt-3 flex min-w-0 flex-col gap-3">
          <LiveLogs />
        </TabsContent>
        <TabsContent value="diag" className="mt-3 flex min-w-0 flex-col gap-3">
          <DiagnosticsPanel />
        </TabsContent>
      </Tabs>
    </>
  )
}

export default function LogsPage() {
  return (
    <Suspense fallback={<Skeleton className="h-64 w-full" />}>
      <LogView />
    </Suspense>
  )
}
