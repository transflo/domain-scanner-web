"use client"

import { api } from "@/lib/api"
import { usePoll } from "@/hooks/use-poll"

/** Backend reachability, polled every 5 seconds. `connected` is undefined until the first answer. */
export function useHealth(): { connected: boolean | undefined } {
  const { data, error, loading } = usePoll(api.health, 5000)
  if (loading && !data && !error) return { connected: undefined }
  return { connected: Boolean(data?.ok) && !error }
}
