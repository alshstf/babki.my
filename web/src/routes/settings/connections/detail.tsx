import { useParams } from "@tanstack/react-router";
import { useTranslation } from "react-i18next";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { useSession } from "@/api/session";

// Placeholder for the connection screen a later task builds out in full (run
// log, reconcile snapshot against the broker, unparsed operations, the
// sync-now button). This task only wires the route — the wizard needs
// somewhere honest to land once a connection is created — and keeps the
// owner-only gate every other route under /settings already has.
export function ConnectionDetailPage() {
  const { t } = useTranslation();
  const { connectionId } = useParams({ from: "/app/settings/connections/$connectionId" });
  const { data: session } = useSession();
  const isOwner = session?.role === "owner";

  if (!isOwner) {
    return (
      <Alert>
        <AlertDescription>{t("settings.ownerOnly")}</AlertDescription>
      </Alert>
    );
  }

  return (
    <div className="grid gap-2">
      <h1 className="text-2xl font-bold">{t("connections.detail.title")}</h1>
      <p className="text-sm text-muted-foreground">{t("connections.detail.placeholder")}</p>
      <p className="text-xs text-muted-foreground">
        {t("connections.detail.idLabel", { id: connectionId })}
      </p>
    </div>
  );
}
