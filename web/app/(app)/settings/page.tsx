"use client"

import { useState } from "react"
import { SendIcon, ShieldCheckIcon } from "lucide-react"
import { toast } from "sonner"

import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import {
  Card,
  CardAction,
  CardContent,
  CardDescription,
  CardFooter,
  CardHeader,
  CardTitle,
} from "@/components/ui/card"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Skeleton } from "@/components/ui/skeleton"
import { usePoll } from "@/hooks/use-poll"
import { api, ApiError } from "@/lib/api"

export default function SettingsPage() {
  const settings = usePoll(api.settings, 60_000)
  // undefined = untouched: show the saved value and do not send it back on save
  const [token, setToken] = useState<string>()
  const [chatId, setChatId] = useState<string>()
  const [saving, setSaving] = useState(false)
  const [testing, setTesting] = useState(false)

  const s = settings.data
  const dirty = token !== undefined || chatId !== undefined

  async function save(e: React.FormEvent) {
    e.preventDefault()
    setSaving(true)
    try {
      await api.saveSettings({
        ...(token !== undefined && { telegram_token: token }),
        ...(chatId !== undefined && { telegram_chat_id: chatId }),
      })
      setToken(undefined)
      setChatId(undefined)
      settings.refresh()
      toast.success("设置已保存")
    } catch (err) {
      toast.error(err instanceof ApiError ? err.message : "保存失败")
    } finally {
      setSaving(false)
    }
  }

  async function test() {
    setTesting(true)
    try {
      await api.testTelegram()
      toast.success("测试消息已发送,请到 Telegram 查看")
    } catch (err) {
      toast.error(err instanceof ApiError ? err.message : "发送失败")
    } finally {
      setTesting(false)
    }
  }

  return (
    <>
      <div>
        <h1 className="text-xl font-semibold">设置</h1>
        <p className="text-sm text-muted-foreground">配置 Telegram 机器人通知。</p>
      </div>

      <Card className="max-w-xl">
        <form onSubmit={save}>
          <CardHeader>
            <CardTitle>Telegram 通知</CardTitle>
            <CardDescription>发现可注册域名时,机器人会把结果推送到指定会话(每 10 秒或满 20 个合并一条)。</CardDescription>
            <CardAction>
              {s && (
                <Badge variant={s.telegram_configured ? "secondary" : "outline"} data-testid="telegram-state">
                  {s.telegram_configured
                    ? s.telegram_source === "env"
                      ? "已配置(环境变量)"
                      : "已配置"
                    : "未配置"}
                </Badge>
              )}
            </CardAction>
          </CardHeader>
          <CardContent className="mt-4 flex flex-col gap-4">
            {settings.error && (
              <Alert variant="destructive">
                <AlertTitle>设置获取失败</AlertTitle>
                <AlertDescription>{settings.error.message}</AlertDescription>
              </Alert>
            )}
            {settings.loading ? (
              <Skeleton className="h-24 w-full" />
            ) : (
              <>
                <div className="flex flex-col gap-2">
                  <Label htmlFor="tg-token">Bot Token</Label>
                  <Input
                    id="tg-token"
                    type="password"
                    autoComplete="off"
                    placeholder="123456789:AAH…"
                    value={token ?? s?.telegram_token ?? ""}
                    onChange={(e) => setToken(e.target.value)}
                    onFocus={(e) => e.target.select()}
                  />
                  <p className="text-xs text-muted-foreground">在 @BotFather 创建机器人获得。保存后只显示脱敏内容。</p>
                </div>
                <div className="flex flex-col gap-2">
                  <Label htmlFor="tg-chat">Chat ID</Label>
                  <Input
                    id="tg-chat"
                    inputMode="numeric"
                    autoComplete="off"
                    placeholder="例如 123456789 或 @channel"
                    value={chatId ?? s?.telegram_chat_id ?? ""}
                    onChange={(e) => setChatId(e.target.value)}
                  />
                  <p className="text-xs text-muted-foreground">先给机器人发一条消息,再用 @userinfobot 等查询自己的 Chat ID。</p>
                </div>
              </>
            )}
          </CardContent>
          <CardFooter className="mt-4 gap-2">
            <Button type="submit" disabled={!dirty || saving}>
              {saving ? "保存中…" : "保存"}
            </Button>
            <Button type="button" variant="outline" onClick={test} disabled={testing || dirty || !s?.telegram_configured}>
              <SendIcon data-icon="inline-start" />
              {testing ? "发送中…" : "发送测试消息"}
            </Button>
            {dirty && <span className="text-xs text-muted-foreground">请先保存再测试</span>}
          </CardFooter>
        </form>
      </Card>

      <Card className="max-w-xl" size="sm">
        <CardHeader>
          <CardTitle className="flex items-center gap-2">
            <ShieldCheckIcon className="size-4" />
            访问保护
          </CardTitle>
          <CardDescription>
            已启用强制口令保护(环境变量 ADMIN_PASSWORD)。会话 7 天有效,连续输错 5 次会锁定 5 分钟;修改口令后所有旧会话失效。
          </CardDescription>
        </CardHeader>
      </Card>
    </>
  )
}
