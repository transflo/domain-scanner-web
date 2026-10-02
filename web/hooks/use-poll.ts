"use client"

import { useCallback, useEffect, useRef, useState } from "react"

interface PollState<T> {
  data: T | undefined
  error: Error | undefined
  loading: boolean
  refresh: () => void
}

/**
 * Calls `fn` immediately and then every `intervalMs` while the tab is visible.
 * `fn` is read through a ref, so callers do not need to memoise it; pass `key` to force a
 * refetch (and a loading state) when the inputs of `fn` change.
 */
export function usePoll<T>(fn: () => Promise<T>, intervalMs: number, key = ""): PollState<T> {
  const [data, setData] = useState<T>()
  const [error, setError] = useState<Error>()
  const [loading, setLoading] = useState(true)
  const fnRef = useRef(fn)
  const tick = useRef(0)

  useEffect(() => {
    fnRef.current = fn
  })

  const run = useCallback(async () => {
    const my = ++tick.current
    try {
      const result = await fnRef.current()
      if (my !== tick.current) return // a newer request superseded this one
      setData(result)
      setError(undefined)
    } catch (e) {
      if (my !== tick.current) return
      setError(e instanceof Error ? e : new Error(String(e)))
    } finally {
      if (my === tick.current) setLoading(false)
    }
  }, [])

  useEffect(() => {
    let timer: ReturnType<typeof setInterval> | undefined
    const start = () => {
      void run()
      timer = setInterval(() => {
        if (document.visibilityState === "visible") void run()
      }, intervalMs)
    }
    start()
    const counter = tick
    return () => {
      if (timer) clearInterval(timer)
      counter.current++ // drop in-flight responses after unmount / key change
    }
  }, [run, intervalMs, key])

  return { data, error, loading, refresh: () => void run() }
}
