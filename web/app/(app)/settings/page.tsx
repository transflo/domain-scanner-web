"use client"

import { useState } from "react"
import { CloudIcon, SendIcon, ShieldCheckIcon } from "lucide-react"
import { toast } from "sonner"

import { Field, SelectField } from "@/components/field"
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
import { Checkbox } from "@/components/ui/checkbox"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Skeleton } from "@/components/ui/skeleton"
import { usePoll } from "@/hooks/use-poll"
import { api, ApiError } from "@/lib/api"
import { levelLabel } from "@/lib/format"
import type { LogLevel, Settings, SettingsUpdate } from "@/lib/types"

type Key = keyof SettingsUpdate

const telegramKeys: Key[] = ["telegram_token", "telegram_chat_id"]
const cloudflareKeys: Key[] = ["cloudflare_account_id", "cloudflare_token"]
const policyKeys: Key[] = ["register_confirm", "register_max_price", "register_daily_cap", "push_unconfirmed"]
const systemKeys: Key[] = ["log_level", "proxy_test_url"]

const logLevelItems = (["debug", "info", "warn", "error"] as LogLevel[]).map((l) => ({
  value: l,
  label: `${levelLabel[l]}(${l})`,
}))

export default function SettingsPage() {
  const settings = usePoll(api.settings, 60_000)
  // A key is absent while untouched: the saved value shows and is not sent back on save.
  const [draft, setDraft] = useState<SettingsUpdate>({})
  const [saving, setSaving] = useState<string>()
  const [testing, setTesting] = useState<"" | "telegram" | "cloudflare">("")

  const s = settings.data
  const val = <K extends Key>(k: K) => (draft[k] !== undefined ? draft[k] : s?.[k as keyof Settings]) as SettingsUpdate[K]
  const edit = <K extends Key>(k: K, v: SettingsUpdate[K]) => setDraft((d) => ({ ...d, [k]: v }))
  const dirty = (keys: Key[]) => keys.some((k) => draft[k] !== undefined)

  async function save(name: string, keys: Key[], e?: React.FormEvent) {
    e?.preventDefault()
    const body: Record<string, unknown> = {}
    for (const k of keys) if (draft[k] !== undefined) body[k] = draft[k]
    setSaving(name)
    try {
      await api.saveSettings(body as SettingsUpdate)
      setDraft((d) => {
        const n = { ...d }
        for (const k of keys) delete n[k]
        return n
      })
      settings.refresh()
      toast.success("设置已保存")
    } catch (err) {
      toast.error(err instanceof ApiError ? err.message : "保存失败")
    } finally {
      setSaving(undefined)
    }
  }

  async function test(which: "telegram" | "cloudflare") {
    setTesting(which)
    try {
      if (which === "telegram") {
        await api.testTelegram()
        toast.success("测试消息已发送,请到 Telegram 查看")
      } else {
        await api.testCloudflare()
        toast.success("Cloudflare 凭据校验通过")
      }
    } catch (err) {
      toast.error(err instanceof ApiError ? err.message : "测试失败")
    } finally {
      setTesting("")
    }
  }

  return (
    <>
      <div>
        <h1 className="text-xl font-semibold">设置</h1>
        <p className="text-sm text-muted-foreground">Telegram 通知、Cloudflare 注册核查与一键注册、日志与代理测活。</p>
      </div>

      {settings.error && (
        <Alert variant="destructive">
          <AlertTitle>设置获取失败</AlertTitle>
          <AlertDescription>{settings.error.message}</AlertDescription>
        </Alert>
      )}

      <div className="grid max-w-5xl gap-4 lg:grid-cols-2">
        <Card>
          <form onSubmit={(e) => save("telegram", telegramKeys, e)}>
            <CardHeader>
              <CardTitle>Telegram 通知</CardTitle>
              <CardDescription>
                只有经 Cloudflare 确认可注册的域名才会推送,并带「一键注册」按钮(每 10 秒或满 20 个合并一条)。
              </CardDescription>
              <CardAction>
                {s && (
                  <Badge variant={s.telegram_configured ? "secondary" : "outline"} data-testid="telegram-state">
                    {s.telegram_configured ? (s.telegram_source === "env" ? "已配置(环境变量)" : "已配置") : "未配置"}
                  </Badge>
                )}
              </CardAction>
            </CardHeader>
            <CardContent className="mt-4 flex flex-col gap-4">
              {settings.loading ? (
                <Skeleton className="h-24 w-full" />
              ) : (
                <>
                  <Field label="Bot Token" htmlFor="tg-token" hint="在 @BotFather 创建机器人获得。保存后只显示脱敏内容。">
                    <Input
                      id="tg-token"
                      type="password"
                      autoComplete="off"
                      placeholder="123456789:AAH…"
                      value={val("telegram_token") ?? ""}
                      onChange={(e) => edit("telegram_token", e.target.value)}
                      onFocus={(e) => e.target.select()}
                    />
                  </Field>
                  <Field
                    label="Chat ID"
                    htmlFor="tg-chat"
                    hint="先给机器人发一条消息,再用 @userinfobot 等查询自己的 Chat ID。一键注册按钮只在私聊(数字 ID)中出现。"
                  >
                    <Input
                      id="tg-chat"
                      inputMode="numeric"
                      autoComplete="off"
                      placeholder="例如 123456789 或 @channel"
                      value={val("telegram_chat_id") ?? ""}
                      onChange={(e) => edit("telegram_chat_id", e.target.value)}
                    />
                  </Field>
                </>
              )}
            </CardContent>
            <CardFooter className="mt-4 flex-wrap gap-2">
              <Button type="submit" disabled={!dirty(telegramKeys) || saving === "telegram"}>
                {saving === "telegram" ? "保存中…" : "保存"}
              </Button>
              <Button
                type="button"
                variant="outline"
                onClick={() => test("telegram")}
                disabled={testing !== "" || dirty(telegramKeys) || !s?.telegram_configured}
              >
                <SendIcon data-icon="inline-start" />
                {testing === "telegram" ? "发送中…" : "发送测试消息"}
              </Button>
              {dirty(telegramKeys) && <span className="text-xs text-muted-foreground">请先保存再测试</span>}
            </CardFooter>
          </form>
        </Card>

        <Card>
          <form onSubmit={(e) => save("cloudflare", cloudflareKeys, e)}>
            <CardHeader>
              <CardTitle className="flex items-center gap-2">
                <CloudIcon className="size-4" />
                Cloudflare Registrar
              </CardTitle>
              <CardDescription>
                用 Registrar API 对扫描到的域名做最终核查(可注册性与价格),并执行一键注册。建议把 Token 权限限定为 Registrar。
              </CardDescription>
              <CardAction>
                {s && (
                  <Badge variant={s.cloudflare_configured ? "secondary" : "outline"} data-testid="cloudflare-state">
                    {s.cloudflare_configured ? "已配置" : "未配置"}
                  </Badge>
                )}
              </CardAction>
            </CardHeader>
            <CardContent className="mt-4 flex flex-col gap-4">
              {settings.loading ? (
                <Skeleton className="h-24 w-full" />
              ) : (
                <>
                  <Field label="账户 ID" htmlFor="cf-account" hint="32 位十六进制,在 Cloudflare 控制台右侧栏可见。">
                    <Input
                      id="cf-account"
                      autoComplete="off"
                      className="font-mono"
                      value={val("cloudflare_account_id") ?? ""}
                      onChange={(e) => edit("cloudflare_account_id", e.target.value)}
                    />
                  </Field>
                  <Field label="API Token" htmlFor="cf-token" hint="保存后只显示末四位。留空保存即清除。">
                    <Input
                      id="cf-token"
                      type="password"
                      autoComplete="off"
                      value={val("cloudflare_token") ?? ""}
                      onChange={(e) => edit("cloudflare_token", e.target.value)}
                      onFocus={(e) => e.target.select()}
                    />
                  </Field>
                </>
              )}
            </CardContent>
            <CardFooter className="mt-4 flex-wrap gap-2">
              <Button type="submit" disabled={!dirty(cloudflareKeys) || saving === "cloudflare"}>
                {saving === "cloudflare" ? "保存中…" : "保存"}
              </Button>
              <Button
                type="button"
                variant="outline"
                onClick={() => test("cloudflare")}
                disabled={testing !== "" || dirty(cloudflareKeys) || !s?.cloudflare_configured}
              >
                {testing === "cloudflare" ? "校验中…" : "校验凭据"}
              </Button>
              {dirty(cloudflareKeys) && <span className="text-xs text-muted-foreground">请先保存再校验</span>}
            </CardFooter>
          </form>
        </Card>

        <Card>
          <form onSubmit={(e) => save("policy", policyKeys, e)}>
            <CardHeader>
              <CardTitle>一键注册策略</CardTitle>
              <CardDescription>
                注册会真实扣费且不可退款。点按钮后默认再确认一次;超过价格上限或当日次数上限时会拒绝。
              </CardDescription>
            </CardHeader>
            <CardContent className="mt-4 flex flex-col gap-4">
              {settings.loading ? (
                <Skeleton className="h-24 w-full" />
              ) : (
                <>
                  <div className="flex items-start gap-2">
                    <Checkbox
                      id="reg-confirm"
                      checked={val("register_confirm") ?? true}
                      onCheckedChange={(c) => edit("register_confirm", c === true)}
                      className="mt-0.5"
                    />
                    <Label htmlFor="reg-confirm" className="flex-col items-start gap-0.5 font-normal">
                      <span>注册前二次确认(强烈建议开启)</span>
                      <span className="text-xs text-muted-foreground">
                        开启:点「注册」→ 看到域名和价格 → 再点「确认注册」才扣费。关闭:点一次就直接注册。
                      </span>
                    </Label>
                  </div>
                  <div className="grid gap-3 sm:grid-cols-2">
                    <Field label="单价上限(美元)" htmlFor="reg-max" hint="0 表示不限制。">
                      <Input
                        id="reg-max"
                        inputMode="decimal"
                        value={val("register_max_price") ?? ""}
                        onChange={(e) => edit("register_max_price", e.target.value)}
                      />
                    </Field>
                    <Field label="每日注册上限(个)" htmlFor="reg-cap" hint="0 表示不限制。">
                      <Input
                        id="reg-cap"
                        type="number"
                        min={0}
                        max={1000}
                        value={val("register_daily_cap") ?? 0}
                        onChange={(e) => edit("register_daily_cap", Number(e.target.value))}
                      />
                    </Field>
                  </div>
                  <div className="flex items-start gap-2">
                    <Checkbox
                      id="push-unconfirmed"
                      checked={val("push_unconfirmed") ?? true}
                      onCheckedChange={(c) => edit("push_unconfirmed", c === true)}
                      className="mt-0.5"
                    />
                    <Label htmlFor="push-unconfirmed" className="flex-col items-start gap-0.5 font-normal">
                      <span>仍推送 Cloudflare 不支持的后缀</span>
                      <span className="text-xs text-muted-foreground">
                        如 .li / .ch / .sh / .cc:消息会标注「未经 Cloudflare 确认」,且没有注册按钮。
                      </span>
                    </Label>
                  </div>
                </>
              )}
            </CardContent>
            <CardFooter className="mt-4">
              <Button type="submit" disabled={!dirty(policyKeys) || saving === "policy"}>
                {saving === "policy" ? "保存中…" : "保存"}
              </Button>
            </CardFooter>
          </form>
        </Card>

        <Card>
          <form onSubmit={(e) => save("system", systemKeys, e)}>
            <CardHeader>
              <CardTitle>日志与代理</CardTitle>
              <CardDescription>日志级别立即生效;调试级别记录每一步检查,体积较大,排障时再开。</CardDescription>
            </CardHeader>
            <CardContent className="mt-4 flex flex-col gap-4">
              {settings.loading ? (
                <Skeleton className="h-24 w-full" />
              ) : (
                <>
                  <SelectField
                    label="日志记录级别"
                    value={val("log_level") ?? "info"}
                    onChange={(v) => edit("log_level", v as LogLevel)}
                    options={logLevelItems}
                    hint="低于该级别的日志不会保存。"
                  />
                  <Field label="代理测活地址" htmlFor="proxy-test-url" hint="测活时通过每个代理请求该地址(留空恢复默认)。">
                    <Input
                      id="proxy-test-url"
                      className="font-mono"
                      value={val("proxy_test_url") ?? ""}
                      onChange={(e) => edit("proxy_test_url", e.target.value)}
                    />
                  </Field>
                </>
              )}
            </CardContent>
            <CardFooter className="mt-4">
              <Button type="submit" disabled={!dirty(systemKeys) || saving === "system"}>
                {saving === "system" ? "保存中…" : "保存"}
              </Button>
            </CardFooter>
          </form>
        </Card>

        <Card size="sm" className="lg:col-span-2">
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
      </div>
    </>
  )
}
