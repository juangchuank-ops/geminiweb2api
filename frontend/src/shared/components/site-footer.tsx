import { useTranslation } from "react-i18next";

export function SiteFooter() {
  const { t } = useTranslation();
  return (
    <footer className="mx-auto flex w-full max-w-[1280px] flex-col gap-2 px-5 pb-8 pt-4 text-[11px] text-muted-foreground sm:flex-row sm:items-center sm:justify-between sm:px-8">
      <span>
        {t("appName")} · {t("appTagline")}
      </span>
      <a
        className="font-mono transition-colors hover:text-foreground"
        href="https://gemini.google.com/"
        target="_blank"
        rel="noreferrer"
      >
        upstream · https://gemini.google.com
      </a>
    </footer>
  );
}
