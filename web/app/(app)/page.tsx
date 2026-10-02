"use client"

import Link from "next/link"
import { ArrowRightIcon } from "lucide-react"

import { JobProgress } from "@/components/job-progress"
import { StatusBadge } from "@/components/status-badge"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { Card, CardAction, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"
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
import { fmtNum } from "@/lib/format"

function StatCard({
  title,
  value,
  hint,
  testId,
}: {
  title: string
  value: number | undefined
  hint: string
  testId: string
}) {
  return (
    <Card size="sm">
      <CardHeader>
        <CardDescription>{title}</CardDescription>
        <CardTitle className="text-2xl tabular-nums" data-testid={testId}>
          {value === undefined ? <Skeleton className="h-8 w-20" /> : fmtNum(value)}
        </CardTitle>
      </CardHeader>
      <CardContent className="text-xs text-muted-foreground">{hint}</CardContent>
    </Card>
  )
}

export default function DashboardPage() {
  const stats = usePoll(api.stats, 3000)
  const jobs = usePoll(api.jobs, 3000)
  const s = stats.data
  const recent = (jobs.data ?? []).slice(0, 6)
  const failed = stats.error ?? jobs.error

  return (
    <>
      <div>
        <h1 className="text-xl font-semibold">仪表盘</h1>
        <p className="text-sm text-muted-foreground">所有任务的汇总,每 3 秒自动刷新。</p>
      </div>

      {failed && (
        <Alert variant="destructive">
          <AlertTitle>数据获取失败</AlertTitle>
          <AlertDescription>{failed.message}</AlertDescription>
        </Alert>
      )}

      <div className="grid gap-4 sm:grid-cols-2 xl:grid-cols-4">
        <StatCard title="运行中任务" value={s?.running_jobs} hint={`共 ${s ? fmtNum(s.jobs) : "–"} 个任务`} testId="stat-running" />
        <StatCard title="已检查域名" value={s?.checked} hint="所有任务累计" testId="stat-checked" />
        <StatCard title="可注册域名" value={s?.available} hint="已通过 RDAP/WHOIS 确认" testId="stat-available" />
        <StatCard title="无法确定" value={s?.unknown} hint="被限流或超时,可稍后重扫" testId="stat-unknown" />
      </div>

      <Card>
        <CardHeader>
          <CardTitle>最近任务</CardTitle>
          <CardDescription>最新创建的 6 个任务</CardDescription>
          <CardAction>
            <Button variant="outline" size="sm" render={<Link href="/jobs" />}>
              全部任务 <ArrowRightIcon data-icon="inline-end" />
            </Button>
          </CardAction>
        </CardHeader>
        <CardContent>
          {jobs.loading ? (
            <Skeleton className="h-24 w-full" />
          ) : recent.length === 0 ? (
            <p className="py-6 text-center text-sm text-muted-foreground">
              还没有任务。前往「扫描任务」创建第一个。
            </p>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>名称</TableHead>
                  <TableHead>状态</TableHead>
                  <TableHead>进度</TableHead>
                  <TableHead className="text-right">可注册</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {recent.map((j) => (
                  <TableRow key={j.id}>
                    <TableCell className="font-medium">
                      #{j.id} {j.name}
                    </TableCell>
                    <TableCell>
                      <StatusBadge status={j.status} />
                    </TableCell>
                    <TableCell>
                      <JobProgress job={j} />
                    </TableCell>
                    <TableCell className="text-right tabular-nums">{fmtNum(j.available)}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>
    </>
  )
}
