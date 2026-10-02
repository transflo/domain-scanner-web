"use client"

import { useState } from "react"
import { ActivityIcon, CheckCircle2Icon, XCircleIcon } from "lucide-react"
import { toast } from "sonner"

import { Field, SelectField } from "@/components/field"
import { Alert, AlertDescription } from "@/components/ui/alert"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { Textarea } from "@/components/ui/textarea"
import { api, ApiError } from "@/lib/api"
import {
  buildConfig,
  type BasicForm,
  emptyForm,
  fingerprints,
  formProblem,
  hasStream,
  parseConfig,
  protocols,
  type Protocol,
  roundTrips,
  ssMethods,
  transports,
  type Transport,
  vmessCiphers,
} from "@/lib/outbound-config"
import type { ImportResult, OutboundDetail, ProbeResult } from "@/lib/types"

type Tab = "basics" | "json" | "import"

const pretty = (v: unknown) => JSON.stringify(v, null, 2)

const securityOptions = (p: Protocol) =>
  [
    { value: "none", label: "无" },
    { value: "tls", label: "TLS" },
    ...(p === "vless" ? [{ value: "reality", label: "Reality" }] : []),
  ].filter((o) => p !== "trojan" || o.value !== "none")

export function ProbeSummary({ r }: { r: ProbeResult }) {
  return r.ok ? (
    <div
      className="flex flex-wrap items-center gap-2 text-sm"
      data-testid="probe-result"
    >
      <CheckCircle2Icon className="size-4 text-emerald-600" />
      <span>可用</span>
      <Badge variant="secondary">{r.delay_ms} ms</Badge>
      <span className="text-xs text-muted-foreground">
        冷启动 {r.cold_ms} ms · 出口 {r.ip || "未知"} {r.country}
      </span>
    </div>
  ) : (
    <div
      className="flex items-start gap-2 text-sm text-destructive"
      data-testid="probe-result"
    >
      <XCircleIcon className="mt-0.5 size-4 shrink-0" />
      <span className="min-w-0 break-words">
        不可用:{r.error || "未知错误"}
      </span>
    </div>
  )
}

interface Props {
  open: boolean
  onOpenChange: (open: boolean) => void
  /** The outbound being edited; undefined creates a new one. */
  editing?: OutboundDetail
  onSaved: () => void
}

/** The dialog content is only mounted while open, so the editor starts from fresh state each time. */
export function OutboundDialog({
  open,
  onOpenChange,
  editing,
  onSaved,
}: Props) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent
        className="flex max-h-[90svh] flex-col sm:max-w-2xl"
        data-testid="outbound-dialog"
      >
        <OutboundEditor
          editing={editing}
          onSaved={onSaved}
          close={() => onOpenChange(false)}
        />
      </DialogContent>
    </Dialog>
  )
}

function initialState(editing?: OutboundDetail): {
  tab: Tab
  form: BasicForm
  json: string
} {
  if (!editing) return { tab: "basics", form: emptyForm(), json: "" }
  const parsed = parseConfig(editing.config)
  if (parsed && roundTrips(editing.config))
    return { tab: "basics", form: parsed, json: pretty(editing.config) }
  return { tab: "json", form: emptyForm(), json: pretty(editing.config) }
}

