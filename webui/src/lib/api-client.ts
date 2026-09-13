import type { ApiError } from "@/types";

const API_BASE = "/api";

let _csrfToken: string | null = null;

export function setCsrfToken(token: string | null) {
  _csrfToken = token;
}

export function getCsrfToken() {
  return _csrfToken;
}

class ApiClientError extends Error {
  status: number;
  data: ApiError;

  constructor(status: number, data: ApiError) {
    super(data.error || "Request failed");
    this.status = status;
    this.data = data;
  }
}

export { ApiClientError };

async function request<T>(path: string, body?: unknown): Promise<T> {
  const isPost = body !== undefined;
  const headers: Record<string, string> = {};

  if (isPost) {
    headers["Content-Type"] = "application/json";
    if (_csrfToken) {
      headers["X-CSRF-Token"] = _csrfToken;
    }
  }

  const res = await fetch(`${API_BASE}${path}`, {
    method: isPost ? "POST" : "GET",
    headers,
    body: isPost ? JSON.stringify(body) : undefined,
    credentials: "same-origin",
  });

  const data = (await res.json().catch(() => ({}))) as ApiError;

  if (!res.ok) {
    if (res.status === 401) {
      setCsrfToken(null);
    }
    throw new ApiClientError(res.status, data);
  }

  return data as T;
}

export const api = {
  session: () => request<import("@/types").SessionInfo>("/session"),

  login: (password: string) =>
    request<import("@/types").LoginResult>("/login", { password }),

  logout: () => request<import("@/types").LogoutResult>("/logout", {}),

  getState: () => request<import("@/types").AppState>("/state"),

  loginStart: (channel: string) =>
    request<import("@/types").LoginStartResult>("/login/start", { channel }),

  loginCancel: () =>
    request<import("@/types").SimpleResult>("/login/cancel", {}),

  accountImport: (raw: string) =>
    request<import("@/types").ImportResult>("/account/import", { raw }),

  accountCheckin: (uid: string) =>
    request<import("@/types").SimpleResult>("/account/checkin", { uid }),

  accountCheckinAll: () =>
    request<import("@/types").CheckinAllResult>("/account/checkin_all", {}),

  travelRunAll: () =>
    request<import("@/types").SimpleResult>("/travel/run_all", {}),

  travelClaimAll: () =>
    request<import("@/types").SimpleResult>("/travel/claim_all", {}),

  firewallPage: (page = 0, size = 100) =>
    request<import("@/types").FirewallPageResult>(
      `/firewall/page?page=${page}&size=${size}`,
    ),

  reqLogPage: (page = 0, size = 100) =>
    request<import("@/types").ReqLogPageResult>(
      `/request_logs/page?page=${page}&size=${size}`,
    ),

  reqLogBodyUrl: (file: string) =>
    `/request_logs/body?file=${encodeURIComponent(file)}`,

  reqLogBody: async (file: string): Promise<string> => {
    const res = await fetch(
      `/request_logs/body?file=${encodeURIComponent(file)}`,
      {
        credentials: "same-origin",
      },
    );
    if (!res.ok) throw new Error(`加载失败 (${res.status})`);
    return res.text();
  },

  firewallStats: () =>
    request<import("@/types").FirewallStatsResult>("/firewall/stats"),

  usageStats: (days = 31, force = false) =>
    request<import("@/types").UsageStatsResult>(
      `/usage/stats?days=${days}${force ? "&refresh=1" : ""}`,
    ),

  tasksList: (uid?: string) =>
    request<import("@/types").TasksListResult>(
      `/tasks/list${uid ? `?uid=${encodeURIComponent(uid)}` : ""}`,
    ),

  taskAuto: (uid: string, taskCode: string) =>
    request<import("@/types").TaskAutoResult>("/tasks/auto", {
      uid,
      task_code: taskCode,
    }),

  taskAutoAll: (uid: string) =>
    request<import("@/types").SimpleResult>("/tasks/auto_all", { uid }),

  taskAutoAllBatch: () =>
    request<import("@/types").SimpleResult>("/tasks/auto_all", { all: true }),

  travelStatus: (force = false) =>
    request<import("@/types").TravelStatusResult>(
      `/travel/status${force ? "?refresh=1" : ""}`,
    ),

  tasks: () => request<import("@/types").TaskFeedResult>("/tasks"),

  limits: () => request<import("@/types").LimitsResult>("/limits"),

  activityRunAll: () =>
    request<import("@/types").SimpleResult>("/activity/run_all", {}),

  accountRefresh: (uid: string) =>
    request<import("@/types").SimpleResult>("/account/refresh", { uid }),

  accountRefreshAll: () =>
    request<import("@/types").RefreshAllResult>("/account/refresh_all", {}),

  accountRemove: (uid: string) =>
    request<import("@/types").SimpleResult>("/account/remove", { uid }),

  accountDisable: (uid: string, disabled: boolean) =>
    request<import("@/types").SimpleResult>("/account/disable", {
      uid,
      disabled,
    }),

  accountResourceDetail: (uid: string) =>
    request<import("@/types").ResourceDetail>("/account/resource_detail", {
      uid,
    }),

  configApiKey: (key: string) =>
    request<import("@/types").SimpleResult>("/config/api_key", { key }),

  configCheckinTimes: (times: string[]) =>
    request<import("@/types").SimpleResult>("/config/checkin_times", {
      times,
    }),

  fees: () => request<import("@/types").FeesInfo>("/fees"),

  feesRefresh: () =>
    request<import("@/types").SimpleResult>("/fees/refresh", {}),

  logs: () => request<import("@/types").LogsData>("/logs"),

  requestLogs: () =>
    request<{ logs: import("@/types").ReqLog[] }>("/request_logs"),
};
