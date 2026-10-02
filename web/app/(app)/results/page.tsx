"use client"

import { Suspense, useEffect, useState } from "react"
import { useSearchParams } from "next/navigation"
import { ClipboardCopyIcon, CopyIcon, DownloadIcon, SearchIcon } from "lucide-react"
import { toast } from "sonner"

import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card, CardContent } from "@/components/ui/card"
import { Input } from "@/components/ui/input"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { Skeleton } from "@/components/ui/skeleton"
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table"
import { usePoll } from "@/hooks/use-poll"
import { api } from "@/lib/api"
import { fmtNum, fmtTime } from "@/lib/format"
import type { CFStatus, ResultRow } from "@/lib/types"

const PAGE_SIZE = 50

const statusItems = [
  { value: "available", label: "可注册" },
  { value: "unknown", label: "无法确定" },
  { value: "all", label: "全部" },
]

const cfItems = [
  { value: "all", label: "Cloudflare:全部" },
  { value: "confirmed", label: "已确认可注册" },
  { value: "unsupported", label: "未经 CF 确认" },
  { value: "rejected", label: "CF 判定不可注册" },
  { value: "pending", label: "核查中" },
  { value: "error", label: "核查失败" },
]

const cfLabel: Record<CFStatus, string> = {
  "": "未核查",
  pending: "核查中",
  confirmed: "CF 已确认",
  rejected: "CF 不可注册",
  unsupported: "未经 CF 确认",
  error: "核查失败",
}

const cfVariant: Record<CFStatus, React.ComponentProps<typeof Badge>["variant"]> = {
  "": "outline",
  pending: "outline",
  confirmed: "default",
  rejected: "destructive",
  unsupported: "secondary",
  error: "destructive",
}

const registerLabel: Record<string, string> = {
  registering: "注册中…",
  succeeded: "已注册",
  failed: "注册失败",
}

function CFCell({ r }: { r: ResultRow }) {
  return (
    <div className="flex min-w-0 flex-col items-start gap-1" data-testid={`cf-${r.id}`}>
      <Badge variant={cfVariant[r.cf_status]}>{cfLabel[r.cf_status]}</Badge>
      {r.cf_status === "confirmed" && r.cf_price && (
        <span className="text-xs tabular-nums text-muted-foreground">
          {r.cf_price} {r.cf_currency}
        </span>
      )}
      {r.cf_reason && r.cf_status !== "confirmed" && (
        <span className="max-w-40 truncate text-xs text-muted-foreground" title={r.cf_reason}>
          {r.cf_reason}
        </span>
      )}
    </div>
  )
}

async function copy(text: string, what: string) {
  try {
    await navigator.clipboard.writeText(text)
    toast.success(`已复制${what}`)
  } catch {
    toast.error("复制失败,请手动选择文本")
  }
}

