import { useState, useEffect, useCallback, useRef } from "react";
import {
  CalendarCheck2,
  CheckCircle2,
  Coins,
  ExternalLink,
  FileJson,
  LoaderCircle,
  PauseCircle,
  PlayCircle,
  Plus,
  ShieldAlert,
  RefreshCw,
  Trash2,
  XCircle,
} from "lucide-react";

import { api } from "@/lib/api-client";
import type {
  AppState,
  BanCheckAllResult,
  BanCheckResult,
  CheckinAllResult,
  RefreshAllResult,
  ResourceDetail,
} from "@/types";
import { cn } from "@/lib/utils";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Textarea } from "@/components/ui/textarea";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
  DialogFooter,
  DialogClose,
} from "@/components/ui/dialog";
import { LoadingSpinner } from "@/components/shared/LoadingSpinner";
import { PaginationControls } from "@/components/shared/PaginationControls";
import { Skeleton } from "@/components/ui/skeleton";

const ACCOUNTS_PAGE_SIZE = 5;

function formatBanCheck(r: BanCheckResult) {
  const extra = r.detail ? `：${r.detail}` : "";
  switch (r.status) {
    case "ok":
      return "账号正常，未被封禁";
    case "banned":
      return `已封禁/拉黑，已停用${extra}`;
    case "session_dead":
      return `登录失效，需重新登录${extra}`;
    default:
      return `探测失败${extra}`;
  }
}

const CHANNEL_LABEL: Record<string, string> = {
  workbuddy: "WorkBuddy",
  traework: "TraeWork",
  qoder: "Qoder",
};

const CHANNEL_DESC: Record<string, string> = {
  workbuddy: "腾讯 CodeBuddy / WorkBuddy",
  traework: "字节 TraeWork",
  qoder: "阿里 Qoder",
};

function metric(label: string, value: string | number) {
  return (
    <div className="rounded-lg border border-border/60 bg-muted/25 px-3 py-2">
      <p className="text-[11px] text-muted-foreground">{label}</p>
      <p className="mt-1 text-lg font-semibold">{value}</p>
    </div>
  );
}

function AccountDetail({
  label,
  value,
  mono = false,
}: {
  label: string;
  value: string | number;
  mono?: boolean;
}) {
  return (
    <div className="min-w-0 rounded-lg bg-muted/35 px-3 py-2.5">
      <p className="text-[11px] text-muted-foreground">{label}</p>
      <p
        className={`mt-1 truncate font-medium ${
          mono ? "font-mono text-xs" : "text-sm"
        }`}
        title={String(value)}
      >
        {value}
      </p>
    </div>
  );
}

type LoginStage =
  { phase: "pick" } | { phase: "waiting"; channel: string; authUrl: string };

