import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Dialog, DialogContent, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { ApiError } from "@/api/operations";
import { useChangePassword, useSignOutElsewhere } from "@/api/session";
import { useOnOpen } from "@/lib/use-on-open";

// The signed-in member's own password and sessions: a new password, which ends
// every other session, and signing out everywhere else without changing it.
export function PasswordDialog({ open, onOpenChange }: { open: boolean; onOpenChange: (open: boolean) => void }) {
  const { t } = useTranslation();
  const change = useChangePassword();
  const elsewhere = useSignOutElsewhere();
  const [current, setCurrent] = useState("");
  const [password, setPassword] = useState("");
  const [repeat, setRepeat] = useState("");

  useOnOpen(open, () => {
    setCurrent("");
    setPassword("");
    setRepeat("");
    change.reset();
    elsewhere.reset();
  });

  // The server's rule (family.MinPasswordRunes); the contract test holds the
  // two together.
  const longEnough = password.length >= 8;
  const valid = current !== "" && longEnough && password === repeat;
  const changeError =
    change.error instanceof ApiError
      ? change.error.status === 400
        ? t("password.wrongCurrent")
        : change.error.status === 429
          ? t("password.locked")
          : t("app.error")
      : change.isError
        ? t("app.error")
        : null;

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-sm">
        <DialogHeader>
          <DialogTitle>{t("password.title")}</DialogTitle>
        </DialogHeader>
        <form
          className="grid gap-3"
          onSubmit={(e) => {
            e.preventDefault();
            if (valid) change.mutate({ current_password: current, new_password: password });
          }}
        >
          <div className="grid gap-2">
            <Label htmlFor="pw-current">{t("password.current")}</Label>
            <Input id="pw-current" type="password" autoComplete="current-password" maxLength={1024}
              value={current} onChange={(e) => setCurrent(e.target.value)} />
          </div>
          <div className="grid gap-2">
            <Label htmlFor="pw-new">{t("password.new")}</Label>
            <Input id="pw-new" type="password" autoComplete="new-password" maxLength={1024}
              value={password} onChange={(e) => setPassword(e.target.value)} />
            {password !== "" && !longEnough && <p className="text-xs text-red-500">{t("password.tooShort")}</p>}
          </div>
          <div className="grid gap-2">
            <Label htmlFor="pw-repeat">{t("password.repeat")}</Label>
            <Input id="pw-repeat" type="password" autoComplete="new-password" maxLength={1024}
              value={repeat} onChange={(e) => setRepeat(e.target.value)} />
            {repeat !== "" && repeat !== password && <p className="text-xs text-red-500">{t("password.mismatch")}</p>}
          </div>
          <p className="text-xs text-muted-foreground">{t("password.hint")}</p>
          {changeError && (
            <Alert variant="destructive">
              <AlertDescription>{changeError}</AlertDescription>
            </Alert>
          )}
          {change.isSuccess && (
            <Alert>
              <AlertDescription>{t("password.changed")}</AlertDescription>
            </Alert>
          )}
          <Button type="submit" disabled={!valid || change.isPending}>
            {t("password.save")}
          </Button>
        </form>
        <div className="grid gap-2 border-t pt-3">
          <p className="text-xs text-muted-foreground">{t("password.elsewhereHint")}</p>
          <Button variant="outline" onClick={() => elsewhere.mutate()} disabled={elsewhere.isPending}>
            {t("password.elsewhere")}
          </Button>
          {elsewhere.isSuccess && <p className="text-xs text-muted-foreground">{t("password.elsewhereDone")}</p>}
          {elsewhere.isError && <p className="text-xs text-red-500">{t("app.error")}</p>}
        </div>
      </DialogContent>
    </Dialog>
  );
}
