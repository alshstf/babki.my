import { useTranslation } from "react-i18next";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Checkbox } from "@/components/ui/checkbox";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { PushToggle } from "@/components/push-toggle";
import { usePushSettings, useSetPushSettings, type PushSettings } from "@/api/push";

const HOURS = Array.from({ length: 24 }, (_, h) => h);

// kindLabel is a reminder's kind as the settings name it; the server owns
// the kinds, and one this screen does not know yet shows as it is.
function kindLabel(t: (key: string) => string, kind: string): string {
  const labels: Record<string, string> = {
    card: t("notifications.kinds.card"),
    budget: t("notifications.kinds.budget"),
    goal: t("notifications.kinds.goal"),
    storage: t("notifications.kinds.storage"),
    payout: t("notifications.kinds.payout"),
    declared: t("notifications.kinds.declared"),
    sync: t("notifications.kinds.sync"),
    reconcile: t("notifications.kinds.reconcile"),
    stale: t("notifications.kinds.stale"),
  };
  return labels[kind] ?? kind;
}

// NotificationsSection is a member's reminders (decision Р-61 Б): on this
// device or not, which kinds come, and the quiet hours when none do.
export function NotificationsSection() {
  const { t } = useTranslation();
  const q = usePushSettings();
  const save = useSetPushSettings();
  const data = q.data;
  const put = (next: PushSettings) =>
    save.mutate({ off: next.kinds.filter((k) => !k.on).map((k) => k.kind), quiet_from: next.quiet_from, quiet_to: next.quiet_to });
  return (
    <Card className="max-w-md" data-testid="notifications">
      <CardHeader>
        <CardTitle>{t("notifications.title")}</CardTitle>
      </CardHeader>
      <CardContent className="grid gap-4">
        <PushToggle />
        {q.isError && (
          <Alert variant="destructive">
            <AlertDescription>{t("app.error")}</AlertDescription>
          </Alert>
        )}
        {data && (
          <>
            <div className="grid gap-2">
              <p className="text-sm font-medium">{t("notifications.kindsTitle")}</p>
              {data.kinds.map((k) => (
                <div key={k.kind} className="flex items-start gap-2">
                  <Checkbox
                    id={`push-kind-${k.kind}`}
                    checked={k.on}
                    disabled={save.isPending}
                    onCheckedChange={(on) => put({ ...data, kinds: data.kinds.map((x) => (x.kind === k.kind ? { ...x, on: on === true } : x)) })}
                  />
                  <Label htmlFor={`push-kind-${k.kind}`} className="font-normal leading-snug">
                    {kindLabel(t, k.kind)}
                  </Label>
                </div>
              ))}
            </div>
            <div className="grid gap-2">
              <p className="text-sm font-medium">{t("notifications.quietTitle")}</p>
              <div className="flex flex-wrap items-center gap-2 text-sm">
                <span>{t("notifications.quietFrom")}</span>
                <HourSelect id="quiet-from" label={t("notifications.quietFrom")} value={data.quiet_from} disabled={save.isPending} onChange={(h) => put({ ...data, quiet_from: h })} />
                <span>{t("notifications.quietTo")}</span>
                <HourSelect id="quiet-to" label={t("notifications.quietTo")} value={data.quiet_to} disabled={save.isPending} onChange={(h) => put({ ...data, quiet_to: h })} />
              </div>
              <p className="text-xs text-muted-foreground">
                {data.quiet_from === data.quiet_to ? t("notifications.quietNever") : t("notifications.quietHint")}
              </p>
            </div>
            {save.isError && <p className="text-xs text-red-700 dark:text-red-400">{t("notifications.failed")}</p>}
          </>
        )}
      </CardContent>
    </Card>
  );
}

function HourSelect({ id, label, value, disabled, onChange }: { id: string; label: string; value: number; disabled: boolean; onChange: (h: number) => void }) {
  return (
    <Select value={String(value)} disabled={disabled} onValueChange={(v) => onChange(Number(v))}>
      <SelectTrigger id={id} aria-label={label} className="w-24">
        <SelectValue />
      </SelectTrigger>
      <SelectContent>
        {HOURS.map((h) => (
          <SelectItem key={h} value={String(h)}>
            {`${String(h).padStart(2, "0")}:00`}
          </SelectItem>
        ))}
      </SelectContent>
    </Select>
  );
}
