import type { JobStatus, LogLevel } from "@/lib/types"

const numberFormat = new Intl.NumberFormat("zh-CN")

export const fmtNum = (n: number) => numberFormat.format(n)

export function fmtTime(iso: string): string {
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  return d.toLocaleString("zh-CN", { hour12: false })
}

export function fmtClock(iso: string): string {
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  return d.toLocaleTimeString("zh-CN", { hour12: false })
}

export const statusLabel: Record<JobStatus, string> = {
  queued: "排队中",
  running: "运行中",
  paused: "已暂停",
  done: "已完成",
  failed: "失败",
  cancelled: "已取消",
}

export const levelLabel: Record<LogLevel, string> = {
  debug: "调试",
  info: "信息",
  warn: "警告",
  error: "错误",
}

export function progressPercent(cursor: number, total: number): number {
  if (total <= 0) return 0
  return Math.min(100, Math.floor((cursor / total) * 100))
}

/** Candidate count for a pattern job, or null when it would overflow safe integers. */
export function estimateSpace(pattern: string, length: number): number | null {
  const size = pattern === "d" ? 10 : pattern === "D" ? 26 : pattern === "a" ? 36 : 0
  if (!size || length < 1) return null
  const total = Math.pow(size, length)
  return Number.isSafeInteger(total) ? total : null
}
