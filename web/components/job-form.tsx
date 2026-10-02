"use client"

import { useRef, useState } from "react"
import { UploadIcon } from "lucide-react"
import { toast } from "sonner"

import { SelectField } from "@/components/field"
import { Alert, AlertDescription } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetFooter,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet"
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { usePoll } from "@/hooks/use-poll"
import { api, ApiError } from "@/lib/api"
import { estimateSpace, fmtNum } from "@/lib/format"
import type { EgressMode, Job, JobParams } from "@/lib/types"

const SOFT_LIMIT = 5_000_000

const patternItems = [
  { value: "d", label: "纯数字(0-9)" },
  { value: "D", label: "纯字母(a-z)" },
  { value: "a", label: "字母 + 数字" },
]

interface Props {
  open: boolean
  onOpenChange: (open: boolean) => void
  onCreated: (job: Job) => void
}

export function JobForm({ open, onOpenChange, onCreated }: Props) {
  const [mode, setMode] = useState<"pattern" | "dictionary">("pattern")
  const [name, setName] = useState("")
  const [suffix, setSuffix] = useState(".com")
  const [pattern, setPattern] = useState("D")
  const [length, setLength] = useState(4)
  const [regex, setRegex] = useState("")
  const [wordlist, setWordlist] = useState("")
  const [workers, setWorkers] = useState(3)
  const [delayMs, setDelayMs] = useState(500)
  const [useReserved, setUseReserved] = useState(false)
  const [force, setForce] = useState(false)
  const [egressMode, setEgressMode] = useState<EgressMode>("direct")
  const [proxyId, setProxyId] = useState("")
  const [failover, setFailover] = useState(true)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string>()
  const fileRef = useRef<HTMLInputElement>(null)

  const lists = usePoll(api.wordlists, 60_000, open ? "open" : "closed")
  const wordlistItems = (lists.data ?? []).map((w) => ({
    value: w.id,
    label: `${w.name}(${fmtNum(w.count)} 词${w.builtin ? ",内置" : ""})`,
  }))

  const proxies = usePoll(api.outbounds, 15_000, open ? "open" : "closed")
  const usable = (proxies.data?.items ?? []).filter((o) => o.enabled)
  const proxyItems = usable.map((o) => ({
    value: String(o.id),
    label: `${o.name}${o.last_test_at ? (o.last_ok ? `(${o.last_delay_ms}ms)` : "(不可用)") : ""}`,
  }))

  const space = mode === "pattern" ? estimateSpace(pattern, length) : null
  const tooLarge = mode === "pattern" && (space === null || space > SOFT_LIMIT)

  async function onUpload(e: React.ChangeEvent<HTMLInputElement>) {
    const file = e.target.files?.[0]
    e.target.value = ""
    if (!file) return
    try {
      const info = await api.uploadWordlist(file.name, file)
      toast.success(`词库已上传:${info.name}(${fmtNum(info.count)} 词)`)
      lists.refresh()
      setWordlist(info.id)
      setMode("dictionary")
    } catch (err) {
      toast.error(err instanceof ApiError ? err.message : "上传失败")
    }
  }

  async function submit(e: React.FormEvent) {
    e.preventDefault()
    setBusy(true)
    setError(undefined)
    const params: JobParams = {
      name: name.trim() || undefined,
      suffix: suffix.trim(),
      regex: regex.trim() || undefined,
      workers,
      delay_ms: delayMs,
      use_reserved: useReserved,
      force,
      egress_mode: egressMode,
      failover,
    }
    if (egressMode === "proxy") params.proxy_id = Number(proxyId)
    if (mode === "pattern") {
      params.pattern = pattern
      params.length = length
    } else {
      params.wordlist = wordlist
    }
    try {
      const job = await api.createJob(params)
      toast.success(`任务 #${job.id} 已创建并开始排队`)
      onCreated(job)
      onOpenChange(false)
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "创建失败")
    } finally {
      setBusy(false)
    }
  }

  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent className="w-full overflow-y-auto sm:max-w-md">
        <form onSubmit={submit} className="flex min-h-full flex-col">
          <SheetHeader>
            <SheetTitle>新建扫描任务</SheetTitle>
            <SheetDescription>任务会在后台运行,关闭页面也不会中断。</SheetDescription>
          </SheetHeader>

          <div className="flex flex-1 flex-col gap-4 px-4">
            <div className="flex flex-col gap-2">
              <Label htmlFor="job-name">名称(可选)</Label>
              <Input id="job-name" value={name} onChange={(e) => setName(e.target.value)} placeholder="例如:4 位字母 .com" />
            </div>

            <div className="flex flex-col gap-2">
              <Label htmlFor="job-suffix">域名后缀</Label>
              <Input
                id="job-suffix"
                required
                value={suffix}
                onChange={(e) => setSuffix(e.target.value)}
                placeholder=".com"
                autoComplete="off"
              />
            </div>

            <Tabs value={mode} onValueChange={(v) => setMode(v as "pattern" | "dictionary")}>
              <TabsList className="w-full">
                <TabsTrigger value="pattern">按规则生成</TabsTrigger>
                <TabsTrigger value="dictionary">字典词库</TabsTrigger>
              </TabsList>
            </Tabs>

            {mode === "pattern" ? (
              <>
                <div className="grid grid-cols-2 gap-3">
                  <div className="flex flex-col gap-2">
                    <Label>字符集</Label>
                    <Select value={pattern} onValueChange={(v) => setPattern(String(v))} items={patternItems}>
                      <SelectTrigger className="w-full" aria-label="字符集">
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent>
                        {patternItems.map((i) => (
                          <SelectItem key={i.value} value={i.value}>
                            {i.label}
                          </SelectItem>
                        ))}
                      </SelectContent>
                    </Select>
                  </div>
                  <div className="flex flex-col gap-2">
                    <Label htmlFor="job-length">长度</Label>
                    <Input
                      id="job-length"
                      type="number"
                      min={1}
                      max={63}
                      value={length}
                      onChange={(e) => setLength(Number(e.target.value))}
                    />
                  </div>
                </div>
                <p className="text-xs text-muted-foreground" data-testid="space-estimate">
                  {space === null ? "候选数量过大" : `共 ${fmtNum(space)} 个候选`}
                  {tooLarge && "(超过 500 万,需勾选「强制」)"}
                </p>
              </>
            ) : (
              <div className="flex flex-col gap-2">
                <Label>词库</Label>
                <div className="flex gap-2">
                  <Select value={wordlist} onValueChange={(v) => setWordlist(String(v))} items={wordlistItems}>
                    <SelectTrigger className="min-w-0 flex-1" aria-label="词库">
                      <SelectValue placeholder="选择词库" />
                    </SelectTrigger>
                    <SelectContent>
                      {wordlistItems.map((i) => (
                        <SelectItem key={i.value} value={i.value}>
                          {i.label}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                  <Button type="button" variant="outline" onClick={() => fileRef.current?.click()}>
                    <UploadIcon data-icon="inline-start" />
                    上传
                  </Button>
                  <input ref={fileRef} type="file" accept=".txt,text/plain" className="hidden" onChange={onUpload} />
                </div>
                <p className="text-xs text-muted-foreground">
                  每行一个词;内置 google-10000-english、english-words-alpha,也可上传自己的 .txt。
                </p>
              </div>
            )}

            <div className="flex flex-col gap-2">
              <Label htmlFor="job-regex">正则过滤(可选)</Label>
              <Input
                id="job-regex"
                value={regex}
                onChange={(e) => setRegex(e.target.value)}
                placeholder="例如 ^[a-z]{3}\d$"
                className="font-mono"
              />
              <p className="text-xs text-muted-foreground">只匹配域名主体(不含后缀)。</p>
            </div>

            <div className="grid grid-cols-2 gap-3">
              <div className="flex flex-col gap-2">
                <Label htmlFor="job-workers">并发数</Label>
                <Input
                  id="job-workers"
                  type="number"
                  min={1}
                  max={50}
                  value={workers}
                  onChange={(e) => setWorkers(Number(e.target.value))}
                />
              </div>
              <div className="flex flex-col gap-2">
                <Label htmlFor="job-delay">每次间隔(毫秒)</Label>
                <Input
                  id="job-delay"
                  type="number"
                  min={0}
                  step={100}
                  value={delayMs}
                  onChange={(e) => setDelayMs(Number(e.target.value))}
                />
              </div>
            </div>

            <div className="flex items-start gap-2">
              <Checkbox id="job-reserved" checked={useReserved} onCheckedChange={(c) => setUseReserved(c === true)} className="mt-0.5" />
              <Label htmlFor="job-reserved" className="flex-col items-start gap-0.5 font-normal">
                <span>启用上游保留名启发式</span>
                <span className="text-xs text-muted-foreground">跳过 1-2 位字母、2-3 位数字等常见保留名(默认关闭,以 RDAP/WHOIS 结果为准)</span>
              </Label>
            </div>

            <div className="flex items-start gap-2">
              <Checkbox id="job-force" checked={force} onCheckedChange={(c) => setForce(c === true)} className="mt-0.5" />
              <Label htmlFor="job-force" className="flex-col items-start gap-0.5 font-normal">
                <span>强制(允许超过 500 万个候选)</span>
                <span className="text-xs text-muted-foreground">大范围扫描会很慢且容易被注册局限流</span>
              </Label>
            </div>

            <div className="flex flex-col gap-3 rounded-lg border p-3" data-testid="egress-section">
              <SelectField
                label="出站方式"
                value={egressMode}
                onChange={(v) => setEgressMode(v as EgressMode)}
                options={[
                  { value: "direct", label: "直连(本机网络)" },
                  { value: "proxy", label: "指定代理" },
                  { value: "pool", label: "代理池(任一可用代理)" },
                ]}
                hint={
                  egressMode === "direct"
                    ? "直连任务数量不受限制;出现大量错误时会自动退避,并切换到可用代理。"
                    : egressMode === "pool"
                      ? "在所有已启用且测活通过的代理中择优使用。"
                      : "全部流量经所选代理发出。"
                }
              />
              {egressMode === "proxy" && (
                <SelectField
                  label="代理"
                  value={proxyId}
                  onChange={setProxyId}
                  options={proxyItems}
                  placeholder={proxyItems.length ? "选择代理" : "还没有可用代理"}
                  hint={proxyItems.length === 0 ? "请先到「出站代理」页添加并启用代理。" : undefined}
                />
              )}
              <div className="flex items-start gap-2">
                <Checkbox id="job-failover" checked={failover} onCheckedChange={(c) => setFailover(c === true)} className="mt-0.5" />
                <Label htmlFor="job-failover" className="flex-col items-start gap-0.5 font-normal">
                  <span>出错时自动切换</span>
                  <span className="text-xs text-muted-foreground">
                    当前出站持续报错(限流/超时/断连)时退避等待,并改用其它可用出站;恢复后自动切回。
                  </span>
                </Label>
              </div>
            </div>

            {error && (
              <Alert variant="destructive" role="alert">
                <AlertDescription>{error}</AlertDescription>
              </Alert>
            )}
          </div>

          <SheetFooter className="mt-4">
            <Button type="submit" disabled={busy || (mode === "dictionary" && !wordlist) || (tooLarge && !force) || (egressMode === "proxy" && !proxyId)}>
              {busy ? "创建中…" : "创建并开始"}
            </Button>
          </SheetFooter>
        </form>
      </SheetContent>
    </Sheet>
  )
}
