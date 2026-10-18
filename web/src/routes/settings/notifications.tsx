import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Checkbox } from "@/components/ui/checkbox";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { PushToggle } from "@/components/push-toggle";
import { usePushSettings, useSetPushSettings, type PushSettings } from "@/api/push";
import {
  useDeleteTelegramBot,
  useLinkTelegram,
  useSetTelegramBot,
  useTelegram,
  useUnlinkTelegram,
  type TelegramLink,
  type TelegramStatus,
} from "@/api/telegram";
import { formatDateTime } from "@/lib/dates";

const HOURS = Array.from({ length: 24 }, (_, h) => h);

// kindLabel is a reminder's kind as the settings name it; the server owns
// the kinds, and one this screen does not know yet shows as it is.
function kindLabel(t: (key: string) => string, kind: string): string {
  const labels: Record<string, string> = {
    card: t("notifications.kinds.card"),
    loyalty: t("notifications.kinds.loyalty"),
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
// device or not, Telegram as the second channel, which kinds come where, and
// the quiet hours when none do.
export function NotificationsSection() {
  const { t } = useTranslation();
  const [link, setLink] = useState<TelegramLink | null>(null);
  const q = usePushSettings();
  const tg = useTelegram(link !== null);
  const save = useSetPushSettings();
  const data = q.data;
  const bot = tg.data?.bot ?? null;
  const put = (next: PushSettings) =>
    save.mutate({
      off: next.kinds.filter((k) => !k.on).map((k) => k.kind),
      telegram_off: next.kinds.filter((k) => !k.telegram).map((k) => k.kind),
      amounts: next.amounts,
      quiet_from: next.quiet_from,
      quiet_to: next.quiet_to,
    });
  return (
    <Card className="max-w-md" data-testid="notifications">
      <CardHeader>
        <CardTitle>{t("notifications.title")}</CardTitle>
      </CardHeader>
      <CardContent className="grid gap-4">
        <PushToggle />
        {tg.data && <TelegramBlock status={tg.data} link={tg.data.linked ? null : link} onLink={setLink} />}
        {(q.isError || tg.isError) && (
          <Alert variant="destructive">
            <AlertDescription>{t("app.error")}</AlertDescription>
          </Alert>
        )}
        {data && (
          <>
            <div className="grid gap-2">
              <div className="grid grid-cols-[1fr_auto_auto] items-end gap-x-3 text-xs text-muted-foreground">
                <span className="text-sm font-medium text-foreground">{t("notifications.kindsTitle")}</span>
                <span>{t("notifications.devices")}</span>
                <span>{bot && t("notifications.telegram.column")}</span>
              </div>
              {data.kinds.map((k) => (
                <div key={k.kind} className="grid grid-cols-[1fr_auto_auto] items-start gap-x-3">
                  <Label htmlFor={`push-kind-${k.kind}`} className="font-normal leading-snug">
                    {kindLabel(t, k.kind)}
                  </Label>
                  <Checkbox
                    id={`push-kind-${k.kind}`}
                    className="mx-auto"
                    checked={k.on}
                    disabled={save.isPending}
                    onCheckedChange={(on) => put({ ...data, kinds: data.kinds.map((x) => (x.kind === k.kind ? { ...x, on: on === true } : x)) })}
                  />
                  {bot ? (
                    <Checkbox
                      aria-label={`${kindLabel(t, k.kind)} — ${t("notifications.telegram.column")}`}
                      className="mx-auto"
                      checked={k.telegram}
                      disabled={save.isPending}
                      onCheckedChange={(on) => put({ ...data, kinds: data.kinds.map((x) => (x.kind === k.kind ? { ...x, telegram: on === true } : x)) })}
                    />
                  ) : (
                    <span />
                  )}
                </div>
              ))}
            </div>
            {bot && (
              <div className="grid gap-1">
                <div className="flex items-start gap-2">
                  <Checkbox id="push-amounts" checked={data.amounts} disabled={save.isPending} onCheckedChange={(on) => put({ ...data, amounts: on === true })} />
                  <Label htmlFor="push-amounts" className="font-normal leading-snug">
                    {t("notifications.telegram.amounts")}
                  </Label>
                </div>
                <p className="text-xs text-muted-foreground">{t("notifications.telegram.amountsHint")}</p>
              </div>
            )}
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

// TelegramBlock is the space's bot — the owner gives or takes it — and the
// member's chat: tied, or a link to tie it with.
function TelegramBlock({ status, link, onLink }: { status: TelegramStatus; link: TelegramLink | null; onLink: (l: TelegramLink | null) => void }) {
  const { t } = useTranslation();
  const [token, setToken] = useState("");
  const setBot = useSetTelegramBot();
  const deleteBot = useDeleteTelegramBot();
  const makeLink = useLinkTelegram();
  const unlink = useUnlinkTelegram();
  const bot = status.bot;
  return (
    <div className="grid gap-2 text-sm" data-testid="telegram">
      <p className="font-medium">{t("notifications.telegram.title")}</p>
      {!status.available ? (
        <p className="text-muted-foreground">{t("notifications.telegram.unavailable")}</p>
      ) : !bot ? (
        status.can_manage ? (
          <div className="grid gap-2">
            <p className="text-xs text-muted-foreground">{t("notifications.telegram.botHint")}</p>
            <div className="flex gap-2">
              <Input
                aria-label={t("notifications.telegram.token")}
                placeholder={t("notifications.telegram.token")}
                autoComplete="off"
                value={token}
                onChange={(e) => setToken(e.target.value)}
              />
              <Button size="sm" disabled={token.trim() === "" || setBot.isPending} onClick={() => setBot.mutate(token.trim(), { onSuccess: () => setToken("") })}>
                {t("common.save")}
              </Button>
            </div>
            {setBot.isError && <p className="text-xs text-red-700 dark:text-red-400">{t("notifications.telegram.botFailed")}</p>}
          </div>
        ) : (
          <p className="text-muted-foreground">{t("notifications.telegram.noBot")}</p>
        )
      ) : (
        <div className="grid gap-2">
          <div className="flex flex-wrap items-center gap-2">
            <span className="text-muted-foreground">{t("notifications.telegram.bot", { name: bot.username, last4: bot.token_last4 })}</span>
            {status.can_manage && (
              <Button size="sm" variant="ghost" disabled={deleteBot.isPending} onClick={() => deleteBot.mutate()}>
                {t("notifications.telegram.removeBot")}
              </Button>
            )}
          </div>
          {status.linked ? (
            <div className="flex flex-wrap items-center gap-2">
              <span>{t("notifications.telegram.linked")}</span>
              <Button size="sm" variant="ghost" disabled={unlink.isPending} onClick={() => unlink.mutate()}>
                {t("notifications.telegram.unlink")}
              </Button>
            </div>
          ) : link ? (
            <div className="grid gap-1">
              <a href={link.url} target="_blank" rel="noreferrer" className="underline" data-testid="telegram-link">
                {t("notifications.telegram.open", { name: bot.username })}
              </a>
              <p className="text-xs text-muted-foreground">{t("notifications.telegram.linkHint", { until: formatDateTime(link.expires_at) })}</p>
            </div>
          ) : (
            <Button size="sm" variant="outline" className="justify-self-start" disabled={makeLink.isPending} onClick={() => makeLink.mutate(undefined, { onSuccess: onLink })}>
              {t("notifications.telegram.link")}
            </Button>
          )}
          {(makeLink.isError || unlink.isError || deleteBot.isError) && <p className="text-xs text-red-700 dark:text-red-400">{t("notifications.failed")}</p>}
        </div>
      )}
    </div>
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
