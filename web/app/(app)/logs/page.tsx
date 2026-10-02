"use client"

import { Suspense, useEffect, useMemo, useRef, useState } from "react"
import { useSearchParams } from "next/navigation"
import { ArrowDownToLineIcon, DownloadIcon, Trash2Icon } from "lucide-react"

import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card, CardContent } from "@/components/ui/card"
import { Label } from "@/components/ui/label"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { Skeleton } from "@/components/ui/skeleton"
import { Switch } from "@/components/ui/switch"
import { usePoll } from "@/hooks/use-poll"
import { api } from "@/lib/api"
import { fmtClock, levelLabel } from "@/lib/format"
import type { LogEntry, LogLevel } from "@/lib/types"
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

function LogView() {
  const params = useSearchParams()
  const [level, setLevel] = useState<LogLevel>("info")
  const [jobId, setJobId] = useState(params.get("job") ?? "all")
  const [entries, setEntries] = useState<LogEntry[]>([])
  const [paused, setPaused] = useState(false)
  const [autoScroll, setAutoScroll] = useState(true)
  const [conn, setConn] = useState<Conn>("connecting")
  const [buffered, setBuffered] = useState(0)

  const pausedRef = useRef(false)
  const bufferRef = useRef<LogEntry[]>([])
  const boxRef = useRef<HTMLDivElement>(null)

  const jobs = usePoll(api.jobs, 15_000)
  const jobItems = [
    { value: "all", label: "全部任务" },
    ...(jobs.data ?? []).map((j) => ({ value: String(j.id), label: `#${j.id} ${j.name}` })),
  ]
  if (jobId !== "all" && !jobItems.some((i) => i.value === jobId)) {
    jobItems.push({ value: jobId, label: `#${jobId}` })
  }

  // History first, then the live stream. Re-runs whenever a filter changes.
  useEffect(() => {
    let cancelled = false
    let source: EventSource | undefined
    const filter = { level, job_id: jobId === "all" ? undefined : Number(jobId) }

    async function init() {
      let history: LogEntry[] = []
      try {
        history = await api.logs({ ...filter, limit: HISTORY })
      } catch {
        // the stream below still works; the retry badge shows connectivity problems
      }
      if (cancelled) return
      bufferRef.current = []
      setBuffered(0)
      setEntries(merge([], history))

      source = new EventSource(api.streamUrl(filter))
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
  }, [level, jobId])

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

  const text = useMemo(
    () =>
      entries
        .map((e) => `${e.time} ${e.level.toUpperCase().padEnd(5)} [job ${e.job_id}] ${e.message}`)
        .join("\n"),
    [entries],
  )

  function download() {
    const url = URL.createObjectURL(new Blob([text + "\n"], { type: "text/plain;charset=utf-8" }))
    const a = document.createElement("a")
    a.href = url
    a.download = `domain-scanner-${new Date().toISOString().slice(0, 19).replace(/[:T]/g, "-")}.log`
    a.click()
    URL.revokeObjectURL(url)
  }

  return (
    <>
      <div className="flex items-start justify-between gap-4">
        <div>
          <h1 className="text-xl font-semibold">运行日志</h1>
          <p className="text-sm text-muted-foreground">实时显示后端日志,用于排查扫描、限流和通知问题。</p>
        </div>
        <Badge variant={conn === "live" ? "secondary" : conn === "retrying" ? "destructive" : "outline"} data-testid="log-conn">
          {conn === "live" ? "已连接" : conn === "retrying" ? "重连中…" : "连接中…"}
        </Badge>
      </div>

      <div className="flex flex-wrap items-center gap-3">
        <Select value={level} onValueChange={(v) => setLevel(v as LogLevel)} items={levelItems}>
          <SelectTrigger aria-label="日志级别" className="w-36">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {levelItems.map((i) => (
              <SelectItem key={i.value} value={i.value}>{i.label}</SelectItem>
            ))}
          </SelectContent>
        </Select>
        <Select value={jobId} onValueChange={(v) => setJobId(String(v))} items={jobItems}>
          <SelectTrigger aria-label="任务筛选" className="w-52">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {jobItems.map((i) => (
              <SelectItem key={i.value} value={i.value}>{i.label}</SelectItem>
            ))}
          </SelectContent>
        </Select>
        <div className="flex items-center gap-2">
          <Switch id="log-pause" checked={paused} onCheckedChange={togglePause} />
          <Label htmlFor="log-pause">暂停{paused && buffered > 0 ? `(${buffered} 条待显示)` : ""}</Label>
        </div>
        <div className="flex items-center gap-2">
          <Switch id="log-follow" checked={autoScroll} onCheckedChange={setAutoScroll} />
          <Label htmlFor="log-follow">自动滚动</Label>
        </div>
        <div className="ml-auto flex gap-2">
          <Button variant="outline" onClick={() => { setAutoScroll(true) }}>
            <ArrowDownToLineIcon data-icon="inline-start" />
            跳到最新
          </Button>
          <Button variant="outline" onClick={() => setEntries([])}>
            <Trash2Icon data-icon="inline-start" />
            清屏
          </Button>
          <Button variant="outline" onClick={download} disabled={entries.length === 0}>
            <DownloadIcon data-icon="inline-start" />
            下载
          </Button>
        </div>
      </div>

      <Card className="min-h-0 flex-1">
        <CardContent className="min-h-0 flex-1">
          <div
            ref={boxRef}
            data-testid="log-box"
            className="h-[calc(100svh-17rem)] min-h-64 overflow-auto rounded-md bg-muted/40 p-3 font-mono text-xs leading-relaxed"
          >
            {conn === "connecting" && entries.length === 0 ? (
              <Skeleton className="h-full w-full" />
            ) : entries.length === 0 ? (
              <p className="text-muted-foreground">暂无日志。</p>
            ) : (
              entries.map((e) => (
                <div key={e.id} className={cn("flex gap-2 whitespace-pre-wrap break-all", levelClass[e.level])}>
                  <span className="shrink-0 text-muted-foreground">{fmtClock(e.time)}</span>
                  <span className="w-10 shrink-0 font-semibold uppercase">{e.level}</span>
                  {e.job_id > 0 && <span className="shrink-0 text-muted-foreground">#{e.job_id}</span>}
                  <span>{e.message}</span>
                </div>
              ))
            )}
          </div>
        </CardContent>
      </Card>
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
