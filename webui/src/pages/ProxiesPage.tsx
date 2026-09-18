import { useCallback, useEffect, useState } from "react";
import {
  CheckCircle2,
  Globe,
  Loader2,
  Plus,
  RefreshCw,
  Save,
  Trash2,
  XCircle,
} from "lucide-react";
import { api } from "@/lib/api-client";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { cn } from "@/lib/utils";

interface ProxyEntry {
  addr: string;
  user?: string;
  pass?: string;
  disabled?: boolean;
  note?: string;
}

interface TestState {
  loading: boolean;
  ip?: string;
  latency_ms?: number;
  error?: string;
}

export default function ProxiesPage() {
  const [proxies, setProxies] = useState<ProxyEntry[]>([]);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [bulkText, setBulkText] = useState("");
  const [tests, setTests] = useState<Record<string, TestState>>({});
  const [message, setMessage] = useState<string | null>(null);
  const [dirty, setDirty] = useState(false);

  const load = useCallback(async () => {
    setLoading(true);
    try {
      const data = await api.proxies();
      setProxies(data.proxies ?? []);
      setDirty(false);
    } catch {
      /* keep old */
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  const save = async () => {
    setSaving(true);
    setMessage(null);
    try {
      const res = await api.saveProxies(proxies);
      setMessage(`已保存 ${res.count} 个出口，热更新即时生效`);
      setDirty(false);
    } catch (e) {
      setMessage(`保存失败：${e instanceof Error ? e.message : "未知错误"}`);
    } finally {
      setSaving(false);
    }
  };

  const testOne = async (p: ProxyEntry, i: number) => {
    const key = `${i}:${p.addr}`;
    setTests((t) => ({ ...t, [key]: { loading: true } }));
    try {
      const res = await api.testProxy(p);
      setTests((t) => ({ ...t, [key]: { loading: false, ...res } }));
    } catch (e) {
      setTests((t) => ({
        ...t,
        [key]: { loading: false, error: e instanceof Error ? e.message : "失败" },
      }));
    }
  };

  const importBulk = () => {
    const lines = bulkText
      .split("\n")
      .map((l) => l.trim())
      .filter((l) => l !== "" && !l.startsWith("#"));
    const entries: ProxyEntry[] = [];
    for (const line of lines) {
      const parts = line.split(":");
      if (parts.length < 2) continue;
      const e: ProxyEntry = { addr: `${parts[0]}:${parts[1]}` };
      if (parts.length >= 4) {
        e.user = parts[2];
        e.pass = parts[3];
      }
      entries.push(e);
    }
    if (entries.length === 0) return;
    setProxies((prev) => {
      const seen = new Set(prev.map((p) => p.addr));
      const fresh = entries.filter((e) => !seen.has(e.addr));
      return [...prev, ...fresh];
    });
    setBulkText("");
    setDirty(true);
    setMessage(`解析到 ${entries.length} 条（已去重合并）`);
  };

  const update = (i: number, patch: Partial<ProxyEntry>) => {
    setProxies((prev) => prev.map((p, idx) => (idx === i ? { ...p, ...patch } : p)));
    setDirty(true);
  };

  const remove = (i: number) => {
    setProxies((prev) => prev.filter((_, idx) => idx !== i));
    setDirty(true);
  };

  const addRow = () => {
    setProxies((prev) => [...prev, { addr: "" }]);
    setDirty(true);
  };

  const enabledCount = proxies.filter((p) => p.addr && !p.disabled).length;

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div>
          <h1 className="flex items-center gap-2 text-xl font-semibold">
            <Globe className="size-5" />
            代理出口
          </h1>
          <p className="mt-1 text-sm text-muted-foreground">
            账号按哈希固定分配到出口 IP（池空则直连）。保存后热更新，无需重启。
            当前启用 {enabledCount} / {proxies.length} 个出口。
          </p>
        </div>
        <div className="flex gap-2">
          <Button variant="outline" size="sm" onClick={() => void load()} disabled={loading}>
            <RefreshCw className={cn("size-4", loading && "animate-spin")} />
            刷新
          </Button>
          <Button size="sm" onClick={() => void save()} disabled={saving || !dirty}>
            <Save className="size-4" />
            {saving ? "保存中…" : "保存"}
          </Button>
        </div>
      </div>

      {message && (
        <div className="rounded-md border bg-card px-4 py-2 text-sm">{message}</div>
      )}

      <Card>
        <CardContent className="space-y-3 p-4">
          <p className="text-sm font-medium">批量导入（每行一条：host:port:user:pass）</p>
          <textarea
            value={bulkText}
            onChange={(e) => setBulkText(e.target.value)}
            placeholder={"45.41.176.35:6333:user:pass\n92.113.129.6:7795:user:pass"}
            rows={4}
            className="w-full rounded-md border bg-background p-3 font-mono text-xs"
          />
          <div>
            <Button size="sm" variant="secondary" onClick={importBulk}>
              <Plus className="size-4" />
              解析并入列表
            </Button>
          </div>
        </CardContent>
      </Card>

      <Card>
        <CardContent className="p-0">
          {loading ? (
            <div className="flex items-center justify-center py-16">
              <Loader2 className="size-6 animate-spin text-muted-foreground" />
            </div>
          ) : (
            <div className="max-h-[60vh] overflow-auto">
              <table className="w-full text-sm">
                <thead className="sticky top-0 bg-muted/60 text-xs text-muted-foreground">
                  <tr className="border-b">
                    <th className="px-4 py-2.5 text-left font-medium">地址</th>
                    <th className="px-3 py-2.5 text-left font-medium">用户名</th>
                    <th className="px-3 py-2.5 text-left font-medium">密码</th>
                    <th className="px-3 py-2.5 text-center font-medium">启用</th>
                    <th className="px-3 py-2.5 text-right font-medium">测试</th>
                    <th className="w-10 px-2 py-2.5" />
                  </tr>
                </thead>
                <tbody>
                  {proxies.map((p, i) => {
                    const key = `${i}:${p.addr}`;
                    const st = tests[key];
                    return (
                      <tr key={key} className="border-b border-border/50">
                        <td className="px-4 py-1.5">
                          <Input
                            value={p.addr}
                            onChange={(e) => update(i, { addr: e.target.value })}
                            className="h-8 w-44 font-mono text-xs"
                            placeholder="host:port"
                          />
                        </td>
                        <td className="px-3 py-1.5">
                          <Input
                            value={p.user ?? ""}
                            onChange={(e) => update(i, { user: e.target.value })}
                            className="h-8 w-28 font-mono text-xs"
                          />
                        </td>
                        <td className="px-3 py-1.5">
                          <Input
                            value={p.pass ?? ""}
                            onChange={(e) => update(i, { pass: e.target.value })}
                            className="h-8 w-28 font-mono text-xs"
                            type="password"
                          />
                        </td>
                        <td className="px-3 py-1.5 text-center">
                          <input
                            type="checkbox"
                            checked={!p.disabled}
                            onChange={(e) => update(i, { disabled: !e.target.checked })}
                            className="size-4"
                          />
                        </td>
                        <td className="px-3 py-1.5">
                          <div className="flex items-center justify-end gap-2">
                            <Button
                              size="sm"
                              variant="ghost"
                              className="h-7 px-2"
                              disabled={!p.addr || st?.loading}
                              onClick={() => void testOne(p, i)}
                            >
                              {st?.loading ? (
                                <Loader2 className="size-3.5 animate-spin" />
                              ) : (
                                "测试"
                              )}
                            </Button>
                            {st?.ip && (
                              <span className="flex items-center gap-1 text-xs text-success">
                                <CheckCircle2 className="size-3.5" />
                                {st.ip}
                              </span>
                            )}
                            {st?.error && (
                              <span className="flex items-center gap-1 text-xs text-destructive">
                                <XCircle className="size-3.5 shrink-0" />
                                <span className="max-w-40 truncate">{st.error}</span>
                              </span>
                            )}
                          </div>
                        </td>
                        <td className="px-2 py-1.5">
                          <button
                            type="button"
                            onClick={() => remove(i)}
                            className="flex size-7 items-center justify-center rounded-md hover:bg-accent"
                            aria-label="删除"
                          >
                            <Trash2 className="size-4 text-muted-foreground" />
                          </button>
                        </td>
                      </tr>
                    );
                  })}
                  {proxies.length === 0 && (
                    <tr>
                      <td colSpan={6} className="py-10 text-center text-sm text-muted-foreground">
                        代理池为空：所有账号直连（不走路由代理）
                      </td>
                    </tr>
                  )}
                </tbody>
              </table>
            </div>
          )}
        </CardContent>
      </Card>
      <div className="flex justify-start">
        <Button size="sm" variant="outline" onClick={addRow}>
          <Plus className="size-4" />
          添加一行
        </Button>
      </div>
    </div>
  );
}
