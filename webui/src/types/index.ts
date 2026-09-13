export interface ApiError {
  error?: string;
}

export interface SessionInfo {
  authenticated: boolean;
  csrf_token?: string;
}

export interface LoginResult {
  success: boolean;
  error?: string;
  csrf_token?: string;
}

export interface LogoutResult {
  success: boolean;
}

export type Channel = "workbuddy" | "traework" | "qoder";

export interface AccountView {
  uid: string;
  group: Channel;
  nickname: string;
  credits: number;
  cooling: boolean;
  until: string;
  reason: string;
  disabled: boolean;
  err_count: number;
  last_checkin_ok: boolean;
  last_checkin_at: string;
  last_checkin_msg: string;
}

export interface AppState {
  accounts: AccountView[];
  checkin_times: string[];
  keepalive_hours: number[];
  listen_host: string;
  listen_port: number;
  api_key: string;
  login_busy: boolean;
  next_checkin: string;
  version: string;
  autostart: boolean;
  running: boolean;
  judge_enabled: boolean;
  judge_active: boolean;
  judge_base_url: string;
  judge_api_key: string;
  judge_model: string;
  judge_timeout_ms: number;
}

export interface LoginStartResult {
  auth_url: string;
}

export interface SimpleResult {
  ok?: boolean;
  msg?: string;
  error?: string;
}

export interface ImportResult {
  ok: boolean;
  uid?: string;
  nickname?: string;
  error?: string;
}

export interface FeeModel {
  model: string;
  rate: string;
  note: string;
}

export interface FeesInfo {
  note: string;
  disclaimer: string;
  cached_at: string;
  error?: string;
  channels: { channel: Channel; models: FeeModel[] }[];
}

export interface LogsData {
  lines: string[];
}

export interface ReqLog {
  time: string;
  body_file?: string;
  model: string;
  channel: string;
  uid: string;
  status: number;
  stream: boolean;
  ttfb_ms: number;
  total_ms: number;
  in_tokens: number;
  out_tokens: number;
  cached_tokens: number;
  credit: number;
}

export interface ResourceDetail {
  remain: number;
  items: {
    name: string;
    total: number;
    used: number;
    remain: number;
    got_at: number; // 获得时间（Unix 秒，0 = 未知）
    expire_at: string; // 到期文案
  }[];
}

export interface CheckinAllResult {
  results: {
    uid: string;
    ok: boolean;
    msg?: string;
    remain?: number;
    has_remain?: boolean;
  }[];
}

export interface RefreshAllResult {
  busy: boolean;
  total: number;
  ok: number;
  failed: number;
}

export interface BanCheckResult {
  uid: string;
  nickname?: string;
  group?: string;
  status: "ok" | "banned" | "session_dead" | "error" | string;
  detail?: string;
  disabled?: boolean;
}

export interface BanCheckAllResult {
  results: BanCheckResult[];
  total: number;
  ok: number;
  banned: number;
  session_dead: number;
  failed: number;
}

export interface CatBuddy {
  id: number;
  name: string;
}

export interface CatTravel {
  state: string; // idle | traveling | arrived
  daily_limit_reached: boolean;
  record_id: number;
  reward_credit: number;
  depart_at: number;
  arrive_at: number;
  location?: Record<string, unknown> | null;
  letter?: Record<string, unknown> | null;
}

export interface CatStreak {
  days: number;
  month_total_days: number;
  month_consumed_days: number;
  next_tier: string;
  next_tier_remaining: number;
}

export interface TravelStatusEntry {
  uid: string;
  nickname: string;
  disabled: boolean;
  buddy?: CatBuddy | null;
  travel?: CatTravel | null;
  streak?: CatStreak | null;
  error?: string;
}

export interface TravelStatusResult {
  fetched_at: number;
  accounts: TravelStatusEntry[];
}

export interface TaskEvent {
  kind: string; // travel | activity
  uid: string;
  msg: string;
  at: number;
}

export interface TaskFeedResult {
  events: TaskEvent[];
  travel_running: boolean;
  activity_running: boolean;
}

export interface ModelCoolEntry {
  uid: string;
  nickname?: string;
  until: string;
}

