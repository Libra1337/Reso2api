import { useCallback, useEffect, useState } from "react";
import {
  BarChart3,
  Coins,
  Cpu,
  History,
  RefreshCw,
  TriangleAlert,
} from "lucide-react";
import { api } from "@/lib/api-client";
import type { UsageAggRow, UsageStatsResult } from "@/types";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { cn } from "@/lib/utils";

const fmtNum = (n: number) => {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
  if (n >= 1_000) return `${(n / 1_000).toFixed(1)}K`;
  return `${Math.round(n)}`;
};

// 日趋势条形图（纯 CSS，无需图表库）
function DailyBars({ daily }: { daily: UsageAggRow[] }) {
  const max = Math.max(...daily.map((d) => d.credit), 0.001);
  return (
    <div className="flex h-40 items-end gap-1 overflow-x-auto pb-1">
      {daily.map((d) => (
        <div
          key={d.key}
          className="group relative flex min-w-5 flex-1 flex-col items-center gap-1"
        >
          <div
            className="w-full rounded-t bg-gradient-to-t from-sky-500/60 to-sky-400 transition-all group-hover:from-sky-500"
            style={{ height: `${Math.max(3, (d.credit / max) * 130)}px` }}
            title={`${d.key}：${d.credit.toFixed(1)} 积分 / ${d.requests} 次`}
          />
          <span className="text-[8px] text-muted-foreground">
            {d.key.slice(5)}
          </span>
        </div>
      ))}
    </div>
  );
}

function AggBars({
  rows,
  unit,
  top = 10,
}: {
  rows: UsageAggRow[];
  unit: string;
  top?: number;
}) {
  const list = rows.slice(0, top);
  const max = Math.max(...list.map((r) => r.credit), 0.001);
  if (list.length === 0)
    return (
      <p className="py-4 text-center text-xs text-muted-foreground">暂无数据</p>
    );
  return (
    <div className="space-y-1.5">
      {list.map((r) => (
        <div key={r.key} className="flex items-center gap-2 text-xs">
          <span className="w-40 truncate font-mono" title={r.key}>
            {r.key}
          </span>
          <div className="h-2.5 flex-1 overflow-hidden rounded-full bg-muted">
            <div
              className="h-full rounded-full bg-gradient-to-r from-sky-500/70 to-violet-500/70"
              style={{ width: `${(r.credit / max) * 100}%` }}
            />
          </div>
          <span className="w-24 shrink-0 text-right tabular-nums text-muted-foreground">
            {fmtNum(r.credit)} {unit} · {r.requests} 次
          </span>
        </div>
      ))}
    </div>
  );
}

