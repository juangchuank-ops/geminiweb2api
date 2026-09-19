import { apiRequest } from "@/shared/api/client";

export type SettingsDTO = {
  server: {
    addr: string;
    maxConcurrentRequests: number;
    adminUsername: string;
  };
  upstream: {
    baseURL: string;
    language: string;
    defaultModel: string;
    requestTimeoutSec: number;
    streamIdleTimeoutSec: number;
    proxy: string;
    userAgent: string;
    /**
     * Where Google reissues __Secure-1PSIDTS. It lives on accounts.google.com
     * rather than on the app host, and a deployment that reaches Gemini through
     * a mirror has to point both at the same place.
     */
    rotateURL: string;
  };
  routing: {
    strategy: "least_inflight" | "round_robin" | "priority" | "random";
    cooldownBaseSec: number;
    cooldownMaxSec: number;
    maxAttempts: number;
    capacityWaitSec: number;
    stickyTTLSec: number;
    preferIdle: boolean;
  };
  audit: {
    retentionDays: number;
    maxRecords: number;
    recordBody: boolean;
    bodyLimitBytes: number;
  };
  media: {
    generatedDir: string;
    publicBaseURL: string;
    maxTotalSizeMB: number;
    autoDownload: boolean;
  };
  /**
   * The cookie keep-alive sweep.
   *
   * This is not a convenience: __Secure-1PSIDTS expires within hours, and a
   * session whose PSIDTS has lapsed is answered as a *guest* — the request
   * succeeds, the quota is smaller, and nothing in the response says so.
   */
  refresh: {
    enabled: boolean;
    intervalMin: number;
    gapSeconds: number;
    timeoutSec: number;
    retireAfterFailures: number;
  };
  about: {
    version: string;
    buildTime: string;
    dataDir: string;
    upstreamURL: string;
  };
};

export function getSettings(): Promise<SettingsDTO> {
  return apiRequest<SettingsDTO>("/admin/api/settings");
}

export function saveSettings(payload: Partial<SettingsDTO> & { adminPassword?: string }): Promise<SettingsDTO> {
  return apiRequest("/admin/api/settings", { method: "PUT", body: payload });
}
