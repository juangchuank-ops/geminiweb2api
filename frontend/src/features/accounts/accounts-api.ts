import { apiRequest } from "@/shared/api/client";

export type AccountStatus = "active" | "cooldown" | "disabled" | "invalid";

/**
 * A cookie account carries a pasted session; a guest carries nothing at all.
 *
 * The distinction is recorded rather than inferred from an empty cookie, because
 * they are different things: a guest is a deliberate choice that works, while a
 * cookie account with no cookie is a broken record.
 */
export type AccountKind = "cookie" | "guest";

export type AccountQuota = {
  syncedAt: string;
  /** The app shell loaded. */
  available: boolean;
  /**
   * A real session came back. A guest session is available but not
   * authenticated, and that difference is the only way to see an expired cookie
   * from the console while every request keeps succeeding.
   */
  authenticated: boolean;
  latencyMs: number;
  note: string;
};

/**
 * Outcome of the last cookie rotation. "" means it has never been tried, which
 * is a distinct state from "failed" and worth showing differently.
 *
 * "throttled" is not a problem with the account: the rotation endpoint refused
 * because it was called too often, and the account is fine.
 *
 * "invalid" means the primary cookie is gone. The account has to be re-added
 * from a browser; retrying cannot help.
 */
export type AccountRefreshStatus = "" | "ok" | "failed" | "throttled" | "skipped" | "invalid";

export type AccountDTO = {
  id: string;
  name: string;
  kind: AccountKind;
  identifier: string;
  /**
   * Which signed-in Google account to act as: 0 is the default, N means /u/N.
   * Getting it wrong does not error — it quietly answers as a different account.
   */
  authUser: number;
  model: string;
  baseURL: string;
  group: string;
  remark: string;
  status: AccountStatus;
  enabled: boolean;
  priority: number;
  maxConcurrent: number;
  inflight: number;
  cooldownUntil: string;
  failCount: number;
  successCount: number;
  lastUsedAt: string;
  lastError: string;
  createdAt: string;
  updatedAt: string;
  quota: AccountQuota | null;
  refreshAt: string;
  refreshStatus: AccountRefreshStatus;
  refreshError: string;
  refreshFailures: number;
  buildLabel: string;
  language: string;
  /** Whether a rotation has ever succeeded: "cookie pasted" vs "cookie alive". */
  hasPsidts: boolean;
  /** Display-only rendering of the credential. */
  cookieMasked: string;
  /** Which cookies the paste actually contained. */
  cookieNames: string[];
};

export type AccountSummary = {
  total: number;
  active: number;
  cooldown: number;
  disabled: number;
  invalid: number;
  routable: number;
};

export type AccountListResult = {
  items: AccountDTO[];
  total: number;
  page: number;
  pageSize: number;
  summary: AccountSummary;
};

export type AccountQuery = {
  page: number;
  pageSize: number;
  search?: string;
  status?: string;
  kind?: string;
  group?: string;
  sortBy?: string;
  sortOrder?: "asc" | "desc";
};

/** Fields the console may write. The backend derives whatever is omitted. */
export type AccountInput = {
  name?: string;
  /** The whole cookie string, pasted from a browser. */
  cookie?: string;
  identifier?: string;
  authUser?: number;
  model?: string;
  baseURL?: string;
  group?: string;
  remark?: string;
  priority?: number;
  maxConcurrent?: number;
  enabled?: boolean;
  kind?: AccountKind;
};

export function listAccounts(query: AccountQuery): Promise<AccountListResult> {
  const params = new URLSearchParams();
  params.set("page", String(query.page));
  params.set("pageSize", String(query.pageSize));
  if (query.search) params.set("search", query.search);
  if (query.status) params.set("status", query.status);
  if (query.kind) params.set("kind", query.kind);
  if (query.group) params.set("group", query.group);
  if (query.sortBy) params.set("sortBy", query.sortBy);
  if (query.sortOrder) params.set("sortOrder", query.sortOrder);
  return apiRequest<AccountListResult>(`/admin/api/accounts?${params.toString()}`);
}