function AddAccountDialog({ onDone }: { onDone: () => void }) {
  const [open, setOpen] = useState(false);
  const [stage, setStage] = useState<LoginStage>({ phase: "pick" });
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const timerRef = useRef<ReturnType<typeof setInterval> | null>(null);
  const countRef = useRef(0);

  const stopPolling = () => {
    if (timerRef.current) clearInterval(timerRef.current);
    timerRef.current = null;
  };

  useEffect(() => stopPolling, []);

  const reset = () => {
    stopPolling();
    setStage({ phase: "pick" });
    setError(null);
  };

  const handleOpenChange = (next: boolean) => {
    if (!next) {
      if (stage.phase === "waiting") {
        stopPolling();
        void api.loginCancel().catch(() => {});
      }
      reset();
    }
    setOpen(next);
  };

  const start = async (channel: string) => {
    setBusy(true);
    setError(null);
    try {
      const before = await api.getState();
      countRef.current = before.accounts.length;
      const r = await api.loginStart(channel);
      setStage({ phase: "waiting", channel, authUrl: r.auth_url });
      window.open(r.auth_url, "_blank", "noopener");
      stopPolling();
      timerRef.current = setInterval(async () => {
        try {
          const st = await api.getState();
          if (!st.login_busy) {
            stopPolling();
            const added = st.accounts.length > countRef.current;
            if (added) {
              handleOpenChange(false);
              onDone();
            } else {
              setStage({ phase: "pick" });
              setError("未检测到新账号，可能登录未完成，请重试");
            }
          }
        } catch {
          /* 轮询失败静默重试 */
        }
      }, 2000);
    } catch (err: unknown) {
      const msg = err instanceof Error ? err.message : "发起登录失败";
      setError(msg);
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog open={open} onOpenChange={handleOpenChange}>
      <DialogTrigger
        render={
          <Button
            variant="outline"
            size="sm"
            className="h-10 w-full sm:h-7 sm:w-auto"
          />
        }
      >
        <Plus className="mr-1.5 h-3.5 w-3.5" />
        OAuth 登录添加
      </DialogTrigger>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>
            {stage.phase === "waiting"
              ? `登录 ${CHANNEL_LABEL[stage.channel]}`
              : "添加账号"}
          </DialogTitle>
        </DialogHeader>
        <div className="space-y-3">
          {error && (
            <Alert variant="destructive">
              <AlertDescription>{error}</AlertDescription>
            </Alert>
          )}
          {stage.phase === "pick" ? (
            <div className="grid gap-2">
              {["workbuddy", "traework", "qoder"].map((ch) => (
                <Button
                  key={ch}
                  variant="outline"
                  disabled={busy}
                  className="h-auto justify-between px-4 py-3"
                  onClick={() => void start(ch)}
                >
                  <span className="flex flex-col items-start">
                    <span className="text-sm font-medium">
                      {CHANNEL_LABEL[ch]}
                    </span>
                    <span className="text-xs text-muted-foreground">
                      {CHANNEL_DESC[ch]}
                    </span>
                  </span>
                  <ExternalLink className="size-4 text-muted-foreground" />
                </Button>
              ))}
            </div>
          ) : (
            <div className="space-y-4">
              <div className="flex items-center gap-2 text-sm text-muted-foreground">
                <LoaderCircle className="size-4 animate-spin text-primary" />
                已打开授权页面，等待登录完成…
              </div>
              <div className="flex items-center gap-1 rounded-md border border-border bg-muted p-2">
                <code className="min-w-0 flex-1 truncate px-1 font-mono text-xs">
                  {stage.authUrl}
                </code>
              </div>
              <div className="flex gap-2">
                <Button
                  variant="secondary"
                  className="flex-1"
                  onClick={() =>
                    window.open(stage.authUrl, "_blank", "noopener")
                  }
                >
                  <ExternalLink className="mr-1.5 h-3.5 w-3.5" />
                  重新打开授权页
                </Button>
                <DialogClose
                  render={
                    <Button
                      variant="ghost"
                      onClick={() => handleOpenChange(false)}
                    />
                  }
                >
                  取消
                </DialogClose>
              </div>
            </div>
          )}
        </div>
      </DialogContent>
    </Dialog>
  );
}

interface ImportItem {
  name: string;
  status: "pending" | "importing" | "ok" | "error";
  detail?: string;
}

function ImportDialog({ onDone }: { onDone: () => void }) {
  const [open, setOpen] = useState(false);
  const [raw, setRaw] = useState("");
  const [items, setItems] = useState<ImportItem[]>([]);
  const [busy, setBusy] = useState(false);
  const [dragOver, setDragOver] = useState(false);
  const fileInputRef = useRef<HTMLInputElement>(null);

  const handleOpenChange = (next: boolean) => {
    if (!next) {
      setRaw("");
      setItems([]);
    }
    setOpen(next);
  };

  const patchItem = (index: number, patch: Partial<ImportItem>) => {
    setItems((current) =>
      current.map((it, idx) => (idx === index ? { ...it, ...patch } : it)),
    );
  };

  const runImports = async (entries: { name: string; content: string }[]) => {
    setBusy(true);
    setItems(entries.map((entry) => ({ name: entry.name, status: "pending" })));
    let anyOk = false;
    for (let i = 0; i < entries.length; i++) {
      patchItem(i, { status: "importing" });
      try {
        const r = await api.accountImport(entries[i].content);
        if (r.ok) {
          anyOk = true;
          patchItem(i, {
            status: "ok",
            detail: r.nickname || r.uid?.slice(0, 8) || "已导入",
          });
        } else {
          patchItem(i, { status: "error", detail: r.error || "导入失败" });
        }
      } catch (err: unknown) {
        patchItem(i, {
          status: "error",
          detail: err instanceof Error ? err.message : "导入失败",
        });
      }
    }
    setBusy(false);
    if (anyOk) onDone();
  };

  const handleFiles = async (files: FileList | File[]) => {
    const arr = Array.from(files).filter(
      (f) =>
        f.name.toLowerCase().endsWith(".json") || f.type === "application/json",
    );
    if (arr.length === 0) {
      setItems([
        {
          name: "（未选择 JSON 文件）",
          status: "error",
          detail: "仅支持 .json 文件",
        },
      ]);
      return;
    }
    const entries = await Promise.all(
      arr.map(async (f) => ({
        name: f.name,
        content: (await f.text()).replace(/^\uFEFF/, ""),
      })),
    );
    await runImports(entries);
  };

  const handlePasteImport = async () => {
    const text = raw.trim().replace(/^\uFEFF/, "");
    if (!text) return;
    let entries: { name: string; content: string }[];
    try {
      const parsed: unknown = JSON.parse(text);
      if (Array.isArray(parsed)) {
        entries = parsed.map((obj, i) => ({
          name: `粘贴 #${i + 1}`,
          content: JSON.stringify(obj),
        }));
      } else {
        entries = [{ name: "粘贴的 JSON", content: text }];
      }
    } catch {
      // 交给后端给出精确的解析错误
      entries = [{ name: "粘贴的 JSON", content: text }];
    }
    await runImports(entries);
  };

  return (
    <Dialog open={open} onOpenChange={handleOpenChange}>
      <DialogTrigger
        render={
          <Button
            variant="outline"
            size="sm"
            className="h-10 w-full sm:h-7 sm:w-auto"
          />
        }
      >
        <FileJson className="mr-1.5 h-3.5 w-3.5" />
        导入 JSON
      </DialogTrigger>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>导入账号凭证</DialogTitle>
          <DialogDescription>
            支持单号 JSON、多号数组合并，以及 workbuddy-desktop 导出文件
          </DialogDescription>
        </DialogHeader>
        <div className="space-y-3">
          <div
            role="button"
            tabIndex={0}
            aria-label="选择或拖拽 JSON 文件"
            onClick={() => fileInputRef.current?.click()}
            onKeyDown={(e) => {
              if (e.key === "Enter" || e.key === " ")
                fileInputRef.current?.click();
            }}
            onDragOver={(e) => {
              e.preventDefault();
              setDragOver(true);
            }}
            onDragLeave={() => setDragOver(false)}
            onDrop={(e) => {
              e.preventDefault();
              setDragOver(false);
              if (!busy) void handleFiles(e.dataTransfer.files);
            }}
            className={cn(
              "flex cursor-pointer select-none flex-col items-center gap-2 rounded-lg border border-dashed px-4 py-8 text-center transition-colors",
              dragOver
                ? "border-primary bg-primary/5"
                : "border-border hover:bg-muted/40",
            )}
          >
            <FileJson className="size-7 text-muted-foreground" />
            <p className="text-sm font-medium">点击选择文件，或拖拽到此处</p>
            <p className="text-xs text-muted-foreground">
              支持一次选择多个 JSON 文件
            </p>
          </div>
          <input
            ref={fileInputRef}
            type="file"
            accept=".json,application/json"
            multiple
            className="hidden"
            onChange={(e) => {
              if (e.target.files && e.target.files.length > 0) {
                void handleFiles(e.target.files);
              }
              e.target.value = "";
            }}
          />

          {items.length > 0 && (
            <div className="max-h-44 space-y-1.5 overflow-auto pr-1">
              {items.map((item, index) => (
                <div
                  key={index}
                  className="flex items-center gap-2 rounded-md border border-border/60 bg-muted/25 px-2.5 py-1.5 text-xs"
                >
                  {item.status === "importing" && (
                    <LoaderCircle className="size-3.5 shrink-0 animate-spin text-primary" />
                  )}
                  {item.status === "ok" && (
                    <CheckCircle2 className="size-3.5 shrink-0 text-success" />
                  )}
                  {item.status === "error" && (
                    <XCircle className="size-3.5 shrink-0 text-destructive" />
                  )}
                  {item.status === "pending" && (
                    <FileJson className="size-3.5 shrink-0 text-muted-foreground" />
                  )}
                  <span className="min-w-0 flex-1 truncate font-mono">
                    {item.name}
                  </span>
                  {item.status === "ok" && item.detail && (
                    <span className="shrink-0 text-muted-foreground">
                      {item.detail}
                    </span>
                  )}
                  {item.status === "error" && item.detail && (
                    <span className="max-w-45 shrink-0 truncate text-destructive">
                      {item.detail}
                    </span>
                  )}
                </div>
              ))}
            </div>
          )}

          <div className="flex items-center gap-2 text-[11px] text-muted-foreground">
            <span className="h-px flex-1 bg-border" />
            或粘贴 JSON
            <span className="h-px flex-1 bg-border" />
          </div>
          <Textarea
            placeholder='单个 {"account":...} 或数组 [{...},{...}]'
            value={raw}
            onChange={(event) => setRaw(event.target.value)}
            rows={3}
            className="font-mono text-xs"
          />
        </div>
        <DialogFooter>
          <DialogClose render={<Button variant="outline" />}>关闭</DialogClose>
          <Button
            onClick={() => void handlePasteImport()}
            disabled={busy || !raw.trim()}
          >
            {busy ? <LoadingSpinner size={16} className="mr-2" /> : null}
            导入粘贴内容
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

interface DetailView {
  account: string;
  channel: string;
  uid: string;
}

function CreditDetailDialog({
  view,
  onClose,
}: {
  view: DetailView | null;
  onClose: () => void;
}) {
  const [data, setData] = useState<ResourceDetail | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (!view) {
      setData(null);
      setError(null);
      return;
    }
    let cancelled = false;
    setLoading(true);
    setError(null);
    setData(null);
    api
      .accountResourceDetail(view.uid)
      .then((r) => {
        if (!cancelled) setData(r);
      })
      .catch((err: unknown) => {
        if (!cancelled) {
          setError(err instanceof Error ? err.message : "加载失败");
        }
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [view]);

  return (
    <Dialog open={view !== null} onOpenChange={(next) => !next && onClose()}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle className="flex items-center gap-2">
            积分明细
            {view && (
              <span className="truncate text-sm font-normal text-muted-foreground">
                {view.account}
              </span>
            )}
          </DialogTitle>
          {view && (
            <DialogDescription>
              {CHANNEL_LABEL[view.channel] ?? view.channel} ·{" "}
              <span className="font-mono">{view.uid.slice(0, 8)}</span>
            </DialogDescription>
          )}
        </DialogHeader>

        {loading ? (
          <div className="space-y-3">
            <Skeleton className="h-16 w-full" />
            <Skeleton className="h-14 w-full" />
            <Skeleton className="h-14 w-full" />
          </div>
        ) : error ? (
          <Alert variant="destructive">
            <AlertDescription>{error}</AlertDescription>
          </Alert>
        ) : data ? (
          <div className="space-y-4">
            <div className="flex items-center justify-between rounded-lg border border-border/60 bg-muted/25 px-3 py-2.5">
              <div>
                <p className="text-[11px] text-muted-foreground">总剩余积分</p>
                <p className="mt-0.5 font-mono text-2xl font-bold tracking-tight text-primary">
                  {(data.remain ?? 0).toLocaleString()}
                </p>
              </div>
              <div className="flex size-9 items-center justify-center rounded-lg bg-primary/10 text-primary">
                <Coins className="size-4.5" />
              </div>
            </div>

            <div className="max-h-80 space-y-2 overflow-auto pr-1">
              {data.items.length === 0 ? (
                <div className="rounded-lg border border-dashed border-border px-4 py-8 text-center text-sm text-muted-foreground">
                  暂无套餐明细
                </div>
              ) : (
                data.items.map((item, index) => {
                  const total = Math.max(
                    item.total,
                    item.used + item.remain,
                    1,
                  );
                  const usedPercent = Math.min(
                    Math.round((item.used / total) * 100),
                    100,
                  );
                  const gotDate = item.got_at
                    ? new Date(item.got_at * 1000)
                    : null;
                  const isToday = gotDate
                    ? new Date().toDateString() === gotDate.toDateString()
                    : false;
                  return (
                    <div
                      key={`${item.name}-${index}`}
                      className="rounded-lg border border-border/60 bg-muted/25 px-3 py-2.5"
                    >
                      <div className="flex items-center justify-between gap-2">
                        <p className="flex min-w-0 items-center gap-1.5">
                          <span className="truncate text-[13px] font-medium">
                            {item.name}
                          </span>
                          {isToday && (
                            <span className="shrink-0 rounded-full bg-emerald-500/15 px-1.5 py-0.5 text-[10px] font-semibold text-emerald-600 dark:text-emerald-400">
                              今天 +
                            </span>
                          )}
                        </p>
                        <span className="shrink-0 font-mono text-xs font-semibold text-primary">
                          剩余 {item.remain.toLocaleString()}
                        </span>
                      </div>
                      <div className="mt-2 h-1.5 overflow-hidden rounded-full bg-muted">
                        <div
                          className="h-full rounded-full bg-primary/70"
                          style={{ width: `${usedPercent}%` }}
                        />
                      </div>
                      <div className="mt-1 flex items-center justify-between font-mono text-[11px] text-muted-foreground">
                        <span>
                          {gotDate
                            ? `获得 ${gotDate.toLocaleDateString("zh-CN", { month: "2-digit", day: "2-digit" })} ${gotDate.toLocaleTimeString("zh-CN", { hour: "2-digit", minute: "2-digit" })}`
                            : `已用 ${item.used.toLocaleString()}`}
                        </span>
                        <span>
                          {item.expire_at
                            ? `到期 ${item.expire_at.slice(5, 16)}`
                            : `总量 ${item.total.toLocaleString()}`}
                        </span>
                      </div>
                    </div>
                  );
                })
              )}
            </div>
          </div>
        ) : null}
      </DialogContent>
    </Dialog>
  );
}

export default function AccountsPage() {
  const [accounts, setAccounts] = useState<AppState["accounts"]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [busyAccount, setBusyAccount] = useState<string | null>(null);
  const [batchBusy, setBatchBusy] = useState<
    "checkin" | "refresh" | "ban" | null
  >(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [actionSuccess, setActionSuccess] = useState<string | null>(null);
  const [detailView, setDetailView] = useState<DetailView | null>(null);
  const [page, setPage] = useState(1);

  const load = useCallback(async () => {
    try {
      const st = await api.getState();
      setAccounts(st.accounts);
      setError(null);
    } catch (err: unknown) {
      const msg = err instanceof Error ? err.message : "加载账号池失败";
      setError(msg);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    load();
    const timer = setInterval(load, 5000);
    return () => clearInterval(timer);
  }, [load]);

  const runAction = async (
    uid: string,
    fn: () => Promise<{ msg?: string }>,
    okMsg: string,
  ) => {
    setBusyAccount(uid);
    setActionError(null);
    setActionSuccess(null);
    try {
      const r = await fn();
      setActionSuccess(r.msg || okMsg);
      await load();
    } catch (err: unknown) {
      const msg = err instanceof Error ? err.message : "操作失败";
      setActionError(msg);
    } finally {
      setBusyAccount(null);
    }
  };

  const runBatch = async (kind: "checkin" | "refresh" | "ban") => {
    setBatchBusy(kind);
    setActionError(null);
    setActionSuccess(null);
    try {
      if (kind === "checkin") {
        const r: CheckinAllResult = await api.accountCheckinAll();
        const total = r.results?.length ?? 0;
        const ok = r.results?.filter((x) => x.ok).length ?? 0;
        setActionSuccess(
          `批量签到完成：${ok}/${total} 成功${ok < total ? `，${total - ok} 个失败` : ""}`,
        );
      } else if (kind === "refresh") {
        const r: RefreshAllResult = await api.accountRefreshAll();
        setActionSuccess(
          `批量刷新完成：${r.ok}/${r.total} 成功${r.failed > 0 ? `，${r.failed} 个失败` : ""}`,
        );
      } else {
        const r: BanCheckAllResult = await api.accountBanCheckAll();
        const bits = [
          `检测完成：${r.ok}/${r.total} 正常`,
        ];
        if (r.banned) bits.push(`${r.banned} 个封禁/拉黑（已停用）`);
        if (r.session_dead) bits.push(`${r.session_dead} 个登录失效`);
        if (r.failed) bits.push(`${r.failed} 个探测失败`);
        setActionSuccess(bits.join("，"));
      }
      await load();
    } catch (err: unknown) {
      const msg = err instanceof Error ? err.message : "批量操作失败";
      setActionError(msg);
    } finally {
      setBatchBusy(null);
    }
  };

  const handleDelete = async (uid: string, name: string) => {
    if (!window.confirm(`确定删除账号 ${name}？该操作不可恢复。`)) return;
    await runAction(uid, () => api.accountRemove(uid), "已删除");
  };

  const total = accounts.length;
  const available = accounts.filter((a) => !a.disabled && !a.cooling).length;
  const cooling = accounts.filter((a) => a.cooling).length;
  const disabled = accounts.filter((a) => a.disabled).length;
  const credits = accounts.reduce((sum, a) => sum + a.credits, 0);

  const pageCount = Math.max(Math.ceil(total / ACCOUNTS_PAGE_SIZE), 1);
  const current = Math.min(Math.max(page, 1), pageCount);
  const startOffset = (current - 1) * ACCOUNTS_PAGE_SIZE;
  const paginated = accounts.slice(
    startOffset,
    startOffset + ACCOUNTS_PAGE_SIZE,
  );
  const startIndex = total === 0 ? 0 : startOffset + 1;
  const endIndex = Math.min(startOffset + ACCOUNTS_PAGE_SIZE, total);

  useEffect(() => {
    setPage((p) => Math.min(Math.max(p, 1), pageCount));
  }, [pageCount]);

  return (
    <div className="mx-auto w-full max-w-[1320px] space-y-5">
      {error && (
        <Alert variant="destructive">
          <AlertDescription>{error}</AlertDescription>
        </Alert>
      )}

      <Card className="border-border/60 shadow-sm">
        <CardHeader className="pb-3">
          <div className="flex flex-col gap-3 lg:flex-row lg:items-center lg:justify-between">
            <CardTitle className="text-sm font-medium">账号池</CardTitle>
            <div className="grid grid-cols-2 gap-2 sm:flex sm:flex-wrap">
              <Button
                variant="outline"
                size="sm"
                disabled={batchBusy !== null || total === 0}
                onClick={() => void runBatch("checkin")}
                className="h-10 w-full sm:h-7 sm:w-auto"
              >
                {batchBusy === "checkin" ? (
                  <LoadingSpinner size={14} className="mr-1.5" />
                ) : (
                  <CalendarCheck2 className="mr-1.5 h-3.5 w-3.5" />
                )}
                批量签到
              </Button>
              <Button
                variant="outline"
                size="sm"
                disabled={batchBusy !== null || total === 0}
                onClick={() => void runBatch("refresh")}
                className="h-10 w-full sm:h-7 sm:w-auto"
              >
                {batchBusy === "refresh" ? (
                  <LoadingSpinner size={14} className="mr-1.5" />
                ) : (
                  <RefreshCw className="mr-1.5 h-3.5 w-3.5" />
                )}
                批量刷新
              </Button>
              <Button
                variant="outline"
                size="sm"
                disabled={batchBusy !== null || total === 0}
                onClick={() => void runBatch("ban")}
                className="h-10 w-full sm:h-7 sm:w-auto"
              >
                {batchBusy === "ban" ? (
                  <LoadingSpinner size={14} className="mr-1.5" />
                ) : (
                  <ShieldAlert className="mr-1.5 h-3.5 w-3.5" />
                )}
                检测封禁
              </Button>
              <AddAccountDialog onDone={load} />
              <ImportDialog onDone={load} />
            </div>
          </div>
        </CardHeader>
        <CardContent className="space-y-4">
          {loading ? (
            <div className="space-y-2">
              {Array.from({ length: 3 }).map((_, index) => (
                <Skeleton key={index} className="h-28 w-full" />
              ))}
            </div>
          ) : (
            <>
              <div className="grid grid-cols-2 gap-2 sm:grid-cols-5">
                {metric("总账号", total)}
                {metric("可用", available)}
                {metric("冷却中", cooling)}
                {metric("已停用", disabled)}
                {metric("总积分", credits.toLocaleString())}
              </div>

              {actionSuccess && (
                <Alert>
                  <AlertDescription>{actionSuccess}</AlertDescription>
                </Alert>
              )}
              {actionError && (
                <Alert variant="destructive">
                  <AlertDescription>{actionError}</AlertDescription>
                </Alert>
              )}

              <div className="grid gap-3">
                {total === 0 ? (
                  <div className="rounded-lg border border-dashed border-border px-4 py-10 text-center text-sm text-muted-foreground">
                    暂无账号 — 点击「OAuth 登录添加」或「导入 JSON」开始
                  </div>
                ) : (
                  paginated.map((account) => (
                    <div
                      key={account.uid}
                      className="rounded-lg border border-border/60 bg-card p-4 shadow-sm"
                    >
                      <div className="flex flex-col gap-4">
                        <div className="flex flex-col gap-2 sm:flex-row sm:items-start sm:justify-between">
                          <div className="flex flex-wrap items-center gap-2">
                            <h3 className="min-w-0 truncate text-sm font-semibold">
                              {account.nickname || "未命名"}
                            </h3>
                            <Badge variant="secondary" className="text-[10px]">
                              {CHANNEL_LABEL[account.group] ?? account.group}
                            </Badge>
                            {account.disabled ? (
                              <Badge
                                variant="destructive"
                                className="text-[10px]"
                              >
                                已停用
                              </Badge>
                            ) : account.cooling ? (
                              <Badge
                                variant="outline"
                                className="text-[10px] text-warning"
                              >
                                冷却至 {account.until || "-"}
                              </Badge>
                            ) : (
                              <Badge className="text-[10px]">可用</Badge>
                            )}
                            {account.err_count > 0 && (
                              <Badge variant="outline" className="text-[10px]">
                                连续失败 {account.err_count}
                              </Badge>
                            )}
                          </div>
                        </div>

                        <div className="grid grid-cols-2 gap-2 lg:grid-cols-5">
                          <AccountDetail
                            label="积分"
                            value={account.credits.toLocaleString()}
                          />
                          <AccountDetail
                            label="UID"
                            value={account.uid.slice(0, 8)}
                            mono
                          />
                          <AccountDetail
                            label="上次签到"
                            value={account.last_checkin_at || "-"}
                          />
                          <AccountDetail
                            label="签到结果"
                            value={
                              account.last_checkin_at
                                ? account.last_checkin_ok
                                  ? "成功"
                                  : account.last_checkin_msg || "失败"
                                : "-"
                            }
                          />
                          <AccountDetail
                            label="状态说明"
                            value={
                              account.disabled
                                ? "手动停用"
                                : account.reason || "正常"
                            }
                          />
                        </div>

                        <div className="grid grid-cols-2 gap-2 border-t border-border/60 pt-3 sm:flex sm:flex-wrap sm:justify-end lg:grid-cols-5">
                          <Button
                            variant="outline"
                            size="sm"
                            onClick={() =>
                              void runAction(
                                account.uid,
                                () => api.accountCheckin(account.uid),
                                "签到成功",
                              )
                            }
                            disabled={busyAccount === account.uid}
                            className="h-10 w-full whitespace-nowrap sm:h-7 sm:w-auto"
                          >
                            {busyAccount === account.uid ? (
                              <LoadingSpinner size={14} className="mr-1.5" />
                            ) : (
                              <CalendarCheck2 className="mr-1.5 h-3.5 w-3.5" />
                            )}
                            签到
                          </Button>
                          <Button
                            variant="outline"
                            size="sm"
                            onClick={() =>
                              void runAction(
                                account.uid,
                                () => api.accountRefresh(account.uid),
                                "已刷新",
                              )
                            }
                            disabled={busyAccount === account.uid}
                            className="h-10 w-full whitespace-nowrap sm:h-7 sm:w-auto"
                          >
                            <RefreshCw className="mr-1.5 h-3.5 w-3.5" />
                            刷新积分
                          </Button>
                          <Button
                            variant="outline"
                            size="sm"
                            onClick={() =>
                              void runAction(
                                account.uid,
                                async () => {
                                  const r = await api.accountBanCheck(account.uid);
                                  return { msg: formatBanCheck(r) };
                                },
                                "检测完成",
                              )
                            }
                            disabled={busyAccount === account.uid}
                            className="h-10 w-full whitespace-nowrap sm:h-7 sm:w-auto"
                          >
                            <ShieldAlert className="mr-1.5 h-3.5 w-3.5" />
                            检测封禁
                          </Button>
                          <Button
                            variant="outline"
                            size="sm"
                            onClick={() =>
                              setDetailView({
                                account:
                                  account.nickname || account.uid.slice(0, 8),
                                channel: account.group,
                                uid: account.uid,
                              })
                            }
                            className="h-10 w-full whitespace-nowrap sm:h-7 sm:w-auto"
                          >
                            <Coins className="mr-1.5 h-3.5 w-3.5" />
                            积分明细
                          </Button>
                          <Button
                            variant="outline"
                            size="sm"
                            onClick={() =>
                              void runAction(
                                account.uid,
                                () =>
                                  api.accountDisable(
                                    account.uid,
                                    !account.disabled,
                                  ),
                                account.disabled ? "已启用" : "已停用",
                              )
                            }
                            disabled={busyAccount === account.uid}
                            className="h-10 w-full whitespace-nowrap sm:h-7 sm:w-auto"
                          >
                            {account.disabled ? (
                              <PlayCircle className="mr-1.5 h-3.5 w-3.5" />
                            ) : (
                              <PauseCircle className="mr-1.5 h-3.5 w-3.5" />
                            )}
                            {account.disabled ? "启用" : "停用"}
                          </Button>
                          <Button
                            variant="outline"
                            size="sm"
                            onClick={() =>
                              void handleDelete(
                                account.uid,
                                account.nickname || account.uid.slice(0, 8),
                              )
                            }
                            disabled={busyAccount === account.uid}
                            className="h-10 w-full whitespace-nowrap text-destructive hover:text-destructive sm:h-7 sm:w-auto"
                          >
                            <Trash2 className="mr-1.5 h-3.5 w-3.5" />
                            删除
                          </Button>
                        </div>
                      </div>
                    </div>
                  ))
                )}
              </div>

              {total > 0 && (
                <PaginationControls
                  page={current}
                  pageCount={pageCount}
                  total={total}
                  startIndex={startIndex}
                  endIndex={endIndex}
                  onPageChange={setPage}
                />
              )}
            </>
          )}
        </CardContent>
      </Card>

      <CreditDetailDialog
        view={detailView}
        onClose={() => setDetailView(null)}
      />
    </div>
  );
}