export interface ModelLimitItem {
  model: string;
  cooled: ModelCoolEntry[];
  total: number;
  available: number;
}

export interface AccountCoolItem {
  uid: string;
  nickname: string;
  reason: string;
  until: string;
}

export interface LimitsResult {
  models: ModelLimitItem[];
  account_cooling: AccountCoolItem[];
}

export interface GrowthTask {
  task_code: string;
  title?: string;
  description?: string;
  task_desc?: string;
  credit?: number;
  energy?: number;
  task_type?: string;
  locked?: boolean;
  target: number;
  current: number;
  accept_status?: string;
  status?: string;
  claimable?: boolean;
  claimed?: boolean;
}

export interface TaskActionMeta {
  task_code: string;
  desc: string;
  attempt?: boolean;
}

export interface TasksListResult {
  tasks?: GrowthTask[];
  actions: TaskActionMeta[];
}

export interface SchoolTask {
  task_code: string;
  title?: string;
  description?: string;
  status?: string;
  progress: number;
  target_count: number;
  task_type?: string;
  reward_credit?: number;
  mode?: "manual" | "share" | "report" | "unknown" | string;
  note?: string;
  report_kind?: string;
}

export interface SchoolListResult {
  uid: string;
  nickname?: string;
  in_period: boolean;
  tasks: SchoolTask[];
}

export interface SchoolRunResult {
  ok: boolean;
  skipped?: boolean;
  message: string;
  in_period?: boolean;
  stats?: Record<string, number>;
  lines?: string[];
}

export interface SchoolChance {
  balance: number;
  total_earned: number;
  voucher_won?: boolean;
  lottery_limit?: number;
}

export interface SchoolPrize {
  prize_code: string;
  label?: string;
}

export interface LotteryStreakStatus {
  chances: number;
  error?: string;
}

export interface LotterySchoolStatus {
  in_period: boolean;
  chance?: SchoolChance;
  prizes?: SchoolPrize[];
  error?: string;
}

export interface LotteryStatusResult {
  uid: string;
  nickname?: string;
  streak: LotteryStreakStatus;
  school: LotterySchoolStatus;
  error?: string;
}

export interface LotteryStatusAllResult {
  accounts: LotteryStatusResult[];
}

export interface LotteryDrawResult {
  ok: boolean;
  uid?: string;
  kind?: string;
  message?: string;
  streak?: { ok?: boolean; message?: string; drawn?: number };
  school?: { ok?: boolean; message?: string; drawn?: number; credit?: number };
}

export interface TaskAutoResult {
  ok: boolean;
  skipped?: boolean;
  message: string;
  progress_before?: string;
  progress_after?: string;
  claimed?: boolean;
  credit?: number;
  energy?: number;
}

export interface UsageAggRow {
  key: string;
  credit: number;
  requests: number;
}

export interface UsageExpiringRow {
  nickname: string;
  name: string;
  remain: number;
  expire_at: string;
}

export interface UsageStatsResult {
  days: number;
  fetched_at: number;
  daily: UsageAggRow[];
  models: UsageAggRow[];
  accounts: UsageAggRow[];
  tokens: UsageAggRow[];
  expiring: UsageExpiringRow[];
  errors?: string[];
}

export interface FirewallEvent {
  at: number;
  rule: string;
  uid?: string;
  nick?: string;
  model?: string;
  snippet?: string;
  content?: string;
  match?: string;
  observe?: boolean;
  keyword?: string;
  verdict?: string;
  reason?: string;
  entry?: string;
  judge?: string;
}

export interface FirewallStatsResult {
  enabled: boolean;
  total: number;
  today: number;
  events: FirewallEvent[];
  rules: { rule: string; count: number }[];
}

export interface ReqLogEntry {
  time: string;
  model: string;
  channel: string;
  uid: string;
  status: number;
  stream: boolean;
  ttfb_ms: number;
  total_ms: number;
  in_tokens: number;
  out_tokens: number;
  cached_tokens: number;
  credit: number;
  body_file?: string;
}

export interface ReqLogPageResult {
  logs: ReqLogEntry[];
  total: number;
  page: number;
  size: number;
}

export interface FirewallPageResult {
  events: FirewallEvent[];
  total: number;
  page: number;
  size: number;
}
