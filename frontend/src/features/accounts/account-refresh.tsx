import { useTranslation } from "react-i18next";

import { Tooltip } from "@/components/ui/tooltip";
import type { AccountDTO, AccountRefreshStatus } from "@/features/accounts/accounts-api";
import { cn } from "@/shared/lib/cn";
import { formatRelative, hasInstant } from "@/shared/lib/format";

/**
 * Tones are keyed off the outcome, not off "did it work".
 *
 * "throttled" is deliberately amber rather than red: the rotation endpoint
 * refused because it was called too often, and the account itself is fine.
 * Painting it as a failure is how an operator ends up deleting a healthy
 * account. "skipped" is grey because nothing was attempted.
 */
const REFRESH_TONE: Record<AccountRefreshStatus, string> = {
  "": "bg-muted text-muted-foreground",
  ok: "bg-emerald-500/10 text-emerald-700 dark:text-emerald-300",
  failed: "bg-red-500/10 text-red-700 dark:text-red-300",
  throttled: "bg-amber-500/10 text-amber-700 dark:text-amber-300",
  skipped: "bg-muted text-muted-foreground",
  invalid: "bg-red-500/10 text-red-700 dark:text-red-300",
};

const REFRESH_LABEL: Record<AccountRefreshStatus, string> = {
  "": "rotatePending",
  ok: "rotateOk",
  failed: "rotateFailed",
  throttled: "rotateThrottled",
  skipped: "rotateSkipped",
  invalid: "rotateInvalid",
};

export function AccountRefreshCell({ account }: { account: AccountDTO }) {
  const { t, i18n } = useTranslation();
  const status = account.refreshStatus;

  const detail = account.refreshError || t(`accounts.${REFRESH_LABEL[status]}`);

  return (
    <div className="flex flex-col items-center gap-1">
      <Tooltip label={detail}>
        <span className={cn("inline-flex h-5 items-center rounded-full px-2 text-[11px] leading-none", REFRESH_TONE[status])}>
          {t(`accounts.${REFRESH_LABEL[status]}`)}
        </span>
      </Tooltip>
      {hasInstant(account.refreshAt) ? (
        <span className="text-[10px] text-muted-foreground tabular-nums">
          {formatRelative(account.refreshAt, i18n.language)}
        </span>
      ) : (
        <span className="text-[10px] text-muted-foreground">{t("accounts.rotateNever")}</span>
      )}
      {account.refreshFailures > 0 ? (
        <span className="text-[10px] text-destructive tabular-nums">
          {t("accounts.rotateFailures", { count: account.refreshFailures })}
        </span>
      ) : null}
    </div>
  );
}

/**
 * Header text for the refresh column.
 *
 * The explanation lives here rather than in a settings page because the reason
 * this column exists is non-obvious: the cookie it rotates expires in hours,
 * and when it does the requests do not fail — they silently fall back to the
 * guest quota. An operator who does not know that will read a green pool as
 * healthy right up until they wonder why answers got worse.
 */
export function AccountRefreshHead() {
  const { t } = useTranslation();
  return (
    <Tooltip label={t("accounts.rotateHelp")}>
      <span>{t("accounts.rotateColumn")}</span>
    </Tooltip>
  );
}
