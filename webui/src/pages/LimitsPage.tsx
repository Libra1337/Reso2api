import { useCallback, useEffect, useState } from "react";
import {
  AlertTriangle,
  CheckCircle2,
  Gauge,
  RefreshCw,
  Snowflake,
} from "lucide-react";
import { api } from "@/lib/api-client";
import type { LimitsResult } from "@/types";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { cn } from "@/lib/utils";

const fmtUntil = (iso: string) => {
  const d = new Date(iso);
  const diff = d.getTime() - Date.now();
  if (diff <= 0) return "已重置";
  const h = Math.floor(diff / 3_600_000);
  const m = Math.floor((diff % 3_600_000) / 60_000);
  const time = d.toLocaleTimeString("zh-CN", {
    hour: "2-digit",
    minute: "2-digit",
  });
  return h > 0 ? `${h}小时${m}分后（${time}）` : `${m}分钟后（${time}）`;
};

export default function LimitsPage() {
  const [data, setData] = useState<LimitsResult | null>(null);
  const [loading, setLoading] = useState(true);
  const [refreshing, setRefreshing] = useState(false);

  const load = useCallback(async () => {
    setRefreshing(true);
    try {
      setData(await api.limits());
    } catch {
      /* keep old data */
    } finally {
      setLoading(false);
      setRefreshing(false);
    }
  }, []);

  useEffect(() => {
    void load();
    const id = setInterval(() => void load(), 30_000);
    return () => clearInterval(id);
  }, [load]);

  const models = data?.models ?? [];
  const cooling = data?.account_cooling ?? [];
  const limitedModels = models.filter((m) => m.cooled.length > 0);
  const worst = limitedModels[0];

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div>
          <h2 className="flex items-center gap-2 text-lg font-semibold">
            <Gauge className="size-5 text-sky-500" />
            模型限流
          </h2>
          <p className="text-xs text-muted-foreground">
            每日用量限额（6004）按 <b>账号 × 模型</b> 独立计算 · 30 秒自动刷新
          </p>
          <p className="mt-0.5 text-[11px] text-muted-foreground">
            某账号的某模型被限 ≠
            模型不可用：该账号的其他模型、其他账号的同款模型都不受影响
          </p>
        </div>
        <Button
          variant="outline"
          size="sm"
          onClick={() => void load()}
          disabled={refreshing}
        >
          <RefreshCw
            className={cn("mr-1.5 size-3.5", refreshing && "animate-spin")}
          />
          刷新
        </Button>
      </div>

      {loading ? (
        <Skeleton className="h-20 w-full" />
      ) : limitedModels.length === 0 && cooling.length === 0 ? (
        <div className="flex items-center gap-2 rounded-md border border-emerald-200 bg-emerald-50 px-3 py-3 text-sm text-emerald-700 dark:border-emerald-900 dark:bg-emerald-950/40 dark:text-emerald-400">
          <CheckCircle2 className="size-4" />
          全部模型与账号状态良好，无限流无冷却
        </div>
      ) : (
        <div className="grid grid-cols-2 gap-3 md:grid-cols-4">
          <Card>
            <CardContent className="pt-4">
              <div className="text-xl font-bold tabular-nums">
                {limitedModels.length}
              </div>
              <div className="text-[11px] text-muted-foreground">
                被限模型数
              </div>
            </CardContent>
          </Card>
          <Card>
            <CardContent className="pt-4">
              <div className="text-xl font-bold tabular-nums text-amber-600 dark:text-amber-400">
                {models.reduce((s, m) => s + m.cooled.length, 0)}
              </div>
              <div className="text-[11px] text-muted-foreground">
                模型级被限账号
              </div>
            </CardContent>
          </Card>
          <Card>
            <CardContent className="pt-4">
              <div className="text-xl font-bold tabular-nums text-sky-600 dark:text-sky-400">
                {cooling.length}
              </div>
              <div className="text-[11px] text-muted-foreground">
                整号冷却中
              </div>
            </CardContent>
          </Card>
          <Card>
            <CardContent className="pt-4">
              <div className="truncate text-sm font-medium">
                {worst ? worst.model : "-"}
              </div>
              <div className="text-[11px] text-muted-foreground">
                {worst
                  ? `最紧张：${worst.available}/${worst.total} 可用`
                  : "无紧张模型"}
              </div>
            </CardContent>
          </Card>
        </div>
      )}

      <Card>
        <CardHeader className="pb-2">
          <CardTitle className="flex items-center gap-1.5 text-sm font-medium">
            <AlertTriangle className="size-4 text-amber-500" />
            模型每日限额（6004）
            <span className="ml-1 text-[11px] font-normal text-muted-foreground">
              账号 ×
              模型的用量墙，按上游重置时间自动恢复；同账号其他模型不受影响
            </span>
          </CardTitle>
        </CardHeader>
        <CardContent>
          {loading ? (
            <Skeleton className="h-24 w-full" />
          ) : limitedModels.length === 0 ? (
            <p className="py-6 text-center text-xs text-muted-foreground">
              当前没有模型触达每日限额
            </p>
          ) : (
            <div className="space-y-3">
              {limitedModels.map((m) => (
                <div
                  key={m.model}
                  className="rounded-lg border border-border/60 bg-muted/25 p-3"
                >
                  <div className="flex items-center justify-between gap-2">
                    <span className="min-w-0 truncate font-mono text-sm font-semibold">
                      {m.model}
                    </span>
                    <span
                      className={cn(
                        "shrink-0 rounded-full px-2 py-0.5 text-[11px] font-medium",
                        m.available === 0
                          ? "bg-red-500/15 text-red-600 dark:text-red-400"
                          : "bg-amber-500/15 text-amber-600 dark:text-amber-400",
                      )}
                    >
                      {m.available === 0
                        ? "全部账号被限"
                        : `${m.available}/${m.total} 账号可用`}
                    </span>
                  </div>
                  <div className="mt-1.5 h-1.5 overflow-hidden rounded-full bg-muted">
                    <div
                      className="h-full rounded-full bg-amber-500/70"
                      style={{
                        width: `${Math.min(100, (m.cooled.length / Math.max(m.total, 1)) * 100)}%`,
                      }}
                    />
                  </div>
                  <div className="mt-2 flex flex-wrap gap-1.5">
                    {m.cooled.map((c) => (
                      <span
                        key={c.uid}
                        className="rounded-md border border-amber-500/30 bg-amber-500/10 px-1.5 py-0.5 text-[10px] text-amber-700 dark:text-amber-400"
                        title={`${c.nickname || ""} UID ${c.uid} — 仅此账号的此模型被限，该账号其他模型不受影响`}
                      >
                        {c.nickname || c.uid.slice(0, 8)} · {fmtUntil(c.until)}
                      </span>
                    ))}
                  </div>
                </div>
              ))}
            </div>
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader className="pb-2">
          <CardTitle className="flex items-center gap-1.5 text-sm font-medium">
            <Snowflake className="size-4 text-sky-500" />
            账号级冷却
            <span className="ml-1 text-[11px] font-normal text-muted-foreground">
              429 限流 / 欠费 / 连续错误导致的整号冷却，到期自动恢复
            </span>
          </CardTitle>
        </CardHeader>
        <CardContent>
          {loading ? (
            <Skeleton className="h-16 w-full" />
          ) : cooling.length === 0 ? (
            <p className="py-4 text-center text-xs text-muted-foreground">
              无冷却中的账号
            </p>
          ) : (
            <div className="space-y-1">
              {cooling.map((c) => (
                <div
                  key={c.uid}
                  className="flex items-center justify-between gap-2 rounded-md bg-muted/40 px-2.5 py-1.5 text-xs"
                >
                  <span className="min-w-0 truncate">
                    <span className="font-medium">
                      {c.nickname || c.uid.slice(0, 8)}
                    </span>
                    <span className="ml-2 text-muted-foreground">
                      {c.reason}
                    </span>
                  </span>
                  <span className="shrink-0 tabular-nums text-muted-foreground">
                    {fmtUntil(c.until)}
                  </span>
                </div>
              ))}
            </div>
          )}
        </CardContent>
      </Card>
    </div>
  );
}