export default function UsagePage() {
  const [data, setData] = useState<UsageStatsResult | null>(null);
  const [days, setDays] = useState(31);
  const [loading, setLoading] = useState(true);
  const [refreshing, setRefreshing] = useState(false);

  const load = useCallback(async (d: number, force = false) => {
    setRefreshing(true);
    try {
      setData(await api.usageStats(d, force));
    } catch {
      /* keep */
    } finally {
      setLoading(false);
      setRefreshing(false);
    }
  }, []);

  useEffect(() => {
    void load(days, true);
  }, [days, load]);

  const totalCredit = (data?.daily ?? []).reduce((s, d) => s + d.credit, 0);
  const totalReqs = (data?.daily ?? []).reduce((s, d) => s + d.requests, 0);
  const totalTokens = (data?.tokens ?? []).reduce((s, t) => s + t.credit, 0);

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div>
          <h2 className="flex items-center gap-2 text-lg font-semibold">
            <BarChart3 className="size-5 text-sky-500" />
            用量统计
          </h2>
          <p className="text-xs text-muted-foreground">
            积分消耗来自 WorkBuddy 官方请求用量接口 · token
            统计来自网关请求日志（最近 1000 条）· 10 分钟缓存
          </p>
        </div>
        <div className="flex items-center gap-2">
          <div className="flex rounded-md border border-border p-0.5">
            {[7, 31].map((d) => (
              <button
                key={d}
                onClick={() => setDays(d)}
                className={cn(
                  "rounded px-2.5 py-1 text-xs",
                  days === d
                    ? "bg-primary text-primary-foreground"
                    : "text-muted-foreground",
                )}
              >
                {d} 天
              </button>
            ))}
          </div>
          <Button
            variant="outline"
            size="sm"
            onClick={() => void load(days, true)}
            disabled={refreshing}
          >
            <RefreshCw
              className={cn("mr-1.5 size-3.5", refreshing && "animate-spin")}
            />
            刷新
          </Button>
        </div>
      </div>

      {!!data?.expiring?.length && (
        <div className="rounded-md border border-amber-300/60 bg-amber-50/60 px-3 py-2.5 dark:border-amber-700/40 dark:bg-amber-950/30">
          <p className="flex items-center gap-1.5 text-xs font-semibold text-amber-700 dark:text-amber-400">
            <TriangleAlert className="size-3.5" />
            积分即将过期（7 天内，过期清零）
          </p>
          <div className="mt-1.5 flex flex-wrap gap-1.5">
            {data.expiring.map((e, i) => (
              <span
                key={i}
                className="rounded-md bg-amber-500/10 px-2 py-0.5 text-[11px] text-amber-800 dark:text-amber-300"
              >
                {e.nickname}：{e.name} 剩 {e.remain} ·{" "}
                {e.expire_at.slice(5, 10)} 到期
              </span>
            ))}
          </div>
        </div>
      )}

      <div className="grid grid-cols-3 gap-3">
        <Card>
          <CardContent className="pt-4">
            <div className="flex items-baseline gap-1 text-xl font-bold tabular-nums text-sky-600 dark:text-sky-400">
              <Coins className="size-4 self-center" />
              {fmtNum(totalCredit)}
            </div>
            <div className="text-[11px] text-muted-foreground">
              积分消耗（{days} 天）
            </div>
          </CardContent>
        </Card>
        <Card>
          <CardContent className="pt-4">
            <div className="text-xl font-bold tabular-nums">{totalReqs}</div>
            <div className="text-[11px] text-muted-foreground">
              API 请求次数
            </div>
          </CardContent>
        </Card>
        <Card>
          <CardContent className="pt-4">
            <div className="flex items-baseline gap-1 text-xl font-bold tabular-nums text-violet-600 dark:text-violet-400">
              <Cpu className="size-4 self-center" />
              {fmtNum(totalTokens)}
            </div>
            <div className="text-[11px] text-muted-foreground">
              token 消耗（日志窗口）
            </div>
          </CardContent>
        </Card>
      </div>

      {loading ? (
        <Skeleton className="h-48 w-full" />
      ) : (
        <>
          <Card>
            <CardHeader className="pb-2">
              <CardTitle className="flex items-center gap-1.5 text-sm font-medium">
                <History className="size-4 text-sky-500" />
                每日积分消耗趋势
              </CardTitle>
            </CardHeader>
            <CardContent>
              {data?.daily?.length ? (
                <DailyBars daily={data.daily} />
              ) : (
                <p className="py-4 text-center text-xs text-muted-foreground">
                  所选时间范围内无请求
                </p>
              )}
            </CardContent>
          </Card>

          <div className="grid grid-cols-1 gap-3 lg:grid-cols-2">
            <Card>
              <CardHeader className="pb-2">
                <CardTitle className="text-sm font-medium">
                  模型消耗排行（积分）
                </CardTitle>
              </CardHeader>
              <CardContent>
                <AggBars rows={data?.models ?? []} unit="分" />
              </CardContent>
            </Card>
            <Card>
              <CardHeader className="pb-2">
                <CardTitle className="text-sm font-medium">
                  账号消耗排行（积分）
                </CardTitle>
              </CardHeader>
              <CardContent>
                <AggBars rows={data?.accounts ?? []} unit="分" />
              </CardContent>
            </Card>
          </div>

          <Card>
            <CardHeader className="pb-2">
              <CardTitle className="text-sm font-medium">
                Token 消耗（按模型，网关日志）
              </CardTitle>
            </CardHeader>
            <CardContent>
              <AggBars rows={data?.tokens ?? []} unit="tok" />
            </CardContent>
          </Card>

          {!!data?.errors?.length && (
            <p className="text-[11px] text-muted-foreground">
              部分账号拉取失败：{data.errors.length} 个（如{" "}
              {data.errors[0].slice(0, 80)}…）
            </p>
          )}
        </>
      )}
    </div>
  );
}
