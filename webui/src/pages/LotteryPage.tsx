import { useCallback, useEffect, useState } from "react";
import {
  Dices,
  GraduationCap,
  Layers,
  LoaderCircle,
  RefreshCw,
  Sparkles,
  Trophy,
} from "lucide-react";
import { api } from "@/lib/api-client";
import type { LotteryStatusAllResult, LotteryStatusResult } from "@/types";
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

export default function LotteryPage() {
  const [accounts, setAccounts] = useState<AccountLite[]>([]);
  const [uid, setUid] = useState("");
  const [all, setAll] = useState<LotteryStatusResult[]>([]);
  const [one, setOne] = useState<LotteryStatusResult | null>(null);
  const [loading, setLoading] = useState(false);
  const [busy, setBusy] = useState<string | null>(null);
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
    setLoading(true);
    try {
      const overview = (await api.lotteryStatus()) as LotteryStatusAllResult;
      setAll(overview.accounts ?? []);
      if (target) {
        setOne((await api.lotteryStatus(target)) as LotteryStatusResult);
      }
    } catch (e) {
      setNotice(e instanceof Error ? e.message : "加载失败");
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void load(uid);
  }, [uid, load]);

  const draw = async (kind: "streak" | "school" | "all", allAccounts = false) => {
    setBusy(kind + (allAccounts ? "-all" : ""));
    setNotice(null);
    try {
      if (allAccounts) {
        await api.lotteryDrawAll(kind);
        setNotice(
          "批量抽奖已启动（串行防风控）。进度见「猫猫乐园 → 任务动态」，结束后刷新本页。",
        );
      } else {
        const r = await api.lotteryDraw(uid, kind);
        setNotice(r.message || "抽奖完成");
        await load(uid);
      }
    } catch (e) {
      setNotice(e instanceof Error ? e.message : "抽奖失败");
    } finally {
      setBusy(null);
    }
  };

  const current = one ?? all.find((x) => x.uid === uid);
  const streakN = all.reduce((s, a) => s + (a.streak?.chances ?? 0), 0);
  const schoolN = all.reduce((s, a) => s + (a.school?.chance?.balance ?? 0), 0);

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div>
          <h2 className="flex items-center gap-2 text-lg font-semibold">
            <Dices className="size-5 text-amber-500" />
            抽奖
          </h2>
          <p className="text-xs text-muted-foreground">
            独立页面。连登抽奖来自成长中心兑换次数；开学季转盘来自活动领奖次数。两者分开抽，互不自动串联。
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
            variant="outline"
            size="sm"
            onClick={() => void load(uid)}
            disabled={loading || busy !== null}
          >
            <RefreshCw className={cn("mr-1.5 size-3.5", loading && "animate-spin")} />
            刷新
          </Button>
        </div>
      </div>

      {notice && (
        <div className="flex items-start gap-2 rounded-md border border-amber-200 bg-amber-50 px-3 py-2 text-xs text-amber-800 dark:border-amber-900 dark:bg-amber-950/40 dark:text-amber-300">
          <Sparkles className="mt-0.5 size-3.5 shrink-0" />
          {notice}
        </div>
      )}

      <div className="grid grid-cols-2 gap-3">
        <Card>
          <CardHeader className="pb-2">
            <CardTitle className="flex items-center gap-2 text-sm font-medium">
              <Trophy className="size-4 text-violet-500" />
              连登抽奖
            </CardTitle>
          </CardHeader>
          <CardContent className="space-y-3">
            <div className="text-2xl font-bold tabular-nums">
              {current?.streak?.chances ?? 0}
              <span className="ml-2 text-xs font-normal text-muted-foreground">
                本账号次数 · 全池 {streakN}
              </span>
            </div>
            {current?.streak?.error && (
              <p className="text-[11px] text-destructive">{current.streak.error}</p>
            )}
            <div className="flex flex-wrap gap-2">
              <Button
                size="sm"
                disabled={!uid || busy !== null}
                onClick={() => void draw("streak")}
              >
                {busy === "streak" ? (
                  <LoaderCircle className="mr-1.5 size-3.5 animate-spin" />
                ) : (
                  <Dices className="mr-1.5 size-3.5" />
                )}
                抽空本账号
              </Button>
              <Button
                variant="secondary"
                size="sm"
                disabled={busy !== null}
                onClick={() => void draw("streak", true)}
              >
                {busy === "streak-all" ? (
                  <LoaderCircle className="mr-1.5 size-3.5 animate-spin" />
                ) : (
                  <Layers className="mr-1.5 size-3.5" />
                )}
                抽空全部账号
              </Button>
            </div>
          </CardContent>
        </Card>

        <Card>
          <CardHeader className="pb-2">
            <CardTitle className="flex items-center gap-2 text-sm font-medium">
              <GraduationCap className="size-4 text-sky-500" />
              开学季转盘
            </CardTitle>
          </CardHeader>
          <CardContent className="space-y-3">
            <div className="text-2xl font-bold tabular-nums">
              {current?.school?.chance?.balance ?? 0}
              <span className="ml-2 text-xs font-normal text-muted-foreground">
                本账号余额 · 全池 {schoolN}
                {current?.school && !current.school.in_period ? " · 活动未开启" : ""}
              </span>
            </div>
            {current?.school?.error && (
              <p className="text-[11px] text-destructive">{current.school.error}</p>
            )}
            <div className="flex flex-wrap gap-2">
              <Button
                size="sm"
                disabled={!uid || busy !== null}
                onClick={() => void draw("school")}
              >
                {busy === "school" ? (
                  <LoaderCircle className="mr-1.5 size-3.5 animate-spin" />
                ) : (
                  <Dices className="mr-1.5 size-3.5" />
                )}
                抽空本账号
              </Button>
              <Button
                variant="secondary"
                size="sm"
                disabled={busy !== null}
                onClick={() => void draw("school", true)}
              >
                {busy === "school-all" ? (
                  <LoaderCircle className="mr-1.5 size-3.5 animate-spin" />
                ) : (
                  <Layers className="mr-1.5 size-3.5" />
                )}
                抽空全部账号
              </Button>
            </div>
          </CardContent>
        </Card>
      </div>

      {loading && all.length === 0 ? (
        <Skeleton className="h-48 w-full" />
      ) : (
        <Card>
          <CardHeader className="pb-2">
            <CardTitle className="text-sm font-medium">账号余额</CardTitle>
          </CardHeader>
          <CardContent className="p-0">
            <div className="divide-y divide-border/60">
              {all.map((row) => (
                <button
                  key={row.uid}
                  type="button"
                  onClick={() => setUid(row.uid)}
                  className={cn(
                    "flex w-full items-center gap-3 px-4 py-2.5 text-left text-sm hover:bg-accent/50",
                    row.uid === uid && "bg-accent/40",
                  )}
                >
                  <span className="min-w-0 flex-1 truncate">
                    {row.nickname || row.uid.slice(0, 8)}
                  </span>
                  <span className="tabular-nums text-[12px] text-violet-600 dark:text-violet-400">
                    连登 {row.streak?.chances ?? 0}
                  </span>
                  <span className="tabular-nums text-[12px] text-sky-600 dark:text-sky-400">
                    转盘 {row.school?.chance?.balance ?? 0}
                  </span>
                </button>
              ))}
              {all.length === 0 && (
                <p className="py-8 text-center text-xs text-muted-foreground">
                  暂无可用 WorkBuddy 账号
                </p>
              )}
            </div>
          </CardContent>
        </Card>
      )}
    </div>
  );
}
