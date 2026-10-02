"use client"

import { useState } from "react"
import Link from "next/link"
import {
  FileTextIcon,
  PauseIcon,
  PlayIcon,
  PlusIcon,
  SquareIcon,
  TableIcon,
  Trash2Icon,
} from "lucide-react"
import { toast } from "sonner"

import { JobForm } from "@/components/job-form"
import { JobProgress } from "@/components/job-progress"
import { StatusBadge } from "@/components/status-badge"
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
import { Button } from "@/components/ui/button"
import { Card, CardContent } from "@/components/ui/card"
import { Skeleton } from "@/components/ui/skeleton"
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table"
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip"
import { usePoll } from "@/hooks/use-poll"
import { api, ApiError } from "@/lib/api"
import { fmtNum, fmtTime } from "@/lib/format"
import type { Job } from "@/lib/types"

type Confirm = { kind: "cancel" | "delete"; job: Job } | null

function IconAction({
  label,
  onClick,
  href,
  children,
  destructive,
}: {
  label: string
  onClick?: () => void
  href?: string
  children: React.ReactNode
  destructive?: boolean
}) {
  return (
    <Tooltip>
      <TooltipTrigger
        render={
          href ? (
            <Button variant="ghost" size="icon-sm" aria-label={label} render={<Link href={href} />} nativeButton={false} />
          ) : (
            <Button
              variant={destructive ? "destructive" : "ghost"}
              size="icon-sm"
              aria-label={label}
              onClick={onClick}
            />
          )
        }
      >
        {children}
      </TooltipTrigger>
      <TooltipContent>{label}</TooltipContent>
    </Tooltip>
  )
}

export default function JobsPage() {
  const jobs = usePoll(api.jobs, 2000)
  const [formOpen, setFormOpen] = useState(false)
  const [confirm, setConfirm] = useState<Confirm>(null)

  async function act(job: Job, action: "pause" | "resume" | "cancel") {
    try {
      await api.jobAction(job.id, action)
      jobs.refresh()
    } catch (err) {
      toast.error(err instanceof ApiError ? err.message : "操作失败")
    }
  }

  async function runConfirmed() {
    if (!confirm) return
    const { kind, job } = confirm
    setConfirm(null)
    try {
      if (kind === "delete") {
        await api.deleteJob(job.id)
        toast.success(`任务 #${job.id} 已删除`)
      } else {
        await api.jobAction(job.id, "cancel")
        toast.success(`任务 #${job.id} 已取消`)
      }
      jobs.refresh()
    } catch (err) {
      toast.error(err instanceof ApiError ? err.message : "操作失败")
    }
  }

  const items = jobs.data ?? []

  return (
    <>
      <div className="flex items-start justify-between gap-4">
        <div>
          <h1 className="text-xl font-semibold">扫描任务</h1>
          <p className="text-sm text-muted-foreground">任务在服务端后台运行,服务重启后会自动续跑。</p>
        </div>
        <Button onClick={() => setFormOpen(true)}>
          <PlusIcon data-icon="inline-start" />
          新建任务
        </Button>
      </div>

      {jobs.error && (
        <Alert variant="destructive">
          <AlertTitle>任务列表获取失败</AlertTitle>
          <AlertDescription>{jobs.error.message}</AlertDescription>
        </Alert>
      )}

      <Card>
        <CardContent>
          {jobs.loading ? (
            <Skeleton className="h-32 w-full" />
          ) : items.length === 0 ? (
            <p className="py-10 text-center text-sm text-muted-foreground">
              还没有任务,点击右上角「新建任务」开始。
            </p>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>任务</TableHead>
                  <TableHead>状态</TableHead>
                  <TableHead>进度</TableHead>
                  <TableHead className="text-right">可注册</TableHead>
                  <TableHead className="text-right">未知</TableHead>
                  <TableHead className="text-right">已注册/保留</TableHead>
                  <TableHead>创建时间</TableHead>
                  <TableHead className="text-right">操作</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {items.map((j) => (
                  <TableRow key={j.id} data-testid={`job-row-${j.id}`}>
                    <TableCell>
                      <div className="font-medium">
                        #{j.id} {j.name}
                      </div>
                      <div className="text-xs text-muted-foreground">
                        {j.wordlist
                          ? `词库 ${j.wordlist}`
                          : `${j.pattern === "d" ? "数字" : j.pattern === "D" ? "字母" : "字母数字"} × ${j.length}`}
                        {j.regex && ` · /${j.regex}/`} · {j.workers} 并发 · {j.delay_ms}ms
                      </div>
                      {j.error && <div className="mt-1 text-xs text-destructive">{j.error}</div>}
                    </TableCell>
                    <TableCell>
                      <StatusBadge status={j.status} />
                    </TableCell>
                    <TableCell>
                      <JobProgress job={j} />
                    </TableCell>
                    <TableCell className="text-right tabular-nums">{fmtNum(j.available)}</TableCell>
                    <TableCell className="text-right tabular-nums">{fmtNum(j.unknown)}</TableCell>
                    <TableCell className="text-right tabular-nums">{fmtNum(j.registered)}</TableCell>
                    <TableCell className="whitespace-nowrap text-xs text-muted-foreground">
                      {fmtTime(j.created_at)}
                    </TableCell>
                    <TableCell>
                      <div className="flex justify-end gap-1">
                        {(j.status === "running" || j.status === "queued") && (
                          <IconAction label="暂停" onClick={() => act(j, "pause")}>
                            <PauseIcon />
                          </IconAction>
                        )}
                        {(j.status === "paused" || j.status === "failed") && (
                          <IconAction label="继续" onClick={() => act(j, "resume")}>
                            <PlayIcon />
                          </IconAction>
                        )}
                        {(j.status === "running" || j.status === "queued" || j.status === "paused") && (
                          <IconAction label="取消" onClick={() => setConfirm({ kind: "cancel", job: j })}>
                            <SquareIcon />
                          </IconAction>
                        )}
                        <IconAction label="查看结果" href={`/results?job=${j.id}`}>
                          <TableIcon />
                        </IconAction>
                        <IconAction label="查看日志" href={`/logs?job=${j.id}`}>
                          <FileTextIcon />
                        </IconAction>
                        <IconAction label="删除" destructive onClick={() => setConfirm({ kind: "delete", job: j })}>
                          <Trash2Icon />
                        </IconAction>
                      </div>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>

      <JobForm open={formOpen} onOpenChange={setFormOpen} onCreated={() => jobs.refresh()} />

      <AlertDialog open={confirm !== null} onOpenChange={(o) => !o && setConfirm(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{confirm?.kind === "delete" ? "删除任务?" : "取消任务?"}</AlertDialogTitle>
            <AlertDialogDescription>
              {confirm?.kind === "delete"
                ? `任务 #${confirm.job.id}「${confirm.job.name}」及其所有扫描结果和日志都会被永久删除。`
                : `任务 #${confirm?.job.id} 将停止并且无法继续,已有结果会保留。`}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>返回</AlertDialogCancel>
            <AlertDialogAction variant="destructive" onClick={runConfirmed}>
              {confirm?.kind === "delete" ? "删除" : "取消任务"}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  )
}
