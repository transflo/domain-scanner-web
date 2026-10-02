"use client"

import { useState } from "react"
import { BrushCleaningIcon, HardDriveIcon } from "lucide-react"
import { toast } from "sonner"

import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import {
  Card,
  CardAction,
  CardContent,
  CardDescription,
  CardFooter,
  CardHeader,
  CardTitle,
} from "@/components/ui/card"
import { Progress } from "@/components/ui/progress"
import { Skeleton } from "@/components/ui/skeleton"
import { usePoll } from "@/hooks/use-poll"
import { api, ApiError } from "@/lib/api"
import { fmtBytes, fmtNum, fmtTime } from "@/lib/format"
import type { StorageReport } from "@/lib/types"

const stateLabel = { ok: "正常", low: "空间偏低", critical: "空间告急", "": "检测中" } as const
const stateVariant = { ok: "secondary", low: "outline", critical: "destructive", "": "outline" } as const

const days = (n: number) => (n > 0 ? `${n} 天` : "不限")

function Row({ k, v, testId }: { k: string; v: React.ReactNode; testId?: string }) {
  return (
    <div className="flex items-baseline justify-between gap-3 text-sm" data-testid={testId}>
      <span className="text-muted-foreground">{k}</span>
      <span className="text-right tabular-nums">{v}</span>
    </div>
  )
}

/** Disk and database usage, the retention rules in force, and a manual cleanup. */
export function StoragePanel() {
  const report = usePoll(api.storage, 30_000)
  const [busy, setBusy] = useState(false)
  const [fresh, setFresh] = useState<StorageReport>()
  const r = fresh ?? report.data

  async function cleanup() {
    setBusy(true)
    try {
      const out = await api.cleanupStorage()
      setFresh(out)
      toast.success(`清理完成:删除日志 ${fmtNum(out.deleted_logs)} 条、过期未知结果 ${fmtNum(out.deleted_unknown)} 条`)
    } catch (e) {
      toast.error(e instanceof ApiError ? e.message : "清理失败")
    } finally {
      setBusy(false)
    }
  }

  const used = r && r.disk_total_bytes > 0 ? Math.round(((r.disk_total_bytes - r.disk_free_bytes) / r.disk_total_bytes) * 100) : 0
  const logTotal = Object.values(r?.logs ?? {}).reduce((a, b) => a + b, 0)

  return (
    <Card className="lg:col-span-2">
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <HardDriveIcon className="size-4" />
          存储与保留
        </CardTitle>
        <CardDescription>
          日志按级别和天数自动清理,数据库超过上限时先删最旧的细日志,磁盘快满时主动瘦身;可注册的扫描结果永远保留。
        </CardDescription>
        <CardAction>
          <Badge variant={stateVariant[r?.state ?? ""]} data-testid="storage-state">
            {stateLabel[r?.state ?? ""]}
          </Badge>
        </CardAction>
      </CardHeader>
      <CardContent className="mt-4 flex flex-col gap-4">
        {report.error && (
          <Alert variant="destructive">
            <AlertTitle>存储信息获取失败</AlertTitle>
            <AlertDescription>{report.error.message}</AlertDescription>
          </Alert>
        )}
        {report.loading && !r ? (
          <Skeleton className="h-32 w-full" />
        ) : r ? (
          <>
            {r.state !== "ok" && r.state !== "" && (
              <Alert variant="destructive" data-testid="storage-warning">
                <AlertTitle>磁盘剩余空间偏低</AlertTitle>
                <AlertDescription>
                  已自动清理日志{r.level_forced ? ",并暂时只记录警告及以上级别,空间恢复后会自动还原" : ""}。请尽快扩容或清理宿主机磁盘。
                </AlertDescription>
              </Alert>
            )}
            {r.disk_total_bytes > 0 && (
              <div className="flex flex-col gap-2">
                <div className="flex items-baseline justify-between text-sm">
                  <span className="text-muted-foreground">数据盘</span>
                  <span className="tabular-nums" data-testid="storage-disk">
                    剩余 {fmtBytes(r.disk_free_bytes)} / 共 {fmtBytes(r.disk_total_bytes)}
                  </span>
                </div>
                <Progress value={used} aria-label="数据盘已用比例" />
              </div>
            )}
            <div className="grid gap-x-8 gap-y-2 md:grid-cols-2">
              <div className="flex flex-col gap-2">
                <Row k="数据库文件" v={fmtBytes(r.db_bytes)} testId="storage-db" />
                <Row k="其中有效数据" v={fmtBytes(r.db_used_bytes)} />
                <Row k="预写日志(WAL)" v={fmtBytes(r.wal_bytes)} />
                <Row k="日志条数" v={fmtNum(logTotal)} />
                <Row
                  k="按级别"
                  v={(["debug", "info", "warn", "error"] as const).map((l) => `${l} ${fmtNum(r.logs?.[l] ?? 0)}`).join(" · ")}
                />
                <Row k="扫描结果" v={Object.entries(r.results ?? {}).map(([k, v]) => `${k} ${fmtNum(v)}`).join(" · ") || "0"} />
              </div>
              <div className="flex flex-col gap-2">
                <Row k="debug 日志保留" v={`${days(r.policy.DebugDays)} / ${fmtNum(r.policy.MaxDebugRows)} 条`} />
                <Row k="info 日志保留" v={days(r.policy.InfoDays)} />
                <Row k="warn / error 保留" v={days(r.policy.WarnDays)} />
                <Row k="数据库大小上限" v={r.policy.MaxDBBytes > 0 ? fmtBytes(r.policy.MaxDBBytes) : "不限"} />
                <Row k="未知结果保留" v={days(r.policy.UnknownResultDays)} />
                <Row k="磁盘告警线" v={r.policy.MinFreeBytes > 0 ? `剩余 < ${fmtBytes(r.policy.MinFreeBytes)}` : "关闭"} />
              </div>
            </div>
            {r.error && (
              <Alert variant="destructive">
                <AlertTitle>上次清理有错误</AlertTitle>
                <AlertDescription className="break-words">{r.error}</AlertDescription>
              </Alert>
            )}
          </>
        ) : null}
      </CardContent>
      <CardFooter className="mt-4 flex-wrap gap-3">
        <Button variant="outline" onClick={cleanup} disabled={busy}>
          <BrushCleaningIcon data-icon="inline-start" />
          {busy ? "清理中…" : "立即清理"}
        </Button>
        {r?.at && <span className="text-xs text-muted-foreground">上次运行:{fmtTime(r.at)}(每 15 分钟自动执行)</span>}
        <span className="text-xs text-muted-foreground">保留规则通过环境变量调整,见 README「长期运行与磁盘」。</span>
      </CardFooter>
    </Card>
  )
}
