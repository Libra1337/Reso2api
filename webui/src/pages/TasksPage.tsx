import { useCallback, useEffect, useState } from "react";
import {
  CheckCircle2,
  Coins,
  Gift,
  ListTodo,
  LoaderCircle,
  PlayCircle,
  RefreshCw,
  Sparkles,
  Zap,
} from "lucide-react";
import { api } from "@/lib/api-client";
import type { GrowthTask, TasksListResult } from "@/types";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { cn } from "@/lib/utils";

interface AccountLite {
  uid: string;
  nickname?: string;
  group: string;
  disabled?: boolean;
}

export default function TasksPage() {
  const [accounts, setAccounts] = useState<AccountLite[]>([]);
  const [uid, setUid] = useState("");
  const [data, setData] = useState<TasksListResult | null>(null);
  const [loading, setLoading] = useState(false);
  const [busyTask, setBusyTask] = useState<string | null>(null);
  const [autoAllBusy, setAutoAllBusy] = useState(false);
  const [notice, setNotice] = useState<string | null>(null);

  useEffect(() => {
    api
      .state()
      .then((s) => {
        const wb = (s.accounts ?? []).filter(
          (a: AccountLite) => a.group === "workbuddy",
        );
        setAccounts(wb);
        if (wb.length > 0) setUid((prev) => prev || wb[0].uid);
      })
      .catch(() => {});
  }, []);

  const load = useCallback(async (target: string) => {
    if (!target) return;
    setLoading(true);
    try {
      setData(await api.tasksList(target));
    } catch {
      /* keep */
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void load(uid);
  }, [uid, load]);

  const runTask = async (code: string) => {
    setBusyTask(code);
    setNotice(null);
    try {
      const r = await api.taskAuto(uid, code);
      setNotice(r.message || r.progress_after || "完成");
      await load(uid);
    } catch (e) {
      setNotice(e instanceof Error ? e.message : "执行失败");
    } finally {
      setBusyTask(null);
    }
  };

  const runAutoAll = async () => {
    setAutoAllBusy(true);
    setNotice(null);
    try {
      await api.taskAutoAll(uid);
      setNotice(
        "一键完成已启动（约 2-4 分钟），进度见「猫猫乐园 → 任务动态」，结束后刷新本页",
      );
      setTimeout(() => void load(uid), 240_000);
    } catch (e) {
      setNotice(e instanceof Error ? e.message : "启动失败");
    } finally {
      setAutoAllBusy(false);
    }
  };

  const actionMap = new Map((data?.actions ?? []).map((x) => [x.task_code, x]));
  const tasks = (data?.tasks ?? []).slice().sort((a, b) => {
    const rank = (t: GrowthTask) =>
      t.claimed ? 4 : t.claimable ? 0 : t.current > 0 ? 1 : 2;
    return rank(a) - rank(b) || (b.credit ?? 0) - (a.credit ?? 0);
  });
  const totalCredit = tasks
    .filter((t) => !t.claimed)
    .reduce((s, t) => s + (t.credit ?? 0), 0);
  const claimedN = tasks.filter((t) => t.claimed).length;

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div>
          <h2 className="flex items-center gap-2 text-lg font-semibold">
            <ListTodo className="size-5 text-violet-500" />
            成长任务
          </h2>
          <p className="text-xs text-muted-foreground">
            17/18 项官方成长任务可纯 API 完成（新号约 +1950 积分 +78 能量）·
            进度达标自动领奖
          </p>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <select
            value={uid}
            onChange={(e) => setUid(e.target.value)}
            className="h-8 rounded-md border border-border bg-background px-2 text-sm"
          >
            {accounts.map((a) => (
              <option key={a.uid} value={a.uid}>
                {a.nickname || a.uid.slice(0, 8)}
                {a.disabled ? "（禁用）" : ""}
              </option>
            ))}
          </select>
          <Button
            size="sm"
            disabled={autoAllBusy || !uid}
            onClick={() => void runAutoAll()}
          >
            {autoAllBusy ? (
              <LoaderCircle className="mr-1.5 size-3.5 animate-spin" />
            ) : (
              <Zap className="mr-1.5 size-3.5" />
            )}
            一键完成全部
          </Button>
          <Button
            variant="outline"
            size="sm"
            onClick={() => void load(uid)}
            disabled={loading || !uid}
          >
            <RefreshCw
              className={cn("mr-1.5 size-3.5", loading && "animate-spin")}
            />
            刷新
          </Button>
        </div>
      </div>

      {notice && (
        <div className="flex items-start gap-2 rounded-md border border-violet-200 bg-violet-50 px-3 py-2 text-xs text-violet-700 dark:border-violet-900 dark:bg-violet-950/40 dark:text-violet-300">
          <Sparkles className="mt-0.5 size-3.5 shrink-0" />
          {notice}
        </div>
      )}

      <div className="grid grid-cols-3 gap-3">
        <Card>
          <CardContent className="pt-4">
            <div className="text-xl font-bold tabular-nums text-violet-600 dark:text-violet-400">
              {totalCredit}
            </div>
            <div className="text-[11px] text-muted-foreground">
              待领积分（未完成任务）
            </div>
          </CardContent>
        </Card>
        <Card>
          <CardContent className="pt-4">
            <div className="text-xl font-bold tabular-nums">
              {claimedN}/{tasks.length || "…"}
            </div>
            <div className="text-[11px] text-muted-foreground">
              已领奖 / 总任务
            </div>
          </CardContent>
        </Card>
        <Card>
          <CardContent className="pt-4">
            <div className="text-xl font-bold tabular-nums text-emerald-600 dark:text-emerald-400">
              {tasks.filter((t) => t.claimable).length}
            </div>
            <div className="text-[11px] text-muted-foreground">
              进度达标可领奖
            </div>
          </CardContent>
        </Card>
      </div>

      {loading && !data ? (
        <Skeleton className="h-64 w-full" />
      ) : (
        <Card>
          <CardHeader className="pb-2">
            <CardTitle className="text-sm font-medium">任务明细</CardTitle>
          </CardHeader>
          <CardContent className="p-0">
            <div className="divide-y divide-border/60">
              {tasks.map((t) => {
                const act = actionMap.get(t.task_code);
                const pct =
                  t.target > 0
                    ? Math.min(100, (t.current / t.target) * 100)
                    : t.claimed
                      ? 100
                      : 0;
                return (
                  <div
                    key={t.task_code}
                    className="flex items-center gap-3 px-4 py-2.5"
                  >
                    <div className="min-w-0 flex-1">
                      <div className="flex flex-wrap items-baseline gap-x-2">
                        <span className="truncate text-sm font-medium">
                          {t.title || t.task_code}
                        </span>
                        {!!t.credit && (
                          <span className="flex shrink-0 items-center gap-0.5 text-[11px] font-medium text-amber-600 dark:text-amber-400">
                            <Coins className="size-3" />+{t.credit}
                          </span>
                        )}
                        {!!t.energy && (
                          <span className="shrink-0 text-[11px] text-emerald-600 dark:text-emerald-400">
                            +{t.energy}能
                          </span>
                        )}
                        {t.claimed && (
                          <span className="flex shrink-0 items-center gap-0.5 text-[11px] text-muted-foreground">
                            <CheckCircle2 className="size-3" />
                            已领奖
                          </span>
                        )}
                      </div>
                      <div className="mt-1 h-1 max-w-56 overflow-hidden rounded-full bg-muted">
                        <div
                          className={cn(
                            "h-full rounded-full",
                            t.claimed
                              ? "bg-muted-foreground/40"
                              : "bg-violet-500/70",
                          )}
                          style={{ width: `${pct}%` }}
                        />
                      </div>
                      <div className="mt-0.5 flex gap-2 text-[11px] text-muted-foreground">
                        <span className="font-mono">
                          {t.target > 0
                            ? `${t.current}/${t.target}`
                            : t.task_code}
                        </span>
                        {act && !t.claimed && (
                          <span className="truncate">{act.desc}</span>
                        )}
                      </div>
                    </div>
                    {!t.claimed && act && (
                      <Button
                        variant={t.claimable ? "default" : "outline"}
                        size="sm"
                        className="h-7 shrink-0"
                        disabled={busyTask !== null || autoAllBusy}
                        onClick={() => void runTask(t.task_code)}
                      >
                        {busyTask === t.task_code ? (
                          <LoaderCircle className="mr-1 size-3 animate-spin" />
                        ) : t.claimable ? (
                          <Gift className="mr-1 size-3" />
                        ) : (
                          <PlayCircle className="mr-1 size-3" />
                        )}
                        {t.claimable ? "领奖" : "执行"}
                      </Button>
                    )}
                    {!t.claimed && !act && (
                      <span className="shrink-0 text-[11px] text-muted-foreground">
                        需客户端
                      </span>
                    )}
                  </div>
                );
              })}
              {tasks.length === 0 && (
                <p className="py-8 text-center text-xs text-muted-foreground">
                  选择账号后加载任务列表
                </p>
              )}
            </div>
          </CardContent>
        </Card>
      )}

      <p className="text-[11px] text-muted-foreground">
        expert_5 / skill_1 / lighthouse 等含真实对话链（fast-model
        短对话，消耗可忽略）；
        Expert_Philanthropy（真实捐款）无法自动化。批量执行进度见「猫猫乐园 →
        任务动态」。
      </p>
    </div>
  );
}
