import { useCallback, useEffect, useState } from "react";
import {
  Ban,
  CalendarDays,
  RefreshCw,
  ShieldAlert,
  ShieldCheck,
} from "lucide-react";
import { api } from "@/lib/api-client";
import type { FirewallStatsResult } from "@/types";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { cn } from "@/lib/utils";

// 规则分级：A 级（平台最高红线）/ B 级（武器毒品恶意软件等）/ 声明类（越狱）
const RULE_LEVEL: Record<string, { level: "A" | "B" | "C"; label: string }> = {
  csam: { level: "A", label: "未成年性化" },
  terror: { level: "A", label: "暴恐宣传" },
  "weapon-cbrn": { level: "B", label: "杀伤武器制造" },
  "drug-synthesis": { level: "B", label: "毒品合成" },
  malware: { level: "B", label: "恶意软件开发" },
  "nude-deepfake": { level: "B", label: "真人裸体深伪" },
  "self-harm": { level: "B", label: "自杀教唆" },
  "minor-adult-claim": { level: "C", label: "未成年成年化声明" },
  "nsfw-legalize": { level: "C", label: "NSFW 合法化声明" },
  "explicit-allow": { level: "C", label: "允许暴力色情" },
  "bypass-review": { level: "C", label: "审核绕过指令" },
  "no-safety-claim": { level: "C", label: "无安全限制声明" },
  "minor-nsfw-cooccur": { level: "C", label: "未成年×NSFW 共现" },
};

const levelStyle = {
  A: "bg-red-500/15 text-red-600 dark:text-red-400 border-red-500/30",
  B: "bg-orange-500/15 text-orange-600 dark:text-orange-400 border-orange-500/30",
  C: "bg-amber-500/15 text-amber-600 dark:text-amber-400 border-amber-500/30",
} as const;

const fmtTime = (unix: number) =>
  new Date(unix * 1000).toLocaleString("zh-CN", {
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  });

