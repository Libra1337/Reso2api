import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  CalendarClock,
  Flame,
  Gift,
  LoaderCircle,
  PawPrint,
  Plane,
  RefreshCw,
  Radio,
  Send,
  Sparkles,
} from "lucide-react";
import { api } from "@/lib/api-client";
import type { TaskEvent, TravelStatusEntry, TravelStatusResult } from "@/types";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { cn } from "@/lib/utils";

// ─────────────────────────── 工具 ───────────────────────────

const fmtTime = (unix: number) => {
  if (!unix) return "-";
  return new Date(unix * 1000).toLocaleTimeString("zh-CN", {
    hour: "2-digit",
    minute: "2-digit",
  });
};

const fmtCountdown = (unix: number) => {
  const diff = unix - Date.now() / 1000;
  if (diff <= 0) return "已到站";
  const h = Math.floor(diff / 3600);
  const m = Math.floor((diff % 3600) / 60);
  const s = Math.floor(diff % 60);
  if (h > 0) return `${h}小时${m}分`;
  if (m > 0) return `${m}分${s}秒`;
  return `${s}秒`;
};

const letterText = (letter: Record<string, unknown> | null | undefined) => {
  if (!letter) return null;
  for (const k of ["content", "text", "message", "title"]) {
    const v = letter[k];
    if (typeof v === "string" && v.trim()) return v;
  }
  return null;
};

const locationName = (loc: Record<string, unknown> | null | undefined) => {
  if (!loc) return null;
  for (const k of ["name", "title", "location_name"]) {
    const v = loc[k];
    if (typeof v === "string" && v.trim()) return v;
  }
  return null;
};

// ─────────────────────────── 猫咪场景动画 ───────────────────────────

