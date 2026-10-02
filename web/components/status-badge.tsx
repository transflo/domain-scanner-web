import { Badge } from "@/components/ui/badge"
import { statusLabel } from "@/lib/format"
import type { JobStatus } from "@/lib/types"

const variant: Record<JobStatus, React.ComponentProps<typeof Badge>["variant"]> = {
  running: "default",
  queued: "outline",
  paused: "outline",
  done: "secondary",
  failed: "destructive",
  cancelled: "ghost",
}

export function StatusBadge({ status }: { status: JobStatus }) {
  return <Badge variant={variant[status]}>{statusLabel[status]}</Badge>
}