export default function FirewallPage() {
  const [data, setData] = useState<FirewallStatsResult | null>(null);
  const [loading, setLoading] = useState(true);
  const [refreshing, setRefreshing] = useState(false);
  const [detail, setDetail] = useState<number | null>(null); // 打开弹窗的事件索引

  const load = useCallback(async () => {
    setRefreshing(true);
    try {
      setData(await api.firewallStats());
    } catch {
      /* keep */
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

  const events = data?.events ?? [];
  const rules = data?.rules ?? [];
  const maxCount = Math.max(...rules.map((r) => r.count), 1);
  const detailEvent = detail != null ? events[detail] : undefined;

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div>
          <h2 className="flex items-center gap-2 text-lg font-semibold">
            <ShieldAlert className="size-5 text-red-500" />
            内容防火墙
          </h2>
          <p className="text-xs text-muted-foreground">
            违禁内容在出网关前拦截（对齐主流大模型平台政策红线）· 命中即 403
            无旁路 · 30 秒自动刷新 · 事件保留最近 500 条
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

      {/* 状态与统计 */}
      <div className="grid grid-cols-2 gap-3 md:grid-cols-4">
        <Card>
          <CardContent className="pt-4">
            <div className="flex items-center gap-1.5 text-xl font-bold">
              {data?.enabled ? (
                <>
                  <ShieldCheck className="size-5 text-emerald-500" />
                  <span className="text-emerald-600 dark:text-emerald-400">
                    防护中
                  </span>
                </>
              ) : (
                <>
                  <ShieldAlert className="size-5 text-red-500" />
                  <span className="text-red-600 dark:text-red-400">已停用</span>
                </>
              )}
            </div>
            <div className="text-[11px] text-muted-foreground">防火墙状态</div>
          </CardContent>
        </Card>
        <Card>
          <CardContent className="pt-4">
            <div className="text-xl font-bold tabular-nums text-red-600 dark:text-red-400">
              {data?.total ?? 0}
            </div>
            <div className="text-[11px] text-muted-foreground">累计拦截</div>
          </CardContent>
        </Card>
        <Card>
          <CardContent className="pt-4">
            <div className="text-xl font-bold tabular-nums text-orange-600 dark:text-orange-400">
              {data?.today ?? 0}
            </div>
            <div className="text-[11px] text-muted-foreground">今日拦截</div>
          </CardContent>
        </Card>
        <Card>
          <CardContent className="pt-4">
            <div className="text-xl font-bold tabular-nums">{rules.length}</div>
            <div className="text-[11px] text-muted-foreground">
              命中过的规则数
            </div>
          </CardContent>
        </Card>
      </div>

      {/* 规则分布 */}
      {!!rules.length && (
        <Card>
          <CardHeader className="pb-2">
            <CardTitle className="flex items-center gap-1.5 text-sm font-medium">
              <Ban className="size-4 text-red-500" />
              拦截规则分布
            </CardTitle>
          </CardHeader>
          <CardContent>
            <div className="space-y-1.5">
              {rules.map((r) => {
                const meta = RULE_LEVEL[r.rule];
                return (
                  <div key={r.rule} className="flex items-center gap-2 text-xs">
                    <span
                      className={cn(
                        "w-44 shrink-0 truncate rounded border px-1.5 py-0.5 text-[10px]",
                        levelStyle[meta?.level ?? "C"],
                      )}
                      title={r.rule}
                    >
                      {meta?.label ?? r.rule}
                    </span>
                    <div className="h-2.5 flex-1 overflow-hidden rounded-full bg-muted">
                      <div
                        className={cn(
                          "h-full rounded-full",
                          meta?.level === "A"
                            ? "bg-red-500/70"
                            : meta?.level === "B"
                              ? "bg-orange-500/70"
                              : "bg-amber-500/70",
                        )}
                        style={{ width: `${(r.count / maxCount) * 100}%` }}
                      />
                    </div>
                    <span className="w-16 shrink-0 text-right tabular-nums text-muted-foreground">
                      {r.count} 次
                    </span>
                  </div>
                );
              })}
            </div>
          </CardContent>
        </Card>
      )}

      {/* 拦截事件流 */}
      <Card>
        <CardHeader className="pb-2">
          <CardTitle className="flex items-center gap-1.5 text-sm font-medium">
            <CalendarDays className="size-4 text-orange-500" />
            拦截事件
          </CardTitle>
        </CardHeader>
        <CardContent>
          {loading ? (
            <Skeleton className="h-32 w-full" />
          ) : events.length === 0 ? (
            <p className="flex h-36 items-center justify-center text-center text-xs text-muted-foreground">
              暂无拦截记录——防护开启以来没有违禁内容触达网关
            </p>
          ) : (
            <div className="h-80 space-y-1.5 overflow-y-auto pr-1">
              {events.map((e, i) => {
                const meta = RULE_LEVEL[e.rule];
                return (
                  <div
                    key={`${e.at}-${i}`}
                    className="flex cursor-pointer items-start gap-2 rounded-md bg-muted/40 px-2.5 py-1.5 text-xs transition-colors hover:bg-muted"
                    onClick={() => setDetail(i)}
                    title="点击查看完整内容"
                  >
                    <span className="shrink-0 tabular-nums text-muted-foreground">
                      {fmtTime(e.at)}
                    </span>
                    <span
                      className={cn(
                        "shrink-0 rounded border px-1.5 py-0.5 text-[10px] font-medium",
                        levelStyle[meta?.level ?? "C"],
                      )}
                    >
                      {meta?.label ?? e.rule}
                    </span>
                    <span className="min-w-0 flex-1">
                      <span
                        className="block truncate text-muted-foreground"
                        title={e.snippet}
                      >
                        {e.snippet || "（无文本内容）"}
                      </span>
                      <span className="mt-0.5 block font-mono text-[10px] text-muted-foreground/70">
                        {e.model || "-"} · {(e.uid || "").slice(0, 8)}
                      </span>
                    </span>
                  </div>
                );
              })}
            </div>
          )}
        </CardContent>
      </Card>

      <p className="text-[11px] text-muted-foreground">
        规则分级：红 = 平台最高红线（未成年性化/暴恐）；橙 =
        政策高危（武器/毒品/恶意软件/深伪/自杀教唆，主题×意图双信号）；黄 =
        越狱与合法化声明。B 级采用双信号共现，正常编程与安全研究不误杀。
      </p>

      {/* 完整内容弹窗 */}
      <Dialog
        open={detailEvent != null}
        onOpenChange={(next) => !next && setDetail(null)}
      >
        <DialogContent className="sm:max-w-2xl">
          <DialogHeader>
            <DialogTitle className="flex flex-wrap items-center gap-2">
              <span
                className={cn(
                  "rounded border px-1.5 py-0.5 text-[10px] font-medium",
                  levelStyle[RULE_LEVEL[detailEvent?.rule ?? ""]?.level ?? "C"],
                )}
              >
                {RULE_LEVEL[detailEvent?.rule ?? ""]?.label ??
                  detailEvent?.rule}
              </span>
              拦截详情
            </DialogTitle>
            {detailEvent && (
              <DialogDescription className="font-mono text-[11px]">
                {fmtTime(detailEvent.at)} · {detailEvent.model || "-"} ·{" "}
                {(detailEvent.uid || "").slice(0, 8)}
              </DialogDescription>
            )}
          </DialogHeader>
          <pre className="max-h-[60vh] overflow-auto whitespace-pre-wrap break-all rounded-md bg-muted/50 p-3 font-mono text-xs leading-relaxed">
            {detailEvent?.content || detailEvent?.snippet || "（无文本内容）"}
          </pre>
        </DialogContent>
      </Dialog>
    </div>
  );
}