function CatScene({ entry }: { entry: TravelStatusEntry }) {
  const t = entry.travel;
  if (entry.disabled) {
    return (
      <div className="relative flex h-28 items-end justify-center overflow-hidden rounded-lg bg-gradient-to-b from-muted to-muted/40">
        <span className="cat-sad absolute bottom-3 text-4xl">😿</span>
        <span className="absolute right-2 top-2 rounded-full bg-muted-foreground/20 px-2 py-0.5 text-[10px] text-muted-foreground">
          已禁用
        </span>
      </div>
    );
  }
  if (!entry.buddy) {
    return (
      <div className="relative flex h-28 items-end justify-center overflow-hidden rounded-lg bg-gradient-to-b from-violet-100 to-violet-50 dark:from-violet-950/60 dark:to-violet-900/20">
        <span
          className="cloud absolute top-3 text-2xl opacity-60"
          style={{ left: "15%", animationDuration: "26s" }}
        >
          ☁️
        </span>
        <span
          className="cloud absolute top-6 text-xl opacity-40"
          style={{ right: "20%", animationDuration: "34s" }}
        >
          ☁️
        </span>
        <span className="egg-wobble absolute bottom-3 text-4xl">🥚</span>
        <span className="absolute bottom-1 left-2 text-[10px] text-muted-foreground">
          待领养 · 触发「活跃上报」补对话量
        </span>
      </div>
    );
  }
  if (t?.state === "arrived") {
    return (
      <div className="relative flex h-28 items-end justify-center overflow-hidden rounded-lg bg-gradient-to-b from-amber-100 to-orange-50 dark:from-amber-950/60 dark:to-orange-900/20">
        <span className="glow-pulse absolute inset-0" />
        <span className="cat-excited absolute bottom-3 left-[38%] text-4xl">
          🐱
        </span>
        <span className="gift-bounce absolute bottom-3 right-[30%] text-4xl">
          🎁
        </span>
        <span className="absolute right-2 top-2 animate-pulse rounded-full bg-amber-500 px-2 py-0.5 text-[10px] font-semibold text-white shadow">
          可领 +{t.reward_credit} 积分
        </span>
        <span className="absolute bottom-1 left-2 text-[10px] text-muted-foreground">
          已到站 · 点「旅行巡检」领奖并再派出
        </span>
      </div>
    );
  }
  if (t?.state === "traveling") {
    const now = Date.now() / 1000;
    const pct =
      t.depart_at && t.arrive_at && t.arrive_at > t.depart_at
        ? Math.min(
            100,
            Math.max(
              2,
              ((now - t.depart_at) / (t.arrive_at - t.depart_at)) * 100,
            ),
          )
        : 50;
    const loc = locationName(t.location);
    return (
      <div className="relative flex h-28 items-end justify-center overflow-hidden rounded-lg bg-gradient-to-b from-sky-100 to-sky-50 dark:from-sky-950/60 dark:to-sky-900/20">
        <span
          className="cloud absolute top-3 text-2xl opacity-70"
          style={{ left: "10%", animationDuration: "22s" }}
        >
          ☁️
        </span>
        <span
          className="cloud absolute top-6 text-xl opacity-50"
          style={{ right: "15%", animationDuration: "30s" }}
        >
          ☁️
        </span>
        <span className="cat-walk absolute bottom-3 text-4xl">🐈</span>
        <span className="absolute bottom-3 left-2 text-2xl">🧳</span>
        {loc && <span className="absolute bottom-3 right-2 text-xl">🏘️</span>}
        <div className="absolute bottom-0 left-0 h-1.5 w-full bg-black/10 dark:bg-white/10">
          <div
            className="h-full bg-gradient-to-r from-sky-400 to-emerald-400 transition-all"
            style={{ width: `${pct}%` }}
          />
        </div>
      </div>
    );
  }
  // idle
  return (
    <div className="relative flex h-28 items-end justify-center overflow-hidden rounded-lg bg-gradient-to-b from-orange-100 to-rose-50 dark:from-orange-950/50 dark:to-rose-900/20">
      <span className="absolute right-3 top-2 text-lg opacity-70">🌇</span>
      <span className="cat-sleep absolute bottom-3 text-4xl">🐱</span>
      <span className="zzz absolute bottom-9 right-[42%] text-sm font-bold text-muted-foreground">
        z
      </span>
      <span
        className="zzz absolute bottom-12 right-[38%] text-xs font-bold text-muted-foreground"
        style={{ animationDelay: "0.7s" }}
      >
        Z
      </span>
      <span
        className="zzz absolute bottom-14 right-[34%] text-[10px] font-bold text-muted-foreground"
        style={{ animationDelay: "1.4s" }}
      >
        Z
      </span>
      <span className="absolute bottom-1 left-2 text-[10px] text-muted-foreground">
        {t?.daily_limit_reached
          ? "今日已派出，明天再来"
          : "休息中 · 可派出旅行"}
      </span>
    </div>
  );
}

// ─────────────────────────── 单账号卡片 ───────────────────────────

