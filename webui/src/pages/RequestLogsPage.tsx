import { useMemo, useState, type ReactNode } from "react";
import {
  ArrowDownToLine,
  ArrowUpFromLine,
  Coins,
  RefreshCw,
  Search,
  Zap,
} from "lucide-react";

import { usePolling } from "@/hooks/use-polling";
import { api } from "@/lib/api-client";
import type { ReqLog } from "@/types";
import { Card, CardContent } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Skeleton } from "@/components/ui/skeleton";
import { cn } from "@/lib/utils";

const CHANNEL_LABEL: Record<string, string> = {
  workbuddy: "WorkBuddy",
  traework: "TraeWork",
  qoder: "Qoder",
};

function fmtMs(ms: number): string {
  if (ms <= 0) return "-";
  if (ms < 1000) return `${ms}ms`;
  return `${(ms / 1000).toFixed(1)}s`;
}

function fmtTok(n: number): string {
  if (n === 0) return "0";
  if (n < 1000) return String(n);
  if (n < 1000000) return `${(n / 1000).toFixed(n < 10000 ? 1 : 0)}k`;
  return `${(n / 1000000).toFixed(1)}M`;
}

function cachePercent(l: ReqLog): string | null {
  if (l.cached_tokens <= 0 || l.in_tokens <= 0) return null;
  return `${Math.round((l.cached_tokens / l.in_tokens) * 100)}%`;
}

function DurationBadge({ ms }: { ms: number }) {
  const tone =
    ms > 30000
      ? "bg-destructive/10 text-destructive"
      : ms > 10000
        ? "bg-warning-muted text-warning"
        : "bg-secondary text-secondary-foreground";
  return (
    <span
      className={cn(
        "inline-flex h-5 items-center rounded-md px-1.5 font-mono text-[10px] tabular-nums",
        tone,
      )}
    >
      {fmtMs(ms)}
    </span>
  );
}

function TtfbBadge({ ms }: { ms: number }) {
  if (ms <= 0) return null;
  const tone =
    ms > 5000 ? "bg-warning-muted text-warning" : "bg-info-muted text-info";
  return (
    <span
      className={cn(
        "inline-flex h-5 items-center rounded-md px-1.5 font-mono text-[10px] tabular-nums",
        tone,
      )}
    >
      {fmtMs(ms)}
    </span>
  );
}

function StreamBadge({ stream }: { stream: boolean }) {
  return (
    <span
      className={cn(
        "inline-flex h-5 items-center rounded-md px-1.5 text-[10px]",
        stream ? "bg-info-muted text-info" : "bg-warning-muted text-warning",
      )}
    >
      {stream ? "流式" : "聚合"}
    </span>
  );
}

function StatChip({
  icon,
  label,
  value,
}: {
  icon: ReactNode;
  label: string;
  value: string;
}) {
  return (
    <div className="flex items-center gap-1.5 rounded-md border border-border/60 bg-muted/25 px-2 py-1">
      <span className="text-muted-foreground">{icon}</span>
      <span className="text-[10px] text-muted-foreground">{label}</span>
      <span className="font-mono text-xs font-semibold tabular-nums">
        {value}
      </span>
    </div>
  );
}