export function createAccount(payload: AccountInput): Promise<{ account: AccountDTO }> {
  return apiRequest("/admin/api/accounts", { method: "POST", body: payload });
}

export function updateAccount(id: string, payload: AccountInput): Promise<{ account: AccountDTO }> {
  return apiRequest(`/admin/api/accounts/${id}`, { method: "PATCH", body: payload });
}

export function deleteAccount(id: string): Promise<void> {
  return apiRequest(`/admin/api/accounts/${id}`, { method: "DELETE" });
}

export type BatchAction =
  | { action: "enable" | "disable" | "delete" | "clearCooldown" | "probe"; ids: string[] }
  | { action: "concurrency"; ids: string[]; maxConcurrent: number };

export function batchAccounts(payload: BatchAction): Promise<{
  updated?: number;
  deleted?: number;
  succeeded?: number;
  failed?: number;
}> {
  return apiRequest("/admin/api/accounts/batch", { method: "POST", body: payload });
}

/**
 * Bulk import. `cookies` takes one account per line in any of the shapes the
 * backend accepts, and `json` is an exported account file.
 */
export function importAccounts(payload: {
  cookies?: string;
  json?: unknown;
}): Promise<{ created: number; updated: number; failed: number; errors?: string[] }> {
  return apiRequest("/admin/api/accounts/import", { method: "POST", body: payload });
}

export function exportAccounts(limit = 10000): Promise<{ accounts: unknown[]; count: number }> {
  return apiRequest(`/admin/api/accounts/export?limit=${limit}`);
}

export function probeAccount(id: string): Promise<{ ok: boolean; message: string; account: AccountDTO }> {
  return apiRequest(`/admin/api/accounts/${id}/probe`, { method: "POST" });
}

export function probeAllAccounts(): Promise<{ healthy: number; unhealthy: number }> {
  return apiRequest("/admin/api/accounts/probe-all", { method: "POST" });
}

export function cleanupAccounts(statuses: string[]): Promise<{ deleted: number }> {
  return apiRequest("/admin/api/accounts/cleanup", { method: "POST", body: { statuses } });
}

export function listAccountGroups(): Promise<{ groups: string[] }> {
  return apiRequest("/admin/api/accounts/groups");
}

// ------------------------------------------------------------ cookie rotation

export type RefreshOutcome = {
  accountId: string;
  accountName: string;
  status: AccountRefreshStatus;
  error: string;
  at: string;
};

export type RefreshSummary = {
  startedAt: string;
  finishedAt: string;
  total: number;
  ok: number;
  failed: number;
  throttled: number;
  skipped: number;
  invalid: number;
  outcomes: RefreshOutcome[] | null;
};

export type RefreshOverview = {
  enabled: boolean;
  intervalMin: number;
  gapSeconds: number;
  timeoutSec: number;
  retireAfter: number;
  /** Account count per refresh status, plus "pending" for never-tried. */
  counts: Record<string, number>;
  nextRunAt: string;
  lastSummary: RefreshSummary;
  totalAccounts: number;
};

export function getRefreshOverview(): Promise<RefreshOverview> {
  return apiRequest("/admin/api/refresh");
}

/**
 * Rotates every eligible account now, ignoring the per-account interval. This is
 * the button an operator presses because something is already wrong, so it must
 * not answer "nothing was due".
 */
export function runRefresh(): Promise<{ summary: RefreshSummary }> {
  return apiRequest("/admin/api/refresh/run", { method: "POST" });
}

/**
 * Rotates one account's cookie.
 *
 * A rejected rotation answers 502 rather than 200 with a sad body: the console
 * keys its toast off the status, and "refreshed" is the one thing this call must
 * never appear to have done when it did not.
 */
export function refreshAccountCookie(id: string): Promise<{ outcome: RefreshOutcome; account: AccountDTO }> {
  return apiRequest(`/admin/api/accounts/${id}/refresh`, { method: "POST" });
}

export function refreshAllCookies(): Promise<{ summary: RefreshSummary }> {
  return apiRequest("/admin/api/accounts/refresh-all", { method: "POST" });
}
