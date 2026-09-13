import { useCallback, useEffect, useState } from "react";
import {
  GraduationCap,
  Layers,
  LoaderCircle,
  PlayCircle,
  RefreshCw,
  Sparkles,
} from "lucide-react";
import { api } from "@/lib/api-client";
import type { SchoolListResult, SchoolTask } from "@/types";
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

const modeLabel: Record<string, string> = {
  share: "分享点亮",
  report: "上报点亮",
  manual: "需人工",
  unknown: "未知，跳过",
};

export default function SchoolPage() {
  const [accounts, setAccounts] = useState<AccountLite[]>([]);
  const [uid, setUid] = useState("");
  const [data, setData] = useState<SchoolListResult | null>(null);
  const [loading, setLoading] = useState(false);
  const [busy, setBusy] = useState(false);
  const [batchBusy, setBatchBusy] = useState(false);
  const [notice, setNotice] = useState<string | null>(null);

  useEffect(() => {
    api
      .getState()
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
      setData(await api.schoolList(target));
    } catch (e) {
      setNotice(e instanceof Error ? e.message : "加载失败");
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void load(uid);
  }, [uid, load]);

  const runOne = async () => {
    setBusy(true);
    setNotice(null);
    try {
      const r = await api.schoolRun(uid);
      setNotice(r.message || "完成");
      await load(uid);
    } catch (e) {
      setNotice(e instanceof Error ? e.message : "执行失败");
    } finally {
      setBusy(false);
    }
  };

  const runBatch = async () => {
    setBatchBusy(true);
    setNotice(null);
    try {
      await api.schoolRunAll();
      setNotice(
        "开学季批量已启动（串行防风控，不含抽奖、不含学生认证）。进度见「猫猫乐园 → 任务动态」。",
      );
    } catch (e) {
      setNotice(e instanceof Error ? e.message : "启动失败");
    } finally {
      setBatchBusy(false);
    }
  };

  const tasks = data?.tasks ?? [];
  const autoN = tasks.filter(
    (t) => t.mode === "share" || t.mode === "report",
  ).length;
  const doneN = tasks.filter((t) =>
    ["completed", "claimed"].includes((t.status || "").toLowerCase()),
  ).length;

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div>
          <h2 className="flex items-center gap-2 text-lg font-semibold">
            <GraduationCap className="size-5 text-sky-500" />
            开学季活动
          </h2>
          <p className="text-xs text-muted-foreground">
            独立活动页，不并入成长任务。学生认证需人工；抽奖请到「抽奖」页。
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
            variant="secondary"
            size="sm"
            disabled={
              batchBusy ||
              busy ||
              accounts.filter((x) => !x.disabled).length === 0
            }
            onClick={() => void runBatch()}
          >
            {batchBusy ? (
              <LoaderCircle className="mr-1.5 size-3.5 animate-spin" />
            ) : (
              <Layers className="mr-1.5 size-3.5" />
            )}
            全部账号执行
          </Button>
          <Button size="sm" disabled={busy || batchBusy || !uid} onClick={() => void runOne()}>
            {busy ? (
              <LoaderCircle className="mr-1.5 size-3.5 animate-spin" />
            ) : (
              <PlayCircle className="mr-1.5 size-3.5" />
            )}
            本账号执行
          </Button>
          <Button
            variant="outline"
            size="sm"
            onClick={() => void load(uid)}
            disabled={loading || !uid}
          >
            <RefreshCw className={cn("mr-1.5 size-3.5", loading && "animate-spin")} />
            刷新
          </Button>
        </div>
      </div>

      {notice && (
        <div className="flex items-start gap-2 rounded-md border border-sky-200 bg-sky-50 px-3 py-2 text-xs text-sky-700 dark:border-sky-900 dark:bg-sky-950/40 dark:text-sky-300">
          <Sparkles className="mt-0.5 size-3.5 shrink-0" />
          {notice}
        </div>
      )}

      <div className="grid grid-cols-3 gap-3">
        <Card>
          <CardContent className="pt-4">
            <div className="text-xl font-bold tabular-nums">
              {data?.in_period ? "进行中" : data ? "未开启" : "…"}
            </div>
            <div className="text-[11px] text-muted-foreground">活动状态</div>
          </CardContent>
        </Card>
        <Card>
          <CardContent className="pt-4">
            <div className="text-xl font-bold tabular-nums">
              {doneN}/{tasks.length || "…"}
            </div>
            <div className="text-[11px] text-muted-foreground">已完成 / 总任务</div>
          </CardContent>
        </Card>
        <Card>
          <CardContent className="pt-4">
            <div className="text-xl font-bold tabular-nums text-sky-600 dark:text-sky-400">
              {autoN}
            </div>
            <div className="text-[11px] text-muted-foreground">可自动点亮</div>
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
              {tasks.map((t) => (
                <SchoolTaskRow key={t.task_code} task={t} />
              ))}
              {tasks.length === 0 && (
                <p className="py-8 text-center text-xs text-muted-foreground">
                  选择账号后加载开学季任务；活动未开启时列表为空
                </p>
              )}
            </div>
          </CardContent>
        </Card>
      )}

      <p className="text-[11px] text-muted-foreground">
        可自动：分享、小程序对话 3 次、桌面对话 1 次、开学季专家。学生认证跳过。本页执行不会抽奖。
      </p>
    </div>
  );
}

function SchoolTaskRow({ task }: { task: SchoolTask }) {
  const status = (task.status || "").toLowerCase();
  const done = status === "completed" || status === "claimed";
  const pct =
    task.target_count > 0
      ? Math.min(100, (task.progress / task.target_count) * 100)
      : done
        ? 100
        : 0;
  return (
    <div className="flex items-center gap-3 px-4 py-2.5">
      <div className="min-w-0 flex-1">
        <div className="flex flex-wrap items-baseline gap-x-2">
          <span className="truncate text-sm font-medium">
            {task.title || task.task_code}
          </span>
          {!!task.reward_credit && (
            <span className="text-[11px] font-medium text-amber-600 dark:text-amber-400">
              +{task.reward_credit}
            </span>
          )}
          <span className="text-[11px] text-muted-foreground">
            {modeLabel[task.mode || ""] || task.mode}
          </span>
        </div>
        <div className="mt-1 h-1 max-w-56 overflow-hidden rounded-full bg-muted">
          <div
            className={cn(
              "h-full rounded-full",
              done ? "bg-muted-foreground/40" : "bg-sky-500/70",
            )}
            style={{ width: `${pct}%` }}
          />
        </div>
        <div className="mt-0.5 flex gap-2 text-[11px] text-muted-foreground">
          <span className="font-mono">
            {task.target_count > 0
              ? `${task.progress}/${task.target_count}`
              : task.task_code}
          </span>
          <span className="truncate">{task.note || task.description}</span>
        </div>
      </div>
      <span className="shrink-0 text-[11px] text-muted-foreground">{status || "-"}</span>
    </div>
  );
}