function ResultsView() {
  const params = useSearchParams()
  const [status, setStatus] = useState("available")
  const [cf, setCf] = useState("all")
  const [jobId, setJobId] = useState(params.get("job") ?? "all")
  const [q, setQ] = useState("")
  const [debouncedQ, setDebouncedQ] = useState("")
  const [page, setPage] = useState(0)

  useEffect(() => {
    const t = setTimeout(() => {
      setDebouncedQ(q.trim())
      setPage(0)
    }, 300)
    return () => clearTimeout(t)
  }, [q])

  const jobs = usePoll(api.jobs, 10_000)
  const jobItems = [
    { value: "all", label: "全部任务" },
    ...(jobs.data ?? []).map((j) => ({ value: String(j.id), label: `#${j.id} ${j.name}` })),
  ]
  // The job list loads asynchronously; until then label a preselected job (?job=N) as "#N".
  if (jobId !== "all" && !jobItems.some((i) => i.value === jobId)) {
    jobItems.push({ value: jobId, label: `#${jobId}` })
  }

  const filter = {
    status: status === "all" ? undefined : status,
    cf_status: cf === "all" ? undefined : cf,
    job_id: jobId === "all" ? undefined : Number(jobId),
    q: debouncedQ || undefined,
  }
  const key = JSON.stringify({ ...filter, page })
  const results = usePoll(
    () => api.results({ ...filter, limit: PAGE_SIZE, offset: page * PAGE_SIZE }),
    5000,
    key,
  )
  const rows = results.data?.items ?? []
  const total = results.data?.total ?? 0
  const pages = Math.max(1, Math.ceil(total / PAGE_SIZE))

  return (
    <>
      <div>
        <h1 className="text-xl font-semibold">扫描结果</h1>
        <p className="text-sm text-muted-foreground">
          默认显示 RDAP/WHOIS 判定可注册的域名;Cloudflare 列是最终核查结果,只有「CF 已确认」的域名才会推送带注册按钮的 Telegram 消息。
        </p>
      </div>

      <div className="flex flex-wrap items-center gap-2">
        <Select value={status} onValueChange={(v) => { setStatus(String(v)); setPage(0) }} items={statusItems}>
          <SelectTrigger aria-label="状态筛选" className="w-32">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {statusItems.map((i) => (
              <SelectItem key={i.value} value={i.value}>{i.label}</SelectItem>
            ))}
          </SelectContent>
        </Select>
        <Select value={cf} onValueChange={(v) => { setCf(String(v)); setPage(0) }} items={cfItems}>
          <SelectTrigger aria-label="Cloudflare 筛选" className="w-44">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {cfItems.map((i) => (
              <SelectItem key={i.value} value={i.value}>{i.label}</SelectItem>
            ))}
          </SelectContent>
        </Select>
        <Select value={jobId} onValueChange={(v) => { setJobId(String(v)); setPage(0) }} items={jobItems}>
          <SelectTrigger aria-label="任务筛选" className="w-full sm:w-52">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {jobItems.map((i) => (
              <SelectItem key={i.value} value={i.value}>{i.label}</SelectItem>
            ))}
          </SelectContent>
        </Select>
        <div className="relative w-full sm:w-auto">
          <SearchIcon className="pointer-events-none absolute top-1/2 left-2.5 size-4 -translate-y-1/2 text-muted-foreground" />
          <Input
            aria-label="搜索域名"
            placeholder="搜索域名…"
            className="w-full pl-8 sm:w-48"
            value={q}
            onChange={(e) => setQ(e.target.value)}
          />
        </div>
        <div className="ml-auto flex gap-2">
          <Button
            variant="outline"
            disabled={rows.length === 0}
            onClick={() => copy(rows.map((r) => r.domain).join("\n"), `本页 ${rows.length} 个域名`)}
          >
            <ClipboardCopyIcon data-icon="inline-start" />
            复制本页
          </Button>
          <Button variant="outline" nativeButton={false} render={<a href={api.exportUrl(filter)} download />}>
            <DownloadIcon data-icon="inline-start" />
            导出 CSV
          </Button>
        </div>
      </div>

      {results.error && (
        <Alert variant="destructive">
          <AlertTitle>结果获取失败</AlertTitle>
          <AlertDescription>{results.error.message}</AlertDescription>
        </Alert>
      )}

      <Card>
        <CardContent>
          {results.loading ? (
            <Skeleton className="h-40 w-full" />
          ) : rows.length === 0 ? (
            <p className="py-10 text-center text-sm text-muted-foreground" data-testid="results-empty">
              没有符合条件的结果。
            </p>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>域名</TableHead>
                  <TableHead>状态</TableHead>
                  <TableHead>Cloudflare</TableHead>
                  <TableHead>注册</TableHead>
                  <TableHead>任务</TableHead>
                  <TableHead>说明</TableHead>
                  <TableHead>发现时间</TableHead>
                  <TableHead className="w-10" />
                </TableRow>
              </TableHeader>
              <TableBody>
                {rows.map((r) => (
                  <TableRow key={r.id}>
                    <TableCell className="font-mono">{r.domain}</TableCell>
                    <TableCell>
                      <Badge variant={r.status === "available" ? "default" : "outline"}>
                        {r.status === "available" ? "可注册" : "无法确定"}
                      </Badge>
                    </TableCell>
                    <TableCell>
                      <CFCell r={r} />
                    </TableCell>
                    <TableCell className="whitespace-nowrap text-xs" title={r.register_note}>
                      {r.register_status ? (
                        <Badge variant={r.register_status === "succeeded" ? "default" : r.register_status === "failed" ? "destructive" : "outline"}>
                          {registerLabel[r.register_status] ?? r.register_status}
                        </Badge>
                      ) : (
                        <span className="text-muted-foreground">—</span>
                      )}
                    </TableCell>
                    <TableCell className="tabular-nums">#{r.job_id}</TableCell>
                    <TableCell className="max-w-72 truncate text-xs text-muted-foreground" title={r.signatures}>
                      {r.signatures || "—"}
                    </TableCell>
                    <TableCell className="whitespace-nowrap text-xs text-muted-foreground">
                      {fmtTime(r.created_at)}
                    </TableCell>
                    <TableCell>
                      <Button variant="ghost" size="icon-sm" aria-label={`复制 ${r.domain}`} onClick={() => copy(r.domain, r.domain)}>
                        <CopyIcon />
                      </Button>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>

      <div className="flex items-center justify-between text-sm text-muted-foreground">
        <span data-testid="results-total">共 {fmtNum(total)} 条</span>
        <div className="flex items-center gap-2">
          <Button variant="outline" size="sm" disabled={page === 0} onClick={() => setPage((p) => p - 1)}>
            上一页
          </Button>
          <span className="tabular-nums">
            {page + 1} / {pages}
          </span>
          <Button variant="outline" size="sm" disabled={page + 1 >= pages} onClick={() => setPage((p) => p + 1)}>
            下一页
          </Button>
        </div>
      </div>
    </>
  )
}

export default function ResultsPage() {
  return (
    <Suspense fallback={<Skeleton className="h-40 w-full" />}>
      <ResultsView />
    </Suspense>
  )
}