function CatCard({ entry }: { entry: TravelStatusEntry }) {
  const t = entry.travel;
  const streak = entry.streak;
  const letter = useMemo(() => letterText(t?.letter), [t?.letter]);
  const [, forceTick] = useState(0);
  useEffect(() => {
    if (t?.state !== "traveling" || !t.arrive_at) return;
    const id = setInterval(() => forceTick((v) => v + 1), 1000);
    return () => clearInterval(id);
  }, [t?.state, t?.arrive_at]);

  return (
    <Card className="overflow-hidden transition-shadow hover:shadow-md">
      <div className="p-3 pb-0">
        <CatScene entry={entry} />
      </div>
      <CardContent className="space-y-2 p-3">
        <div className="flex items-baseline justify-between gap-2">
          <div className="min-w-0">
            <div className="truncate text-sm font-semibold">
              {entry.buddy
                ? entry.buddy.name || `猫猫 #${entry.buddy.id}`
                : "尚未领养"}
            </div>
            <div className="truncate text-xs text-muted-foreground">
              {entry.nickname || entry.uid.slice(0, 8)}
            </div>
          </div>
          {streak && (
            <div className="flex shrink-0 items-center gap-1 rounded-full bg-orange-500/10 px-2 py-0.5 text-xs font-medium text-orange-600 dark:text-orange-400">
              <Flame className="size-3" />
              {streak.days} 天
            </div>
          )}
        </div>

        {t?.state === "traveling" && t.arrive_at > 0 && (
          <div className="flex items-center justify-between rounded-md bg-muted/60 px-2 py-1 text-xs">
            <span className="flex items-center gap-1 text-muted-foreground">
              <Plane className="size-3" />
              {fmtTime(t.depart_at)} 出发
            </span>
            <span className="font-medium tabular-nums text-emerald-600 dark:text-emerald-400">
              {fmtCountdown(t.arrive_at)}后到站
            </span>
          </div>
        )}
        {t?.state === "arrived" && (
          <div className="flex items-center gap-1 rounded-md bg-amber-500/10 px-2 py-1 text-xs font-medium text-amber-700 dark:text-amber-400">
            <Gift className="size-3" />
            到站待领奖 · 奖励 {t.reward_credit} 积分
          </div>
        )}
        {t?.state === "idle" && !t.daily_limit_reached && (
          <div className="flex items-center gap-1 rounded-md bg-muted/60 px-2 py-1 text-xs text-muted-foreground">
            <PawPrint className="size-3" />
            空闲中 · 等待下次巡检派出
          </div>
        )}

        {letter && (
          <div className="rounded-md border border-dashed border-amber-300/60 bg-amber-50/50 px-2 py-1 text-xs italic text-amber-800 dark:border-amber-700/40 dark:bg-amber-950/30 dark:text-amber-300">
            💌 {letter.length > 60 ? letter.slice(0, 60) + "…" : letter}
          </div>
        )}

        {streak && streak.next_tier && (
          <div className="flex items-center justify-between text-[11px] text-muted-foreground">
            <span>
              下一档 {streak.next_tier} 还差 {streak.next_tier_remaining} 天
            </span>
            <span>
              本月 {streak.month_consumed_days}/{streak.month_total_days} 天
            </span>
          </div>
        )}
        {entry.error && (
          <div className="text-[11px] text-destructive">{entry.error}</div>
        )}
      </CardContent>
    </Card>
  );
}

// ─────────────────────────── 页面 ───────────────────────────

