"use client"

import { useEffect, useState } from "react"
import { useRouter } from "next/navigation"
import { RadarIcon } from "lucide-react"

import { Alert, AlertDescription } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import {
  Card,
  CardContent,
  CardDescription,
  CardFooter,
  CardHeader,
  CardTitle,
} from "@/components/ui/card"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { api, ApiError } from "@/lib/api"

export default function LoginPage() {
  const router = useRouter()
  const [password, setPassword] = useState("")
  const [error, setError] = useState<string>()
  const [busy, setBusy] = useState(false)

  // A still-valid session skips the form.
  useEffect(() => {
    api
      .me()
      .then((r) => {
        if (r.authenticated) router.replace("/")
      })
      .catch(() => {})
  }, [router])

  async function onSubmit(e: React.FormEvent) {
    e.preventDefault()
    setBusy(true)
    setError(undefined)
    try {
      await api.login(password)
      router.replace("/")
      router.refresh()
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "登录失败")
      setBusy(false)
    }
  }

  return (
    <main className="flex min-h-svh items-center justify-center p-4">
      <Card className="w-full max-w-sm">
        <form onSubmit={onSubmit}>
          <CardHeader>
            <div className="mb-2 flex size-9 items-center justify-center rounded-lg bg-primary text-primary-foreground">
              <RadarIcon className="size-5" />
            </div>
            <CardTitle>Domain Scanner</CardTitle>
            <CardDescription>请输入管理口令以继续</CardDescription>
          </CardHeader>
          <CardContent className="mt-4 flex flex-col gap-4">
            <div className="flex flex-col gap-2">
              <Label htmlFor="password">口令</Label>
              <Input
                id="password"
                type="password"
                autoComplete="current-password"
                autoFocus
                required
                value={password}
                onChange={(e) => setPassword(e.target.value)}
              />
            </div>
            {error && (
              <Alert variant="destructive" role="alert">
                <AlertDescription>{error}</AlertDescription>
              </Alert>
            )}
          </CardContent>
          <CardFooter className="mt-4">
            <Button type="submit" className="w-full" disabled={busy || password.length === 0}>
              {busy ? "登录中…" : "登录"}
            </Button>
          </CardFooter>
        </form>
      </Card>
    </main>
  )
}
