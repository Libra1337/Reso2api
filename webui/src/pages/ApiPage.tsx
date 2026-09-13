import { useState, type ReactNode } from "react"
import {
  CalendarClock,
  CalendarCheck2,
  CircleDollarSign,
  Clock,
  Eye,
  EyeOff,
  Key,
  Link2,
  LoaderCircle,
  Pencil,
  Plus,
  RefreshCw,
  ShieldAlert,
  ShieldCheck,
  Trash2,
} from "lucide-react"

import { usePolling } from "@/hooks/use-polling"
import { api } from "@/lib/api-client"
import type { AppState, FeesInfo } from "@/types"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { Button } from "@/components/ui/button"
import { Badge } from "@/components/ui/badge"
import { Alert, AlertDescription } from "@/components/ui/alert"
import { Input } from "@/components/ui/input"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
  DialogFooter,
  DialogClose,
} from "@/components/ui/dialog"
import { CopyButton } from "@/components/shared/CopyButton"
import { LoadingSpinner } from "@/components/shared/LoadingSpinner"
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table"
import { Skeleton } from "@/components/ui/skeleton"
import { cn } from "@/lib/utils"

const CHANNEL_LABEL: Record<string, string> = {
  workbuddy: "WorkBuddy",
  traework: "TraeWork",
  qoder: "Qoder",
}

function maskKey(key: string) {
  if (key.length <= 10) return key
  return `${key.slice(0, 6)}${"•".repeat(12)}${key.slice(-4)}`
}

