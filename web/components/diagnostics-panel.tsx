"use client"

import { useState } from "react"

import { SelectField } from "@/components/field"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Badge } from "@/components/ui/badge"
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"
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

const rangeItems = [
  { value: "1", label: "最近 1 小时" },
  { value: "6", label: "最近 6 小时" },
  { value: "24", label: "最近 24 小时" },
  { value: "168", label: "最近 7 天" },
]

const pct = (x: number) => `${(x * 100).toFixed(1)}%`

/** Aggregates over the structured logs: where checks fail and which egress carries the load. */
export function DiagnosticsPanel() {
  const [hours, setHours] = useState("24")
  const d = usePoll(() => api.diagnostics(Number(hours)), 15_000, hours)
  const data = d.data
  const egress = data?.diagnostics.by_egress ?? []
  const tld = data?.diagnostics.by_tld ?? []
  const names = new Map((data?.egresses ?? []).map((e) => [e.id, e.name]))

  return (
    <>
      <div className="flex flex-wrap items-end justify-between gap-3">
        <div className="w-full sm:w-48">
          <SelectField label="统计范围" value={hours} onChange={setHours} options={rangeItems} />
        </div>
        {data && (
          <p className="text-xs text-muted-foreground" data-testid="diag-summary">
            共 {fmtNum(data.diagnostics.checks)} 次检查 · 日志 {fmtNum(data.logs.count)} 条
            {data.logs.oldest && ` · 最早 ${fmtTime(data.logs.oldest)}`}
          </p>
        )}
      </div>

      {d.error && (
        <Alert variant="destructive">
          <AlertTitle>诊断数据获取失败</AlertTitle>
          <AlertDescription>{d.error.message}</AlertDescription>
        </Alert>
      )}

      <Card>
        <CardHeader>
          <CardTitle>按出站统计</CardTitle>
          <CardDescription>成功率 = 得到明确结论的检查 / 全部检查;限流、超时、网络错误高说明该出站需要退避或换用。</CardDescription>
        </CardHeader>
        <CardContent>
          {d.loading ? (
            <Skeleton className="h-24 w-full" />
          ) : egress.length === 0 ? (
            <p className="py-6 text-center text-sm text-muted-foreground">所选时间范围内没有检查记录。</p>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>出站</TableHead>
                  <TableHead className="text-right">检查</TableHead>
                  <TableHead className="text-right">成功率</TableHead>
                  <TableHead className="text-right">可注册</TableHead>
                  <TableHead className="text-right">限流</TableHead>
                  <TableHead className="text-right">超时</TableHead>
                  <TableHead className="text-right">网络错误</TableHead>
                  <TableHead className="text-right">平均 / 最大耗时</TableHead>
                  <TableHead className="text-right">退避</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {egress.map((e) => (
                  <TableRow key={e.egress} data-testid={`diag-egress-${e.egress}`}>
                    <TableCell className="whitespace-nowrap">
                      <span className="font-medium">{names.get(e.egress) ?? e.egress}</span>
                      <span className="ml-1 font-mono text-xs text-muted-foreground">{e.egress}</span>
                    </TableCell>
                    <TableCell className="text-right tabular-nums">{fmtNum(e.checks)}</TableCell>
                    <TableCell className="text-right tabular-nums">
                      <Badge variant={e.success_rate >= 0.8 ? "secondary" : "destructive"}>{pct(e.success_rate)}</Badge>
                    </TableCell>
                    <TableCell className="text-right tabular-nums">{fmtNum(e.available)}</TableCell>
                    <TableCell className="text-right tabular-nums">{fmtNum(e.rate_limited)}</TableCell>
                    <TableCell className="text-right tabular-nums">{fmtNum(e.timeouts)}</TableCell>
                    <TableCell className="text-right tabular-nums">{fmtNum(e.network_errors)}</TableCell>
                    <TableCell className="text-right tabular-nums whitespace-nowrap">
                      {Math.round(e.avg_ms)} / {fmtNum(e.max_ms)} ms
                    </TableCell>
                    <TableCell className="text-right tabular-nums">{fmtNum(e.storms)}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>按后缀统计</CardTitle>
          <CardDescription>哪些后缀的检查经常得不到结论(未知),可用来决定要不要换检测方式。</CardDescription>
        </CardHeader>
        <CardContent>
          {tld.length === 0 ? (
            <p className="py-6 text-center text-sm text-muted-foreground">没有数据。</p>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>后缀</TableHead>
                  <TableHead className="text-right">检查</TableHead>
                  <TableHead className="text-right">可注册</TableHead>
                  <TableHead className="text-right">已注册</TableHead>
                  <TableHead className="text-right">未知</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {tld.map((t) => (
                  <TableRow key={t.tld}>
                    <TableCell className="font-mono">.{t.tld}</TableCell>
                    <TableCell className="text-right tabular-nums">{fmtNum(t.checks)}</TableCell>
                    <TableCell className="text-right tabular-nums">{fmtNum(t.available)}</TableCell>
                    <TableCell className="text-right tabular-nums">{fmtNum(t.registered)}</TableCell>
                    <TableCell className="text-right tabular-nums">{fmtNum(t.unknown)}</TableCell>
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