function OutboundEditor({
  editing,
  onSaved,
  close,
}: {
  editing?: OutboundDetail
  onSaved: () => void
  close: () => void
}) {
  const [init] = useState(() => initialState(editing))
  const [tab, setTab] = useState<Tab>(init.tab)
  const [name, setName] = useState(editing?.name ?? "")
  const [form, setForm] = useState<BasicForm>(init.form)
  const [jsonText, setJsonText] = useState(init.json)
  const [importText, setImportText] = useState("")
  const [preview, setPreview] = useState<ImportResult>()
  const [probe, setProbe] = useState<ProbeResult>()
  const [error, setError] = useState<string>()
  const [busy, setBusy] = useState<"" | "test" | "save" | "preview">("")

  const set = <K extends keyof BasicForm>(k: K, v: BasicForm[K]) => {
    setForm((f) => ({ ...f, [k]: v }))
    setProbe(undefined)
  }

  function changeProtocol(p: Protocol) {
    setForm((f) => ({
      ...f,
      protocol: p,
      // keep the security choice valid for the new protocol
      security: !hasStream(p)
        ? "none"
        : p === "trojan" && f.security === "none"
          ? "tls"
          : p !== "vless" && f.security === "reality"
            ? "tls"
            : f.security,
      port:
        p === "socks"
          ? 1080
          : p === "http"
            ? 8080
            : p === "shadowsocks"
              ? 8388
              : f.port,
    }))
    setProbe(undefined)
  }

  function changeTab(next: Tab) {
    if (next === tab) return
    setError(undefined)
    if (next === "json" && tab === "basics") {
      setJsonText(pretty(buildConfig(form)))
    } else if (next === "basics" && tab === "json") {
      let obj: Record<string, unknown>
      try {
        obj = JSON.parse(jsonText)
      } catch {
        if (jsonText.trim() === "") {
          setTab(next)
          return
        }
        toast.error("JSON 格式不正确,请先修正再切换")
        return
      }
      const parsed = parseConfig(obj)
      if (!parsed || !roundTrips(obj)) {
        toast.error("这份配置含有基础表单无法表示的内容,请继续用 JSON 编辑")
        return
      }
      setForm(parsed)
    }
    setTab(next)
  }

  /** The config text to submit, from whichever tab is active. */
  function currentConfig(): { text?: string; problem?: string } {
    if (tab === "json") {
      if (!jsonText.trim()) return { problem: "请填写 JSON 配置" }
      try {
        JSON.parse(jsonText)
      } catch (e) {
        return {
          problem: `JSON 格式不正确:${e instanceof Error ? e.message : ""}`,
        }
      }
      return { text: jsonText }
    }
    const problem = formProblem(form)
    return problem ? { problem } : { text: JSON.stringify(buildConfig(form)) }
  }

  async function runTest() {
    const { text, problem } = currentConfig()
    if (!text) {
      setError(problem)
      return
    }
    setError(undefined)
    setBusy("test")
    setProbe(undefined)
    try {
      setProbe(await api.testOutboundConfig(text))
    } catch (e) {
      setError(e instanceof ApiError ? e.message : "测活失败")
    } finally {
      setBusy("")
    }
  }

  async function save() {
    const { text, problem } = currentConfig()
    if (!text) {
      setError(problem)
      return
    }
    setError(undefined)
    setBusy("save")
    try {
      const saved = editing
        ? await api.updateOutbound(editing.id, {
            name: name.trim() || undefined,
            config: text,
          })
        : await api.createOutbound({
            name: name.trim() || undefined,
            config: text,
          })
      if (saved.reload_error)
        toast.warning(`已保存,但 xray 重载失败:${saved.reload_error}`)
      else toast.success(editing ? "出站代理已更新" : "出站代理已添加")
      onSaved()
      close()
    } catch (e) {
      setError(e instanceof ApiError ? e.message : "保存失败")
    } finally {
      setBusy("")
    }
  }

  async function runPreview() {
    setError(undefined)
    setBusy("preview")
    try {
      setPreview(await api.importOutbounds(importText, false))
    } catch (e) {
      setError(e instanceof ApiError ? e.message : "解析失败")
    } finally {
      setBusy("")
    }
  }

  async function runImport() {
    setError(undefined)
    setBusy("save")
    try {
      const r = await api.importOutbounds(importText, true)
      if (r.reload_error)
        toast.warning(
          `已导入 ${r.saved.length} 个,但 xray 重载失败:${r.reload_error}`
        )
      else
        toast.success(
          `已导入 ${r.saved.length} 个${r.skipped ? `,跳过 ${r.skipped} 个重复` : ""}`
        )
      onSaved()
      close()
    } catch (e) {
      setError(e instanceof ApiError ? e.message : "导入失败")
    } finally {
      setBusy("")
    }
  }

  const stream = hasStream(form.protocol)
  const needsId = form.protocol === "vless" || form.protocol === "vmess"
  const needsPassword =
    form.protocol === "trojan" || form.protocol === "shadowsocks"
  const wsLike =
    form.transport === "ws" ||
    form.transport === "httpupgrade" ||
    form.transport === "xhttp"

  return (
    <>
      <DialogHeader>
        <DialogTitle>
          {editing ? `编辑出站代理 #${editing.id}` : "添加出站代理"}
        </DialogTitle>
        <DialogDescription>
          通过 Xray 内核转发扫描流量。可以用表单填写、直接编辑
          JSON,或粘贴分享链接批量导入。
        </DialogDescription>
      </DialogHeader>

      <Tabs
        value={tab}
        onValueChange={(v) => changeTab(v as Tab)}
        className="min-h-0 flex-1"
      >
        <TabsList className="w-full">
          <TabsTrigger value="basics">基础</TabsTrigger>
          <TabsTrigger value="json">JSON</TabsTrigger>
          {!editing && <TabsTrigger value="import">导入</TabsTrigger>}
        </TabsList>

        <div className="mt-3 min-h-0 flex-1 overflow-y-auto px-0.5">
          <TabsContent value="basics" className="flex flex-col gap-3">
            <div className="grid gap-3 sm:grid-cols-2">
              <Field label="备注名称" htmlFor="ob-name">
                <Input
                  id="ob-name"
                  value={name}
                  onChange={(e) => setName(e.target.value)}
                  placeholder="留空则自动命名"
                />
              </Field>
              <SelectField
                label="协议"
                value={form.protocol}
                onChange={(v) => changeProtocol(v as Protocol)}
                options={protocols}
              />
              <Field
                label="服务器地址"
                htmlFor="ob-address"
                className="sm:col-span-1"
              >
                <Input
                  id="ob-address"
                  value={form.address}
                  onChange={(e) => set("address", e.target.value)}
                  placeholder="example.com"
                  autoComplete="off"
                />
              </Field>
              <Field label="端口" htmlFor="ob-port">
                <Input
                  id="ob-port"
                  type="number"
                  min={1}
                  max={65535}
                  value={form.port}
                  onChange={(e) => set("port", Number(e.target.value))}
                />
              </Field>

              {needsId && (
                <Field label="UUID" htmlFor="ob-id" className="sm:col-span-2">
                  <Input
                    id="ob-id"
                    className="font-mono"
                    value={form.id}
                    onChange={(e) => set("id", e.target.value)}
                    autoComplete="off"
                  />
                </Field>
              )}
              {form.protocol === "vless" && (
                <SelectField
                  label="Flow"
                  value={form.flow || "none"}
                  onChange={(v) => set("flow", v === "none" ? "" : v)}
                  options={[
                    { value: "none", label: "无" },
                    { value: "xtls-rprx-vision", label: "xtls-rprx-vision" },
                  ]}
                />
              )}
              {form.protocol === "vmess" && (
                <SelectField
                  label="加密方式"
                  value={form.vmessSecurity}
                  onChange={(v) => set("vmessSecurity", v)}
                  options={vmessCiphers.map((c) => ({ value: c, label: c }))}
                />
              )}
              {form.protocol === "shadowsocks" && (
                <SelectField
                  label="加密方式"
                  value={form.method}
                  onChange={(v) => set("method", v)}
                  options={ssMethods.map((c) => ({ value: c, label: c }))}
                />
              )}
              {needsPassword && (
                <Field label="密码" htmlFor="ob-pass" className="sm:col-span-2">
                  <Input
                    id="ob-pass"
                    value={form.password}
                    onChange={(e) => set("password", e.target.value)}
                    autoComplete="off"
                  />
                </Field>
              )}
              {(form.protocol === "socks" || form.protocol === "http") && (
                <>
                  <Field label="用户名(可选)" htmlFor="ob-user">
                    <Input
                      id="ob-user"
                      value={form.user}
                      onChange={(e) => set("user", e.target.value)}
                      autoComplete="off"
                    />
                  </Field>
                  <Field label="密码(可选)" htmlFor="ob-userpass">
                    <Input
                      id="ob-userpass"
                      value={form.pass}
                      onChange={(e) => set("pass", e.target.value)}
                      autoComplete="off"
                    />
                  </Field>
                </>
              )}
            </div>

            {stream && (
              <div className="grid gap-3 border-t pt-3 sm:grid-cols-2">
                <SelectField
                  label="传输方式"
                  value={form.transport}
                  onChange={(v) => set("transport", v as Transport)}
                  options={transports}
                />
                <SelectField
                  label="传输层安全"
                  value={form.security}
                  onChange={(v) => set("security", v as BasicForm["security"])}
                  options={securityOptions(form.protocol)}
                />
                {wsLike && (
                  <>
                    <Field label="路径" htmlFor="ob-path">
                      <Input
                        id="ob-path"
                        value={form.path}
                        onChange={(e) => set("path", e.target.value)}
                      />
                    </Field>
                    <Field label="Host(可选)" htmlFor="ob-host">
                      <Input
                        id="ob-host"
                        value={form.host}
                        onChange={(e) => set("host", e.target.value)}
                      />
                    </Field>
                  </>
                )}
                {form.transport === "grpc" && (
                  <Field
                    label="serviceName"
                    htmlFor="ob-svc"
                    className="sm:col-span-2"
                  >
                    <Input
                      id="ob-svc"
                      value={form.serviceName}
                      onChange={(e) => set("serviceName", e.target.value)}
                    />
                  </Field>
                )}
                {form.security !== "none" && (
                  <>
                    <Field label="SNI" htmlFor="ob-sni">
                      <Input
                        id="ob-sni"
                        value={form.sni}
                        onChange={(e) => set("sni", e.target.value)}
                        placeholder="留空则用服务器地址"
                      />
                    </Field>
                    <SelectField
                      label="指纹"
                      value={form.fingerprint || "none"}
                      onChange={(v) =>
                        set("fingerprint", v === "none" ? "" : v)
                      }
                      options={fingerprints.map((f) => ({
                        value: f || "none",
                        label: f || "默认",
                      }))}
                    />
                  </>
                )}
                {form.security === "tls" && (
                  <>
                    <Field label="ALPN(逗号分隔)" htmlFor="ob-alpn">
                      <Input
                        id="ob-alpn"
                        value={form.alpn}
                        onChange={(e) => set("alpn", e.target.value)}
                        placeholder="h2,http/1.1"
                      />
                    </Field>
                    <div className="flex items-center gap-2 self-end pb-2">
                      <Checkbox
                        id="ob-insecure"
                        checked={form.allowInsecure}
                        onCheckedChange={(c) =>
                          set("allowInsecure", c === true)
                        }
                      />
                      <Label htmlFor="ob-insecure" className="font-normal">
                        允许不安全证书
                      </Label>
                    </div>
                  </>
                )}
                {form.security === "reality" && (
                  <>
                    <Field
                      label="公钥 publicKey"
                      htmlFor="ob-pbk"
                      className="sm:col-span-2"
                    >
                      <Input
                        id="ob-pbk"
                        className="font-mono"
                        value={form.publicKey}
                        onChange={(e) => set("publicKey", e.target.value)}
                        autoComplete="off"
                      />
                    </Field>
                    <Field label="shortId" htmlFor="ob-sid">
                      <Input
                        id="ob-sid"
                        className="font-mono"
                        value={form.shortId}
                        onChange={(e) => set("shortId", e.target.value)}
                      />
                    </Field>
                    <Field label="spiderX" htmlFor="ob-spx">
                      <Input
                        id="ob-spx"
                        value={form.spiderX}
                        onChange={(e) => set("spiderX", e.target.value)}
                      />
                    </Field>
                  </>
                )}
              </div>
            )}
          </TabsContent>

          <TabsContent value="json" className="flex flex-col gap-3">
            <Field label="备注名称" htmlFor="ob-name-json">
              <Input
                id="ob-name-json"
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="留空则自动命名"
              />
            </Field>
            <Field
              label="Xray 出站 JSON"
              htmlFor="ob-json"
              hint="Xray outbound 对象(含 protocol / settings / streamSettings)。tag 由系统自动设置。"
            >
              <Textarea
                id="ob-json"
                value={jsonText}
                onChange={(e) => {
                  setJsonText(e.target.value)
                  setProbe(undefined)
                }}
                spellCheck={false}
                className="h-72 font-mono text-xs"
                placeholder={
                  '{\n  "protocol": "vless",\n  "settings": { "address": "example.com", "port": 443, "id": "…", "encryption": "none" },\n  "streamSettings": { "network": "ws", "security": "tls" }\n}'
                }
              />
            </Field>
          </TabsContent>

          {!editing && (
            <TabsContent value="import" className="flex flex-col gap-3">
              <Field
                label="分享链接 / 订阅内容 / JSON"
                htmlFor="ob-import"
                hint="每行一个 vless:// vmess:// trojan:// ss:// socks5:// http:// 链接;也可粘贴 base64 订阅内容,或 xray 配置 / 出站数组 JSON。"
              >
                <Textarea
                  id="ob-import"
                  value={importText}
                  onChange={(e) => {
                    setImportText(e.target.value)
                    setPreview(undefined)
                  }}
                  spellCheck={false}
                  className="h-40 font-mono text-xs"
                  placeholder="vless://…&#10;trojan://…"
                />
              </Field>
              {preview && (
                <div
                  className="flex flex-col gap-2 text-sm"
                  data-testid="import-preview"
                >
                  <p>
                    解析出 <strong>{preview.items.length}</strong> 个节点
                    {preview.errors.length > 0 && (
                      <>
                        ,
                        <span className="text-destructive">
                          {preview.errors.length} 行无法解析
                        </span>
                      </>
                    )}
                  </p>
                  {preview.items.length > 0 && (
                    <ul className="max-h-40 overflow-auto rounded-md border p-2 text-xs">
                      {preview.items.map((it, i) => (
                        <li key={i} className="flex items-center gap-2 py-0.5">
                          <Badge variant="outline">{it.protocol}</Badge>
                          <span className="min-w-0 truncate">{it.name}</span>
                          <span className="ml-auto shrink-0 font-mono text-muted-foreground">
                            {it.address}:{it.port}
                          </span>
                        </li>
                      ))}
                    </ul>
                  )}
                  {preview.errors.length > 0 && (
                    <ul className="max-h-32 overflow-auto rounded-md border border-destructive/40 p-2 text-xs text-destructive">
                      {preview.errors.map((er, i) => (
                        <li key={i} className="py-0.5 break-all">
                          第 {er.line} 行:{er.message}
                          {er.input && (
                            <span className="text-muted-foreground">
                              ({er.input})
                            </span>
                          )}
                        </li>
                      ))}
                    </ul>
                  )}
                </div>
              )}
            </TabsContent>
          )}
        </div>
      </Tabs>

      {error && (
        <Alert variant="destructive" role="alert">
          <AlertDescription className="break-words">{error}</AlertDescription>
        </Alert>
      )}
      {probe && tab !== "import" && <ProbeSummary r={probe} />}

      <DialogFooter className="flex-row flex-wrap justify-end gap-2">
        <Button variant="ghost" onClick={close}>
          取消
        </Button>
        {tab === "import" ? (
          <>
            <Button
              variant="outline"
              onClick={runPreview}
              disabled={!importText.trim() || busy !== ""}
            >
              {busy === "preview" ? "解析中…" : "解析预览"}
            </Button>
            <Button
              onClick={runImport}
              disabled={!importText.trim() || busy !== ""}
            >
              {busy === "save" ? "导入中…" : "导入全部有效节点"}
            </Button>
          </>
        ) : (
          <>
            <Button variant="outline" onClick={runTest} disabled={busy !== ""}>
              <ActivityIcon data-icon="inline-start" />
              {busy === "test" ? "测活中…" : "测活"}
            </Button>
            <Button onClick={save} disabled={busy !== ""}>
              {busy === "save" ? "保存中…" : "保存"}
            </Button>
          </>
        )}
      </DialogFooter>
    </>
  )
}