export default function CatsPage() {
  const [data, setData] = useState<TravelStatusResult | null>(null);
  const [loading, setLoading] = useState(true);
  const [refreshing, setRefreshing] = useState(false);
  const [actionBusy, setActionBusy] = useState<
    "travel" | "activity" | "claim" | null
  >(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [tasks, setTasks] = useState<TaskEvent[]>([]);
  const [travelRunning, setTravelRunning] = useState(false);
  const [activityRunning, setActivityRunning] = useState(false);
  const seenEndRef = useRef(0); // 已见完成事件数：出现新完成 → 自动刷新状态

  const load = useCallback(async (force = false) => {
    setRefreshing(true);
    try {
      const r = await api.travelStatus(force);
      setData(r);
    } catch {
      /* 网络抖动保留旧数据 */
    } finally {
      setLoading(false);
      setRefreshing(false);
    }
  }, []);

  useEffect(() => {
    void load(true);
    const id = setInterval(() => void load(false), 60_000); // 与上游 poll_interval 对齐
    return () => clearInterval(id);
  }, [load]);

  // 任务动态流轮询：运行中 3s 高频，空闲 10s 低频；出现新的「完成」事件自动刷新猫咪状态
  useEffect(() => {
    let stopped = false;
    const poll = async () => {
      try {
        const r = await api.tasks();
        if (stopped) return;
        setTasks(r.events ?? []);
        setTravelRunning(r.travel_running);
        setActivityRunning(r.activity_running);
        const endEvents = (r.events ?? []).filter(
          (e) => !e.uid && e.msg.includes("完成"),
        );
        if (endEvents.length > seenEndRef.current && seenEndRef.current > 0) {
          void load(true); // 新一轮任务刚结束 → 立即刷新状态看结果
          // 上报刚结束 → 自动接一轮旅行巡检（把刚领养的猫派出，新号一键走完全流程）
          const last = endEvents[0];
          if (last?.kind === "activity" && !r.travel_running) {
            setNotice("活跃上报完成，自动开始旅行巡检（领养→派出）…");
            void api
              .travelRunAll()
              .catch(() => {})
              .finally(() => setTimeout(() => void load(true), 90_000));
          }
        }
        seenEndRef.current = endEvents.length;
      } catch {
        /* ignore */
      }
    };
    void poll();
    const id = setInterval(
      poll,
      travelRunning || activityRunning ? 3_000 : 10_000,
    );
    return () => {
      stopped = true;
      clearInterval(id);
    };
  }, [travelRunning, activityRunning, load]);

  const runAction = async (kind: "travel" | "activity") => {
    setActionBusy(kind);
    setNotice(null);
    try {
      if (kind === "travel") await api.travelRunAll();
      else await api.activityRunAll();
      setNotice(
        kind === "travel"
          ? "旅行巡检已启动，下方动态实时更新，完成后自动刷新"
          : "活跃上报已启动，下方动态实时更新，完成后自动刷新",
      );
      // 兜底：动态流之外按预期时长再刷一次（防轮询错过完成事件）
      setTimeout(() => void load(true), kind === "travel" ? 90_000 : 180_000);
    } finally {
      setActionBusy(null);
    }
  };

  const accounts = data?.accounts ?? [];
  const cats = accounts.filter((a) => a.buddy);
  const traveling = cats.filter((a) => a.travel?.state === "traveling");
  const arrived = cats.filter((a) => a.travel?.state === "arrived");
  const claimable = arrived.reduce(
    (s, a) => s + (a.travel?.reward_credit ?? 0),
    0,
  );
  const maxStreak = accounts.reduce(
    (m, a) => Math.max(m, a.streak?.days ?? 0),
    0,
  );
  const noCat = accounts.filter((a) => !a.disabled && !a.buddy);

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div>
          <h2 className="flex items-center gap-2 text-lg font-semibold">
            <PawPrint className="size-5 text-orange-500" />
            猫猫乐园
          </h2>
          <p className="text-xs text-muted-foreground">
            每日 09/21 点自动巡检旅行 · 10 点活跃上报保连登
            {data && (
              <span className="ml-1">
                · 更新于{" "}
                {new Date(data.fetched_at * 1000).toLocaleTimeString("zh-CN")}
              </span>
            )}
          </p>
        </div>
        <div className="flex flex-wrap gap-2">
          <Button
            variant="outline"
            size="sm"
            disabled={actionBusy !== null}
            onClick={() => void runAction("travel")}
          >
            {actionBusy === "travel" ? (
              <LoaderCircle className="mr-1.5 size-3.5 animate-spin" />
            ) : (
              <PawPrint className="mr-1.5 size-3.5" />
            )}
            旅行巡检
          </Button>
          <Button
            variant="outline"
            size="sm"
            disabled={actionBusy !== null}
            onClick={() => void runAction("activity")}
          >
            {actionBusy === "activity" ? (
              <LoaderCircle className="mr-1.5 size-3.5 animate-spin" />
            ) : (
              <Send className="mr-1.5 size-3.5" />
            )}
            活跃上报
          </Button>
          <Button
            variant="outline"
            size="sm"
            onClick={() => void load(true)}
            disabled={refreshing}
          >
            <RefreshCw
              className={cn("mr-1.5 size-3.5", refreshing && "animate-spin")}
            />
            刷新
          </Button>
        </div>
      </div>

      <p className="flex items-center gap-1 text-[11px] text-muted-foreground">
        <CalendarClock className="size-3" />
        领养 +300 积分一次性 · 旅行每天 1 趟 ·
        连登按天累积，断签可用补签卡（上游规则）
      </p>

      {notice && (
        <div className="flex items-center gap-2 rounded-md border border-emerald-200 bg-emerald-50 px-3 py-2 text-xs text-emerald-700 dark:border-emerald-900 dark:bg-emerald-950/40 dark:text-emerald-400">
          <Sparkles className="size-3.5" />
          {notice}
        </div>
      )}

      {(travelRunning || activityRunning) && (
        <div className="flex items-center gap-2 rounded-md border border-sky-200 bg-sky-50 px-3 py-2 text-xs font-medium text-sky-700 dark:border-sky-900 dark:bg-sky-950/40 dark:text-sky-400">
          <LoaderCircle className="size-3.5 animate-spin" />
          {travelRunning ? "旅行巡检进行中…" : "活跃上报进行中…"}
          {tasks[0]?.msg && (
            <span className="font-normal text-muted-foreground">
              最新：{tasks[0].msg}
            </span>
          )}
        </div>
      )}

      {/* 任务动态流 */}
      <Card>
        <CardHeader className="pb-2">
          <CardTitle className="flex items-center gap-1.5 text-sm font-medium">
            <Radio className="size-4 text-sky-500" />
            任务动态
            <span className="ml-1 text-[11px] font-normal text-muted-foreground">
              巡检 / 上报的逐账号实时反馈
            </span>
          </CardTitle>
        </CardHeader>
        <CardContent>
          {tasks.length === 0 ? (
            <p className="flex h-44 items-center justify-center text-center text-xs text-muted-foreground">
              暂无动态——点上方按钮触发一次，或等定时任务（09/21 点巡检 · 10
              点上报）
            </p>
          ) : (
            <div className="h-44 space-y-1 overflow-y-auto pr-1">
              {tasks.slice(0, 40).map((e, i) => (
                <div
                  key={`${e.at}-${i}`}
                  className="flex items-baseline gap-2 text-xs"
                >
                  <span
                    className={cn(
                      "mt-1 size-1.5 shrink-0 rounded-full",
                      !e.uid
                        ? "bg-sky-500"
                        : e.msg.includes("失败") ||
                            e.msg.includes("中断") ||
                            e.msg.includes("疑似")
                          ? "bg-amber-500"
                          : e.msg.includes("+")
                            ? "bg-emerald-500"
                            : "bg-muted-foreground/40",
                    )}
                  />
                  <span className="shrink-0 tabular-nums text-muted-foreground">
                    {new Date(e.at * 1000).toLocaleTimeString("zh-CN", {
                      hour: "2-digit",
                      minute: "2-digit",
                      second: "2-digit",
                    })}
                  </span>
                  <span
                    className={cn(
                      "shrink-0",
                      e.kind === "travel"
                        ? "text-orange-500"
                        : "text-violet-500",
                    )}
                  >
                    {e.kind === "travel" ? "巡检" : "上报"}
                  </span>
                  <span
                    className={cn("min-w-0 truncate", !e.uid && "font-medium")}
                  >
                    {e.uid ? `· ${e.msg}` : e.msg}
                  </span>
                </div>
              ))}
            </div>
          )}
        </CardContent>
      </Card>

      {/* 统计卡 */}
      <div className="grid grid-cols-2 gap-3 md:grid-cols-4">
        {[
          {
            icon: "🐱",
            title: "已领养",
            value: `${cats.length}/${accounts.filter((a) => !a.disabled).length}`,
            detail: noCat.length
              ? `${noCat.length} 个新号待领养`
              : "全部领养完成",
          },
          {
            icon: "🧳",
            title: "旅行中",
            value: `${traveling.length}`,
            detail: "在路上赚积分",
          },
          {
            icon: "🎁",
            title: "待领奖",
            value: `${arrived.length}`,
            detail: arrived.length ? `共 ${claimable} 积分` : "暂无到站",
          },
          {
            icon: "🔥",
            title: "最高连登",
            value: `${maxStreak} 天`,
            detail: "连登奖励阶梯成长",
          },
        ].map((s) => (
          <Card key={s.title}>
            <CardHeader className="pb-1">
              <CardTitle className="flex items-center gap-1.5 text-xs font-medium text-muted-foreground">
                <span>{s.icon}</span>
                {s.title}
              </CardTitle>
            </CardHeader>
            <CardContent>
              <div className="text-xl font-bold tabular-nums">
                {loading ? "…" : s.value}
              </div>
              <div className="text-[11px] text-muted-foreground">
                {s.detail}
              </div>
            </CardContent>
          </Card>
        ))}
      </div>

      {/* 猫咪卡片：按生命周期分组 */}
      {loading ? (
        <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-3 xl:grid-cols-4">
          {Array.from({ length: 8 }).map((_, i) => (
            <Skeleton key={i} className="h-56 w-full" />
          ))}
        </div>
      ) : accounts.length === 0 ? (
        <Card>
          <CardContent className="flex flex-col items-center gap-2 py-10 text-sm text-muted-foreground">
            <PawPrint className="size-8 opacity-40" />
            暂无 WorkBuddy 账号，先去「账号管理」导入
          </CardContent>
        </Card>
      ) : (
        <div className="space-y-5">
          {[
            {
              key: "arrived",
              emoji: "🎁",
              title: "到站待领奖",
              desc: arrived.length ? `共 ${claimable} 积分待领` : "",
              list: arrived,
              action: "claim" as const,
            },
            {
              key: "traveling",
              emoji: "🧳",
              title: "旅行中",
              desc: "",
              list: traveling,
            },
            {
              key: "idle",
              emoji: "😴",
              title: "空闲",
              desc: "",
              list: cats.filter((a) => a.travel?.state === "idle" || !a.travel),
            },
            {
              key: "nocat",
              emoji: "🥚",
              title: "待领养（新账号）",
              desc: "流程：活跃上报补 5 条对话 → 自动领养 +300 → 下一轮巡检派出",
              list: noCat,
              action: "activity" as const,
            },
            {
              key: "disabled",
              emoji: "🚫",
              title: "已禁用",
              desc: "",
              list: accounts.filter((a) => a.disabled),
            },
          ]
            .filter((g) => g.list.length > 0)
            .map((g) => (
              <section key={g.key} className="space-y-2">
                <div className="flex flex-wrap items-center gap-2">
                  <h3 className="flex items-center gap-1.5 text-sm font-semibold">
                    <span>{g.emoji}</span>
                    {g.title}
                    <span className="rounded-full bg-muted px-1.5 py-0.5 text-[11px] font-normal text-muted-foreground">
                      {g.list.length}
                    </span>
                  </h3>
                  {g.desc && (
                    <span className="text-[11px] text-muted-foreground">
                      {g.desc}
                    </span>
                  )}
                  {g.action === "activity" && (
                    <Button
                      variant="outline"
                      size="sm"
                      className="h-6"
                      disabled={actionBusy !== null}
                      onClick={() => void runAction("activity")}
                    >
                      {actionBusy === "activity" ? (
                        <LoaderCircle className="mr-1 size-3 animate-spin" />
                      ) : (
                        <Send className="mr-1 size-3" />
                      )}
                      一键上报并领养
                    </Button>
                  )}
                  {g.action === "claim" && (
                    <Button
                      variant="outline"
                      size="sm"
                      className="h-6"
                      disabled={actionBusy !== null}
                      onClick={() => void runAction("claim")}
                    >
                      {actionBusy === "claim" ? (
                        <LoaderCircle className="mr-1 size-3 animate-spin" />
                      ) : (
                        <Gift className="mr-1 size-3" />
                      )}
                      领取全部奖励
                    </Button>
                  )}
                </div>
                <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-3 xl:grid-cols-4">
                  {g.list.map((a) => (
                    <CatCard key={a.uid} entry={a} />
                  ))}
                </div>
              </section>
            ))}
        </div>
      )}
    </div>
  );
}