export default function RequestLogsPage() {
  const [autoUpdate, setAutoUpdate] = useState(true);
  const [search, setSearch] = useState("");
  const [channel, setChannel] = useState("");
  const [statusFilter, setStatusFilter] = useState("");
  const [page, setPage] = useState(0);
  const [detail, setDetail] = useState<ReqLog | null>(null);
  const [bodyText, setBodyText] = useState<string | null>(null);
  const [bodyLoading, setBodyLoading] = useState(false);

  const { data, loading, refresh } = usePolling<{
    logs: ReqLog[];
    total?: number;
  }>(() => api.reqLogPage(page, 100), 5000, autoUpdate);
  const total = data?.total ?? 0;
  const pageCount = Math.max(Math.ceil(total / 100), 1);

  const openDetail = async (l: ReqLog) => {
    setDetail(l);
    setBodyText(null);
    if (l.body_file) {
      setBodyLoading(true);
      try {
        setBodyText(await api.reqLogBody(l.body_file));
      } catch {
        setBodyText("（存档加载失败）");
      } finally {
        setBodyLoading(false);
      }
    }
  };

  const logs = useMemo(() => {
    let list = data?.logs ?? [];
    const q = search.trim().toLowerCase();
    if (q) {
      list = list.filter(
        (l) =>
          l.model.toLowerCase().includes(q) || l.uid.toLowerCase().includes(q),
      );
    }
    if (channel) list = list.filter((l) => l.channel === channel);
    if (statusFilter === "ok") list = list.filter((l) => l.status < 400);
    if (statusFilter === "err") list = list.filter((l) => l.status >= 400);
    return list;
  }, [data, search, channel, statusFilter]);

  const channels = useMemo(() => {
    const set = new Set((data?.logs ?? []).map((l) => l.channel));
    return Array.from(set);
  }, [data]);

  const totals = useMemo(
    () =>
      logs.reduce(
        (acc, l) => ({
          in: acc.in + l.in_tokens,
          out: acc.out + l.out_tokens,
          cache: acc.cache + l.cached_tokens,
          credit: acc.credit + l.credit,
        }),
        { in: 0, out: 0, cache: 0, credit: 0 },
      ),
    [logs],
  );

  return (
    <div className="mx-auto w-full max-w-[1320px] space-y-4">
      {/* 筛选栏 */}
      <div className="flex flex-wrap items-center gap-2">
        <div className="relative min-w-52 flex-1">
          <Search className="absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground" />
          <Input
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            placeholder="搜索模型 / 账号 UID…"
            className="h-8 pl-8 text-xs"
          />
        </div>
        <select
          value={channel}
          onChange={(e) => setChannel(e.target.value)}
          className="h-8 rounded-md border border-input bg-background px-2 text-xs text-foreground"
          aria-label="渠道筛选"
        >
          <option value="">全部渠道</option>
          {channels.map((c) => (
            <option key={c} value={c}>
              {CHANNEL_LABEL[c] ?? c}
            </option>
          ))}
        </select>
        <select
          value={statusFilter}
          onChange={(e) => setStatusFilter(e.target.value)}
          className="h-8 rounded-md border border-input bg-background px-2 text-xs text-foreground"
          aria-label="状态筛选"
        >
          <option value="">全部状态</option>
          <option value="ok">成功</option>
          <option value="err">错误</option>
        </select>
        <Button
          variant={autoUpdate ? "default" : "outline"}
          size="sm"
          className="h-8 gap-1.5 text-xs"
          onClick={() => setAutoUpdate((v) => !v)}
          aria-pressed={autoUpdate}
        >
          <Zap className="size-3" />
          {autoUpdate ? "实时已开" : "实时已停"}
        </Button>
        <Button
          variant="outline"
          size="sm"
          className="h-8 text-xs"
          onClick={() => void refresh()}
        >
          <RefreshCw className="mr-1 size-3" />
          刷新
        </Button>
      </div>

      {/* 汇总 */}
      <div className="flex flex-wrap items-center gap-2">
        <span className="text-xs text-muted-foreground">
          共 <span className="font-mono font-semibold">{logs.length}</span> 条
        </span>
        <StatChip
          icon={<ArrowUpFromLine className="size-3" />}
          label="输入"
          value={fmtTok(totals.in)}
        />
        <StatChip
          icon={<ArrowDownToLine className="size-3" />}
          label="输出"
          value={fmtTok(totals.out)}
        />
        <StatChip
          icon={<Zap className="size-3" />}
          label="缓存命中"
          value={fmtTok(totals.cache)}
        />
        <StatChip
          icon={<Coins className="size-3" />}
          label="积分"
          value={totals.credit.toFixed(2)}
        />
      </div>

      {loading ? (
        <Skeleton className="h-72 w-full" />
      ) : logs.length === 0 ? (
        <Card className="border-dashed">
          <CardContent className="py-10 text-center text-sm text-muted-foreground">
            没有匹配的调用记录
          </CardContent>
        </Card>
      ) : (
        <Card className="border-border/60 shadow-sm">
          <CardContent className="p-0">
            <div className="max-h-[calc(100dvh-230px)] min-h-80 overflow-auto">
              <Table>
                <TableHeader>
                  <TableRow className="sticky top-0 z-10 bg-card hover:bg-card">
                    <TableHead>时间</TableHead>
                    <TableHead>模型</TableHead>
                    <TableHead>渠道</TableHead>
                    <TableHead>耗时 / 首字</TableHead>
                    <TableHead className="text-right">输入</TableHead>
                    <TableHead className="text-right">输出</TableHead>
                    <TableHead className="text-right">积分</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {logs.map((l, i) => {
                    const ok = l.status < 400;
                    const cp = cachePercent(l);
                    return (
                      <TableRow
                        key={i}
                        className="cursor-pointer hover:bg-muted/60"
                        onClick={() => void openDetail(l)}
                        title="点击查看详情"
                      >
                        {/* 时间 + 账号 + 状态点 */}
                        <TableCell className="whitespace-nowrap py-1 pl-3 pr-2 align-middle font-mono text-muted-foreground">
                          <span className="flex flex-col leading-4">
                            <span className="h-4 text-[11px]">{l.time}</span>
                            <span className="flex h-5 items-center gap-1 text-[10px]">
                              <span
                                className={cn(
                                  "size-1.5 rounded-full",
                                  ok ? "bg-success" : "bg-destructive",
                                )}
                                aria-label={ok ? "成功" : `错误 ${l.status}`}
                              />
                              <span>{l.uid ? l.uid.slice(0, 8) : "-"}</span>
                            </span>
                          </span>
                        </TableCell>

                        {/* 模型 + 流式徽章 */}
                        <TableCell className="max-w-56 py-1 pr-2 align-middle">
                          <span className="block max-w-full truncate font-mono text-[11px] font-medium text-foreground">
                            {l.model}
                          </span>
                          <span className="mt-0.5 flex h-5 items-center">
                            <StreamBadge stream={l.stream} />
                            {!ok && (
                              <span className="ml-1 font-mono text-[10px] text-destructive">
                                HTTP {l.status}
                              </span>
                            )}
                          </span>
                        </TableCell>

                        <TableCell className="whitespace-nowrap py-1 pr-2 align-middle text-xs">
                          {CHANNEL_LABEL[l.channel] ?? l.channel}
                        </TableCell>

                        {/* 耗时徽章组 */}
                        <TableCell className="whitespace-nowrap py-1 pr-2 align-middle">
                          <span className="inline-flex items-center gap-1">
                            <DurationBadge ms={l.total_ms} />
                            <TtfbBadge ms={l.ttfb_ms} />
                          </span>
                        </TableCell>

                        {/* 输入 + 缓存副行 */}
                        <TableCell className="whitespace-nowrap py-1 pr-2 text-right align-middle">
                          <span className="flex flex-col items-end leading-4">
                            <span className="h-4 font-mono text-sm font-medium tabular-nums text-foreground">
                              {fmtTok(l.in_tokens)}
                            </span>
                            {cp && (
                              <span className="h-4 font-mono text-[10px] tabular-nums text-success">
                                {fmtTok(l.cached_tokens)} · {cp}
                              </span>
                            )}
                          </span>
                        </TableCell>

                        <TableCell className="whitespace-nowrap py-1 pr-2 text-right align-middle">
                          <span className="h-4 font-mono text-sm font-medium tabular-nums text-foreground">
                            {fmtTok(l.out_tokens)}
                          </span>
                        </TableCell>

                        <TableCell className="whitespace-nowrap py-1 pr-3 text-right align-middle">
                          <span className="font-mono text-xs tabular-nums text-muted-foreground">
                            {l.credit > 0 ? l.credit.toFixed(2) : "0"}
                          </span>
                        </TableCell>
                      </TableRow>
                    );
                  })}
                </TableBody>
              </Table>
            </div>
          </CardContent>
        </Card>
      )}

      {/* 历史分页（永久日志 newest-first） */}
      <div className="flex items-center justify-between text-xs text-muted-foreground">
        <span>
          共 {total} 条 · 第 {page + 1}/{pageCount} 页（每页 100 条，永久保留）
        </span>
        <div className="flex gap-2">
          <Button
            variant="outline"
            size="sm"
            className="h-7"
            disabled={page === 0}
            onClick={() => {
              setPage((v) => Math.max(0, v - 1));
              void refresh();
            }}
          >
            上一页
          </Button>
          <Button
            variant="outline"
            size="sm"
            className="h-7"
            disabled={page + 1 >= pageCount}
            onClick={() => {
              setPage((v) => v + 1);
              void refresh();
            }}
          >
            下一页
          </Button>
        </div>
      </div>

      {/* 详情弹窗 */}
      <Dialog
        open={detail !== null}
        onOpenChange={(next) => !next && setDetail(null)}
      >
        <DialogContent className="sm:max-w-2xl">
          <DialogHeader>
            <DialogTitle>请求详情</DialogTitle>
            {detail && (
              <DialogDescription className="font-mono text-[11px]">
                {detail.time} · {detail.model} · HTTP {detail.status} ·{" "}
                {detail.total_ms}ms
              </DialogDescription>
            )}
          </DialogHeader>
          {detail && (
            <div className="space-y-2 text-xs">
              <div className="grid grid-cols-2 gap-2 sm:grid-cols-4">
                {[
                  { k: "输入 tokens", v: detail.in_tokens },
                  { k: "输出 tokens", v: detail.out_tokens },
                  { k: "缓存命中", v: detail.cached_tokens },
                  { k: "积分消耗", v: detail.credit },
                ].map((x) => (
                  <div key={x.k} className="rounded-md bg-muted/50 px-2 py-1.5">
                    <div className="text-[10px] text-muted-foreground">
                      {x.k}
                    </div>
                    <div className="font-mono text-sm font-semibold">{x.v}</div>
                  </div>
                ))}
              </div>
              <div>
                <div className="mb-1 text-[11px] font-medium text-muted-foreground">
                  完整请求内容{" "}
                  {detail.body_file
                    ? "(10% 采样存档，永久保留)"
                    : "（该请求未被采样存档）"}
                </div>
                <pre className="max-h-[50vh] overflow-auto whitespace-pre-wrap break-all rounded-md bg-muted/50 p-3 font-mono text-[11px] leading-relaxed">
                  {detail.body_file
                    ? bodyLoading
                      ? "加载中…"
                      : (bodyText ?? "（无）")
                    : "此请求未命中 10% 存档采样，仅有上方统计字段。"}
                </pre>
              </div>
            </div>
          )}
        </DialogContent>
      </Dialog>
    </div>
  );
}
