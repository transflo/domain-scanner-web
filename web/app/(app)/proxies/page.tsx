"use client"

import { useState } from "react"
import {
  ActivityIcon,
  PencilIcon,
  PlusIcon,
  RefreshCwIcon,
  Trash2Icon,
  ZapIcon,
} from "lucide-react"
import { toast } from "sonner"

import { OutboundDialog } from "@/components/outbound-dialog"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card, CardContent } from "@/components/ui/card"
import { Skeleton } from "@/components/ui/skeleton"
import { Switch } from "@/components/ui/switch"
import { usePoll } from "@/hooks/use-poll"
import { api, ApiError } from "@/lib/api"
import { fmtTime } from "@/lib/format"
import type { EgressStatus, Outbound, OutboundDetail } from "@/lib/types"

function health(o: Outbound, eg?: EgressStatus) {
  if (!o.enabled) return { label: "已停用", variant: "outline" as const }
  if (eg?.penalized_until && new Date(eg.penalized_until).getTime() > Date.now())
    return { label: "冷却中", variant: "destructive" as const }
  if (!o.last_test_at) return { label: "未测活", variant: "outline" as const }
  return o.last_ok ? { label: "可用", variant: "secondary" as const } : { label: "不可用", variant: "destructive" as const }
}

export default function ProxiesPage() {
  const list = usePoll(api.outbounds, 5000)
  const [dialog, setDialog] = useState<{ open: boolean; editing?: OutboundDetail }>({ open: false })
  const [testing, setTesting] = useState<Set<number>>(new Set())
  const [testingAll, setTestingAll] = useState(false)
  const [reloading, setReloading] = useState(false)
  const [del, setDel] = useState<Outbound | null>(null)

  const items = list.data?.items ?? []
  const status = list.data?.status
  const egressById = new Map((status?.egresses ?? []).map((e) => [e.id, e]))

  const fail = (e: unknown, fallback: string) => toast.error(e instanceof ApiError ? e.message : fallback)

  async function openEdit(o: Outbound) {
    try {
      setDialog({ open: true, editing: await api.outbound(o.id) })
    } catch (e) {
      fail(e, "读取配置失败")
    }
  }

  async function test(o: Outbound) {
    setTesting((s) => new Set(s).add(o.id))
    try {
      const r = await api.testOutbound(o.id)
      if (r.ok) toast.success(`「${o.name}」可用:${r.delay_ms} ms,出口 ${r.ip} ${r.country}`)
      else toast.error(`「${o.name}」不可用:${r.error}`)
      list.refresh()
    } catch (e) {
      fail(e, "测活失败")
    } finally {
      setTesting((s) => {
        const n = new Set(s)
        n.delete(o.id)
        return n
      })
    }
  }

  async function testAll() {
    setTestingAll(true)
    try {
      const res = await api.testAllOutbounds()
      const all = Object.values(res)
      toast.success(`测活完成:${all.filter((r) => r.ok).length} / ${all.length} 个可用`)
      list.refresh()
    } catch (e) {
      fail(e, "测活失败")
    } finally {
      setTestingAll(false)
    }
  }

  async function reload() {
    setReloading(true)
    try {
      await api.reloadOutbounds()
      toast.success("xray 已重载")
    } catch (e) {
      fail(e, "重载失败")
    } finally {
      setReloading(false)
      list.refresh()
    }
  }

  async function toggle(o: Outbound, enabled: boolean) {
    try {
      const r = await api.updateOutbound(o.id, { enabled })
      if (r.reload_error) toast.warning(`已保存,但 xray 重载失败:${r.reload_error}`)
      list.refresh()
    } catch (e) {
      fail(e, "操作失败")
    }
  }

  async function remove() {
    if (!del) return
    const o = del
    setDel(null)
    try {
      await api.deleteOutbound(o.id)
      toast.success(`已删除「${o.name}」`)
      list.refresh()
    } catch (e) {
      fail(e, "删除失败")
    }
  }

  return (
    <>
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h1 className="text-xl font-semibold">出站代理</h1>
          <p className="text-sm text-muted-foreground">
            由 Xray 内核转发扫描流量;创建任务时可指定使用某个代理,直连被限流时会自动切换到可用代理。
          </p>
        </div>
        <div className="flex flex-wrap gap-2">
          <Button variant="outline" onClick={reload} disabled={reloading}>
            <RefreshCwIcon data-icon="inline-start" className={reloading ? "animate-spin" : ""} />
            重载
          </Button>
          <Button variant="outline" onClick={testAll} disabled={testingAll || items.length === 0}>
            <ZapIcon data-icon="inline-start" />
            {testingAll ? "测活中…" : "全部测活"}
          </Button>
          <Button onClick={() => setDialog({ open: true })}>
            <PlusIcon data-icon="inline-start" />
            添加
          </Button>
        </div>
      </div>

      {status && !status.xray_available && (
        <Alert variant="destructive" data-testid="xray-missing">
          <AlertTitle>未找到 Xray 内核</AlertTitle>
          <AlertDescription>
            服务端找不到 xray 可执行文件(环境变量 XRAY_BIN),出站代理暂不可用,任务只能直连。Docker 镜像已内置 Xray。
          </AlertDescription>
        </Alert>
      )}
      {status && status.xray_available && status.error && (
        <Alert variant="destructive" data-testid="xray-error">
          <AlertTitle>Xray 未能运行</AlertTitle>
          <AlertDescription className="break-words">{status.error}</AlertDescription>
        </Alert>
      )}
      {list.error && (
        <Alert variant="destructive">
          <AlertTitle>代理列表获取失败</AlertTitle>
          <AlertDescription>{list.error.message}</AlertDescription>
        </Alert>
      )}

      <Card>
        <CardContent className="flex flex-col divide-y">
          {list.loading ? (
            <Skeleton className="h-32 w-full" />
          ) : items.length === 0 ? (
            <p className="py-10 text-center text-sm text-muted-foreground" data-testid="proxies-empty">
              还没有出站代理。点击「添加」,可以用表单、JSON 或分享链接导入。
            </p>
          ) : (
            items.map((o) => {
              const h = health(o, egressById.get(`proxy-${o.id}`))
              return (
                <div
                  key={o.id}
                  data-testid={`proxy-row-${o.id}`}
                  className="flex flex-col gap-3 py-3 first:pt-0 last:pb-0 md:flex-row md:items-center"
                >
                  <div className="min-w-0 flex-1">
                    <div className="flex flex-wrap items-center gap-2">
                      <span className="min-w-0 break-all font-medium">{o.name}</span>
                      <Badge variant={h.variant}>{h.label}</Badge>
                    </div>
                    <div className="mt-1 flex flex-wrap items-center gap-x-2 gap-y-1 text-xs text-muted-foreground">
                      <Badge variant="outline">{o.protocol}</Badge>
                      <Badge variant="outline">{o.transport}</Badge>
                      {o.security !== "none" && <Badge variant="outline">{o.security}</Badge>}
                      <span className="break-all font-mono">
                        {o.address}:{o.port}
                      </span>
                    </div>
                    {o.last_test_at && (
                      <div className="mt-1 text-xs text-muted-foreground">
                        {o.last_ok ? (
                          <span data-testid={`proxy-result-${o.id}`}>
                            {o.last_delay_ms} ms · 出口 {o.last_ip} {o.last_country}
                          </span>
                        ) : (
                          <span className="break-words text-destructive" data-testid={`proxy-result-${o.id}`}>
                            {o.last_error || "测活失败"}
                          </span>
                        )}
                        <span> · {fmtTime(o.last_test_at)}</span>
                      </div>
                    )}
                  </div>
                  <div className="flex items-center gap-2 md:shrink-0">
                    <Switch
                      checked={o.enabled}
                      onCheckedChange={(c) => toggle(o, c)}
                      aria-label={`${o.enabled ? "停用" : "启用"} ${o.name}`}
                    />
                    <Button variant="outline" size="sm" onClick={() => test(o)} disabled={testing.has(o.id)}>
                      <ActivityIcon data-icon="inline-start" />
                      {testing.has(o.id) ? "测活中…" : "测活"}
                    </Button>
                    <Button variant="ghost" size="icon-sm" aria-label={`编辑 ${o.name}`} onClick={() => openEdit(o)}>
                      <PencilIcon />
                    </Button>
                    <Button variant="ghost" size="icon-sm" aria-label={`删除 ${o.name}`} onClick={() => setDel(o)}>
                      <Trash2Icon />
                    </Button>
                  </div>
                </div>
              )
            })
          )}
        </CardContent>
      </Card>

      <OutboundDialog
        open={dialog.open}
        onOpenChange={(o) => setDialog((d) => ({ ...d, open: o }))}
        editing={dialog.editing}
        onSaved={() => list.refresh()}
      />

      <AlertDialog open={del !== null} onOpenChange={(o) => !o && setDel(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>删除出站代理?</AlertDialogTitle>
            <AlertDialogDescription>
              「{del?.name}」将被永久删除;正在使用它的任务会回到直连或其它可用代理。
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>返回</AlertDialogCancel>
            <AlertDialogAction variant="destructive" onClick={remove}>
              删除
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  )
}
