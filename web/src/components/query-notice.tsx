import { useTranslation } from "react-i18next";
import { Alert, AlertDescription } from "@/components/ui/alert";
import type { QueryState } from "@/lib/query-state";

// What a screen shows INSTEAD of its content while it has none to show. Renders
// nothing once the state is "ready".
export function QueryGate({ state }: { state: QueryState }) {
  const { t } = useTranslation();
  switch (state) {
    case "ready":
      return null;
    case "loading":
      return <div className="text-muted-foreground">{t("app.loading")}</div>;
    case "offline":
      return (
        <div className="text-muted-foreground" data-testid="query-offline">
          {t("app.startupOffline")}
        </div>
      );
    case "failed":
      return (
        <Alert variant="destructive">
          <AlertDescription>{t("app.error")}</AlertDescription>
        </Alert>
      );
  }
}

// A line above content that is an earlier answer: the last refresh failed and
// the data stayed. Renders nothing when `show` is false.
export function RefreshFailedNotice({ show }: { show: boolean }) {
  const { t } = useTranslation();
  if (!show) return null;
  return (
    <div className="text-sm text-muted-foreground" role="status" data-testid="refresh-failed">
      {t("app.refreshFailed")}
    </div>
  );
}