function ApiKeyChangeDialog({ onDone }: { onDone: () => void }) {
  const [open, setOpen] = useState(false)
  const [draft, setDraft] = useState("")
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const handleSave = async () => {
    const key = draft.trim()
    if (!key) {
      setError("API Key 不能为空")
      return
    }
    setSaving(true)
    setError(null)
    try {
      await api.configApiKey(key)
      setDraft("")
      setOpen(false)
      onDone()
    } catch (err: unknown) {
      const msg = err instanceof Error ? err.message : "保存失败"
      setError(msg)
    } finally {
      setSaving(false)
    }
  }

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger
        render={
          <Button variant="outline" size="sm" className="h-10 w-full sm:h-7 sm:w-auto" />
        }
      >
        <Key className="mr-1.5 h-3.5 w-3.5" />
        修改 Key
      </DialogTrigger>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>修改 API Key</DialogTitle>
        </DialogHeader>
        <div className="space-y-3">
          {error && (
            <Alert variant="destructive">
              <AlertDescription>{error}</AlertDescription>
            </Alert>
          )}
          <div className="grid gap-2">
            <label className="text-xs text-muted-foreground">
              新的 API Key（客户端通过 Authorization: Bearer 访问）
            </label>
            <Input
              value={draft}
              onChange={(event) => setDraft(event.target.value)}
              placeholder="sk-..."
              className="h-10 font-mono md:h-8"
              autoFocus
            />
          </div>
        </div>
        <DialogFooter>
          <DialogClose render={<Button variant="outline" />}>取消</DialogClose>
          <Button onClick={handleSave} disabled={saving || !draft.trim()}>
            {saving ? <LoadingSpinner size={16} className="mr-2" /> : null}
            保存
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

function ScheduleRow({
  icon,
  label,
  description,
  action,
  children,
}: {
  icon: ReactNode
  label: string
  description: string
  action?: ReactNode
  children: ReactNode
}) {
  return (
    <div className="flex flex-col gap-2 rounded-lg border border-border/60 bg-muted/25 px-3 py-2.5 sm:flex-row sm:items-center sm:justify-between">
      <div className="flex min-w-0 items-center gap-3">
        <div className="flex size-8 shrink-0 items-center justify-center rounded-lg bg-background text-muted-foreground">
          {icon}
        </div>
        <div className="min-w-0">
          <p className="text-[13px] font-medium">{label}</p>
          <p className="truncate text-[11px] text-muted-foreground">
            {description}
          </p>
        </div>
      </div>
      <div className="flex items-center gap-2 pl-11 sm:pl-0">
        {children}
        {action}
      </div>
    </div>
  )
}

function CheckinTimesDialog({
  times,
  onSaved,
}: {
  times: string[]
  onSaved: () => void
}) {
  const [open, setOpen] = useState(false)
  const [draft, setDraft] = useState<string[]>([])
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const openDialog = () => {
    setDraft(times.length > 0 ? [...times] : ["09:00"])
    setError(null)
    setOpen(true)
  }

  const update = (index: number, value: string) => {
    setDraft((current) =>
      current.map((t, i) => (i === index ? value : t)),
    )
  }

  const remove = (index: number) => {
    setDraft((current) => current.filter((_, i) => i !== index))
  }

  const add = () => {
    setDraft((current) => [...current, ""])
  }

  const handleSave = async () => {
    const cleaned = draft.map((t) => t.trim()).filter(Boolean)
    if (cleaned.length === 0) {
      setError("请至少保留一个时间点")
      return
    }
    if (!cleaned.every((t) => /^([01]\d|2[0-3]):[0-5]\d$/.test(t))) {
      setError("时间格式应为 HH:MM，如 09:00")
      return
    }
    setSaving(true)
    setError(null)
    try {
      await api.configCheckinTimes(cleaned)
      setOpen(false)
      onSaved()
    } catch (err: unknown) {
      const msg = err instanceof Error ? err.message : "保存失败"
      setError(msg)
    } finally {
      setSaving(false)
    }
  }

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger
        render={
          <Button variant="ghost" size="icon-sm" aria-label="编辑签到时间" />
        }
        onClick={openDialog}
      >
        <Pencil className="size-3.5" />
      </DialogTrigger>
      <DialogContent className="sm:max-w-sm">
        <DialogHeader>
          <DialogTitle>编辑签到时间</DialogTitle>
          <DialogDescription>
            每日在这些时间点自动签到领取额度（服务器时区）
          </DialogDescription>
        </DialogHeader>
        <div className="space-y-3">
          {error && (
            <Alert variant="destructive">
              <AlertDescription>{error}</AlertDescription>
            </Alert>
          )}
          <div className="space-y-2">
            {draft.map((time, index) => (
              <div key={index} className="flex items-center gap-2">
                <Clock className="size-3.5 shrink-0 text-muted-foreground" />
                <Input
                  value={time}
                  onChange={(event) => update(index, event.target.value)}
                  placeholder="HH:MM"
                  className="h-10 font-mono md:h-8"
                />
                <Button
                  variant="ghost"
                  size="icon-sm"
                  className="shrink-0 text-destructive hover:text-destructive"
                  onClick={() => remove(index)}
                  disabled={draft.length <= 1}
                  aria-label="删除此时间点"
                >
                  <Trash2 className="size-3.5" />
                </Button>
              </div>
            ))}
          </div>
          <Button
            variant="outline"
            size="sm"
            className="w-full border-dashed"
            onClick={add}
          >
            <Plus className="mr-1.5 h-3.5 w-3.5" />
            添加时间点
          </Button>
        </div>
        <DialogFooter>
          <DialogClose render={<Button variant="outline" />}>取消</DialogClose>
          <Button onClick={handleSave} disabled={saving}>
            {saving ? <LoadingSpinner size={16} className="mr-2" /> : null}
            保存
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

function ScheduleCard({
  data,
  loading,
  onChanged,
}: {
  data?: AppState
  loading: boolean
  onChanged: () => void
}) {
  const checkinTimes = data?.checkin_times ?? []
  const keepalive = data?.keepalive_hours ?? []

  return (
    <Card className="border-border/60 shadow-sm">
      <CardHeader className="pb-3">
        <CardTitle className="flex items-center gap-2 text-sm font-medium">
          <CalendarClock className="size-4 text-primary" />
          调度策略
        </CardTitle>
      </CardHeader>
      <CardContent className="grid gap-2">
        {loading ? (
          <>
            <Skeleton className="h-14 w-full" />
            <Skeleton className="h-14 w-full" />
          </>
        ) : (
          <>
            <ScheduleRow
              icon={<CalendarCheck2 className="size-4" />}
              label="自动签到"
              description="每日定时签到领取额度，余额恢复自动解冻账号"
              action={<CheckinTimesDialog times={checkinTimes} onSaved={onChanged} />}
            >
              {checkinTimes.map((t) => (
                <Badge key={t} variant="secondary" className="font-mono">
                  {t}
                </Badge>
              ))}
            </ScheduleRow>
            <ScheduleRow
              icon={<ShieldCheck className="size-4" />}
              label="Token 保活"
              description="每日整点刷新账号 token，会话失效自动停用"
            >
              {keepalive.map((h) => (
                <Badge key={h} variant="secondary" className="font-mono">
                  {String(h).padStart(2, "0")}:00
                </Badge>
              ))}
            </ScheduleRow>
          </>
        )}
      </CardContent>
    </Card>
  )
}

function JudgeDialog({
  data,
  onSaved,
}: {
  data?: AppState
  onSaved: () => void
}) {
  const [open, setOpen] = useState(false)
  const [enabled, setEnabled] = useState(false)
  const [baseUrl, setBaseUrl] = useState("")
  const [apiKey, setApiKey] = useState("")
  const [model, setModel] = useState("")
  const [timeoutMs, setTimeoutMs] = useState(4000)
  const [showKey, setShowKey] = useState(false)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const fill = () => {
    setEnabled(!!data?.judge_enabled)
    setBaseUrl(data?.judge_base_url || "")
    setApiKey(data?.judge_api_key || "")
    setModel(data?.judge_model || "")
    setTimeoutMs(data?.judge_timeout_ms && data.judge_timeout_ms > 0 ? data.judge_timeout_ms : 4000)
    setShowKey(false)
    setError(null)
  }

  const handleOpenChange = (next: boolean) => {
    if (next) fill()
    setOpen(next)
  }

  const save = async () => {
    setSaving(true)
    setError(null)
    try {
      await api.configJudge({
        enabled,
        base_url: baseUrl.trim(),
        api_key: apiKey.trim(),
        model: model.trim(),
        timeout_ms: timeoutMs,
      })
      setOpen(false)
      onSaved()
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : "保存失败")
    } finally {
      setSaving(false)
    }
  }

  return (
    <Dialog open={open} onOpenChange={handleOpenChange}>
      <DialogTrigger
        render={
          <Button variant="outline" size="sm" className="h-10 w-full sm:h-7 sm:w-auto" />
        }
      >
        <Pencil className="mr-1.5 h-3.5 w-3.5" />
        配置审查
      </DialogTrigger>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>外部审查</DialogTitle>
          <DialogDescription>
            关键词只做提示。真正拦不拦由外部 LLM 判定：porn / political 才拦截；
            未启用、未配齐或审查失败一律放行。
          </DialogDescription>
        </DialogHeader>
        <div className="space-y-3">
          {error && (
            <Alert variant="destructive">
              <AlertDescription>{error}</AlertDescription>
            </Alert>
          )}
          <label className="flex items-center justify-between rounded-lg border border-border/60 bg-muted/25 px-3 py-2.5 text-sm">
            <span>
              启用 LLM 审查
              <span className="mt-0.5 block text-[11px] text-muted-foreground">
                配齐 Base URL、模型、API Key 后才会生效
              </span>
            </span>
            <input
              type="checkbox"
              className="size-4"
              checked={enabled}
              onChange={(e) => setEnabled(e.target.checked)}
            />
          </label>
          <div className="grid gap-3 sm:grid-cols-2">
            <div className="grid gap-1.5 sm:col-span-2">
              <label className="text-xs text-muted-foreground">审核 Base URL</label>
              <Input
                placeholder="https://api.openai.com"
                value={baseUrl}
                onChange={(e) => setBaseUrl(e.target.value)}
                className="h-10 font-mono text-xs md:h-8"
              />
            </div>
            <div className="grid gap-1.5">
              <label className="text-xs text-muted-foreground">审核模型</label>
              <Input
                placeholder="gpt-4.1-mini"
                value={model}
                onChange={(e) => setModel(e.target.value)}
                className="h-10 font-mono text-xs md:h-8"
              />
            </div>
            <div className="grid gap-1.5">
              <label className="text-xs text-muted-foreground">超时（毫秒）</label>
              <Input
                type="number"
                min={500}
                step={500}
                value={timeoutMs}
                onChange={(e) => setTimeoutMs(Number(e.target.value) || 4000)}
                className="h-10 font-mono text-xs md:h-8"
              />
            </div>
            <div className="grid gap-1.5 sm:col-span-2">
              <label className="text-xs text-muted-foreground">审核 API Key</label>
              <div className="flex items-center gap-1">
                <Input
                  placeholder="sk-..."
                  type={showKey ? "text" : "password"}
                  value={apiKey}
                  onChange={(e) => setApiKey(e.target.value)}
                  className="h-10 font-mono text-xs md:h-8"
                />
                <Button
                  variant="ghost"
                  size="icon-sm"
                  onClick={() => setShowKey((v) => !v)}
                  aria-label={showKey ? "隐藏 Key" : "显示 Key"}
                >
                  {showKey ? <EyeOff /> : <Eye />}
                </Button>
              </div>
            </div>
          </div>
        </div>
        <DialogFooter>
          <DialogClose render={<Button variant="outline" />}>取消</DialogClose>
          <Button onClick={() => void save()} disabled={saving}>
            {saving ? <LoadingSpinner size={16} className="mr-2" /> : null}
            保存
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

function JudgeCard({
  data,
  loading,
  onChanged,
}: {
  data?: AppState
  loading: boolean
  onChanged: () => void
}) {
  return (
    <Card className="border-border/60 shadow-sm">
      <CardHeader className="pb-3">
        <CardTitle className="flex items-center gap-2 text-sm font-medium">
          <ShieldAlert className="size-4 text-primary" />
          外部审查
        </CardTitle>
      </CardHeader>
      <CardContent>
        {loading && !data ? (
          <Skeleton className="h-14 w-full" />
        ) : (
          <ScheduleRow
            icon={<ShieldAlert className="size-4" />}
            label="LLM 审查"
            description="关键词命中后交外部模型判定，未配置则放行"
            action={<JudgeDialog data={data} onSaved={onChanged} />}
          >
            <Badge variant={data?.judge_active ? "default" : "secondary"}>
              {data?.judge_active ? "已生效" : data?.judge_enabled ? "未配齐" : "未启用"}
            </Badge>
            {data?.judge_model ? (
              <Badge variant="outline" className="font-mono">
                {data.judge_model}
              </Badge>
            ) : null}
          </ScheduleRow>
        )}
      </CardContent>
    </Card>
  )
}

function FeesCard({ onChanged }: { onChanged: () => void }) {
  const { data: fees, loading } = usePolling<FeesInfo>(api.fees, 300000)
  const [tab, setTab] = useState<string>("")
  const [refreshing, setRefreshing] = useState(false)

  const channels = fees?.channels ?? []
  const activeTab = channels.some((c) => c.channel === tab)
    ? tab
    : (channels[0]?.channel ?? "")
  const active = channels.find((c) => c.channel === activeTab)

  const refresh = async () => {
    setRefreshing(true)
    try {
      await api.feesRefresh()
      await onChanged()
    } finally {
      setRefreshing(false)
    }
  }

  return (
    <Card className="border-border/60 shadow-sm">
      <CardHeader className="pb-3">
        <div className="flex items-center justify-between gap-3">
          <CardTitle className="flex items-center gap-2 text-sm font-medium">
            <CircleDollarSign className="size-4 text-primary" />
            渠道费率
          </CardTitle>
          <Button
            variant="outline"
            size="sm"
            className="h-7 text-xs"
            disabled={refreshing}
            onClick={() => void refresh()}
          >
            {refreshing ? (
              <LoaderCircle className="mr-1.5 size-3 animate-spin" />
            ) : (
              <RefreshCw className="mr-1.5 size-3" />
            )}
            刷新
          </Button>
        </div>
      </CardHeader>
      <CardContent className="space-y-3">
        {fees?.error && (
          <Alert variant="destructive">
            <AlertDescription>{fees.error}</AlertDescription>
          </Alert>
        )}

        {loading ? (
          <Skeleton className="h-48 w-full" />
        ) : channels.length === 0 ? (
          <div className="rounded-lg border border-dashed border-border px-4 py-10 text-center text-sm text-muted-foreground">
            暂无费率数据 — 添加账号后可从上游拉取
          </div>
        ) : (
          <>
            <div
              role="tablist"
              aria-label="费率渠道"
              className="flex flex-wrap gap-1 rounded-lg bg-muted/40 p-1"
            >
              {channels.map(({ channel, models }) => (
                <button
                  key={channel}
                  type="button"
                  role="tab"
                  aria-selected={channel === activeTab}
                  onClick={() => setTab(channel)}
                  className={cn(
                    "flex items-center gap-1.5 rounded-md px-3 py-1.5 text-xs font-medium transition-colors",
                    channel === activeTab
                      ? "bg-background text-foreground shadow-xs"
                      : "text-muted-foreground hover:text-foreground",
                  )}
                >
                  {CHANNEL_LABEL[channel] ?? channel}
                  <span
                    className={cn(
                      "rounded px-1 font-mono text-[10px]",
                      channel === activeTab
                        ? "bg-primary/10 text-primary"
                        : "bg-muted text-muted-foreground",
                    )}
                  >
                    {models.length}
                  </span>
                </button>
              ))}
            </div>

            {active && (
              <div>
                <p className="mb-2 text-[11px] text-muted-foreground">
                  模型 ID 需带前缀{" "}
                  <code className="rounded bg-muted px-1 font-mono">
                    {active.channel}/
                  </code>
                </p>
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead>模型</TableHead>
                      <TableHead className="text-right">费率</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {active.models.map((m) => (
                      <TableRow key={m.model}>
                        <TableCell className="font-mono text-xs">
                          {m.model}
                          {m.note && (
                            <span className="ml-1.5 text-[11px] text-muted-foreground">
                              {m.note}
                            </span>
                          )}
                        </TableCell>
                        <TableCell className="whitespace-nowrap text-right text-xs">
                          {m.rate}
                        </TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              </div>
            )}
          </>
        )}

        {fees && (
          <p className="text-[11px] text-muted-foreground">
            缓存于 {fees.cached_at || "—"} · {fees.disclaimer}
          </p>
        )}
      </CardContent>
    </Card>
  )
}

export default function ApiPage() {
  const { data, loading, error, refresh } = usePolling<AppState>(
    api.getState,
    10000,
  )
  const [showKey, setShowKey] = useState(false)
  const [notice, setNotice] = useState<string | null>(null)

  const baseUrl = `${window.location.origin}/v1`

  return (
    <div className="mx-auto w-full max-w-[1320px] space-y-5">
      {error && (
        <Alert variant="destructive">
          <AlertDescription>加载失败：{error}</AlertDescription>
        </Alert>
      )}
      {notice && (
        <Alert>
          <AlertDescription>{notice}</AlertDescription>
        </Alert>
      )}

      <div className="grid grid-cols-1 items-start gap-5 xl:grid-cols-2">
        <div className="space-y-5">
          <Card className="border-border/60 shadow-sm">
            <CardHeader className="pb-3">
              <CardTitle className="flex items-center gap-2 text-sm font-medium">
                <Link2 className="size-4 text-primary" />
                OpenAI 兼容端点
              </CardTitle>
            </CardHeader>
            <CardContent className="space-y-3">
              {loading ? (
                <>
                  <Skeleton className="h-10 w-full" />
                  <Skeleton className="h-10 w-full" />
                </>
              ) : (
                <>
                  <div className="flex items-center gap-1 rounded-lg border border-border/60 bg-muted/25 p-2">
                    <code className="min-w-0 flex-1 truncate px-1 font-mono text-xs">
                      {baseUrl}
                    </code>
                    <CopyButton text={baseUrl} />
                  </div>
                  <div className="flex items-center gap-1 rounded-lg border border-border/60 bg-muted/25 p-2">
                    <code className="min-w-0 flex-1 truncate px-1 font-mono text-xs">
                      {showKey ? data?.api_key : maskKey(data?.api_key ?? "")}
                    </code>
                    <Button
                      variant="ghost"
                      size="icon-sm"
                      onClick={() => setShowKey((v) => !v)}
                      aria-label={showKey ? "隐藏 Key" : "显示 Key"}
                    >
                      {showKey ? <EyeOff /> : <Eye />}
                    </Button>
                    <CopyButton text={data?.api_key ?? ""} />
                    <ApiKeyChangeDialog
                      onDone={() => {
                        setNotice("API Key 已更新")
                        void refresh()
                      }}
                    />
                  </div>
                  <p className="text-xs text-muted-foreground">
                    模型 ID 需带渠道前缀：workbuddy/、traework/、qoder/
                  </p>
                </>
              )}
            </CardContent>
          </Card>

          <ScheduleCard
            data={data ?? undefined}
            loading={loading}
            onChanged={() => {
              setNotice("调度策略已更新")
              void refresh()
            }}
          />
          <JudgeCard
            data={data ?? undefined}
            loading={loading}
            onChanged={() => {
              setNotice("外部审查设置已更新")
              void refresh()
            }}
          />
        </div>

        <FeesCard
          onChanged={() => {
            setNotice("费率已刷新")
          }}
        />
      </div>
    </div>
  )
}
