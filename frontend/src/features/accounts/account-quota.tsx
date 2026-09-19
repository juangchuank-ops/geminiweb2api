import { useTranslation } from "react-i18next";

import { Tooltip } from "@/components/ui/tooltip";
import type { AccountKind, AccountQuota } from "@/features/accounts/accounts-api";
import { cn } from "@/shared/lib/cn";
import { formatDuration, formatRelative, hasInstant } from "@/shared/lib/format";

/**
 * Probe result: reachability, and whether the session was actually accepted.
 *
 * The two are not the same question, and the difference is the whole point of
 * this column. `available` only means the app shell loaded, which it does for
 * anyone. `authenticated` means a real session came back. A Gemini cookie that
 * has expired does not fail — every request keeps succeeding on the guest
 * quota — so without this distinction an expired pool looks perfectly healthy
 * while answering as an anonymous visitor.
 *
 * `kind` is needed to read that correctly. An account that *is* a guest is
 * unauthenticated by definition, and calling that "your cookie expired" would
 * send the operator hunting for a credential the account was never meant to
 * have.
 */
export function AccountQuotaCell({ quota, kind }: { quota: AccountQuota | null; kind: AccountKind }) {
  const { t, i18n } = useTranslation();

  if (!quota) {
    return <span className="text-xs text-muted-foreground">{t("accounts.probeUnsynced")}</span>;
  }

  const guest = kind === "guest";
  // Only a *cookie* account falling back to the guest quota is a problem worth
  // flagging.
  const downgraded = !guest && quota.available && !quota.authenticated;
  const tone = !quota.available
    ? "text-destructive"
    : downgraded
      ? "text-amber-700 dark:text-amber-300"
      : quota.available
        ? "text-foreground"
        : "text-muted-foreground";
  const label = !quota.available
    ? t("accounts.probeFailed")
    : downgraded
      ? t("accounts.probeGuest")
      : guest
        ? t("accounts.probeGuestOk")
        : t("accounts.probeSucceeded");

  return (
    <div className="min-w-0 space-y-1">
      <div className="flex items-center gap-2">
        <span className={cn("text-xs", tone)}>{label}</span>
        {quota.latencyMs > 0 ? (
          <span className="text-[10px] text-muted-foreground tabular-nums">{formatDuration(quota.latencyMs)}</span>
        ) : null}
      </div>
      {downgraded ? (
        <p className="max-w-40 truncate text-[10px] text-amber-700 dark:text-amber-300" title={t("accounts.probeGuestHelp")}>
          {t("accounts.probeGuestHelp")}
        </p>
      ) : null}
      <Tooltip label={`${quota.note || "—"} · ${hasInstant(quota.syncedAt) ? formatRelative(quota.syncedAt, i18n.language) : ""}`}>
        <span className="block max-w-40 truncate text-[10px] text-muted-foreground">
          {hasInstant(quota.syncedAt) ? formatRelative(quota.syncedAt, i18n.language) : t("accounts.probeUnsynced")}
        </span>
      </Tooltip>
    </div>
  );
}
