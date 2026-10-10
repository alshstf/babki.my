import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { Bell, BellOff } from "lucide-react";
import { Button } from "@/components/ui/button";
import { pushState, turnOff, turnOn, type PushState } from "@/api/push";

// PushToggle turns the payment reminders on or off for this device (decision
// Р-27). Where the browser has no push it says how to get it.
export function PushToggle() {
  const { t } = useTranslation();
  const [state, setState] = useState<PushState | null>(null);
  const [busy, setBusy] = useState(false);
  const [failed, setFailed] = useState(false);
  useEffect(() => {
    let live = true;
    pushState()
      .then((s) => live && setState(s))
      .catch(() => live && setState("unsupported"));
    return () => {
      live = false;
    };
  }, []);
  if (state === null) return null;
  const run = async (fn: () => Promise<PushState>) => {
    setBusy(true);
    setFailed(false);
    try {
      setState(await fn());
    } catch {
      setFailed(true);
    } finally {
      setBusy(false);
    }
  };
  return (
    <div className="grid gap-1 text-sm" data-testid="push-toggle">
      {state === "unsupported" && <p className="text-muted-foreground">{t("push.unsupported")}</p>}
      {state === "denied" && <p className="text-muted-foreground">{t("push.denied")}</p>}
      {state === "off" && (
        <Button size="sm" variant="outline" className="justify-self-start" disabled={busy} onClick={() => void run(turnOn)}>
          <Bell className="size-4" />
          {t("push.turnOn")}
        </Button>
      )}
      {state === "on" && (
        <div className="flex flex-wrap items-center gap-2">
          <span className="text-muted-foreground">{t("push.on")}</span>
          <Button size="sm" variant="ghost" disabled={busy} onClick={() => void run(turnOff)}>
            <BellOff className="size-4" />
            {t("push.turnOff")}
          </Button>
        </div>
      )}
      {failed && <p className="text-xs text-red-700 dark:text-red-400">{t("push.failed")}</p>}
    </div>
  );
}
