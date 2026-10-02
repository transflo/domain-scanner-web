import { Progress } from "@/components/ui/progress"
import { fmtNum, progressPercent } from "@/lib/format"
import type { Job } from "@/lib/types"

export function JobProgress({ job }: { job: Job }) {
  const pct = progressPercent(job.cursor, job.total)
  return (
    <div className="flex min-w-36 flex-col gap-1">
      <Progress value={pct} aria-label={`任务 ${job.id} 进度`} />
      <span className="text-xs tabular-nums text-muted-foreground">
        {fmtNum(job.cursor)} / {fmtNum(job.total)}({pct}%)
      </span>
    </div>
  )
}
