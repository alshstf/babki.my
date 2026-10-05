import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { SessionNotKept, SignInLocked, useLogin } from "@/api/session";
import { isUnauthorized } from "@/api/operations";

export function LoginPage() {
  const { t } = useTranslation();
  const login = useLogin();
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");

  return (
    <div className="min-h-screen flex items-center justify-center bg-background p-4">
      <Card className="w-full max-w-sm">
        <CardHeader>
          <CardTitle className="text-2xl">{t("app.name")}</CardTitle>
          <p className="text-sm text-muted-foreground">{t("auth.loginTitle")}</p>
        </CardHeader>
        <CardContent>
          <form
            className="grid gap-4"
            onSubmit={(e) => {
              e.preventDefault();
              login.mutate({ username, password });
            }}
          >
            <div className="grid gap-2">
              <Label htmlFor="username">{t("auth.username")}</Label>
              <Input
                id="username"
                autoComplete="username"
                value={username}
                onChange={(e) => setUsername(e.target.value)}
              />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="password">{t("auth.password")}</Label>
              <Input
                id="password"
                type="password"
                autoComplete="current-password"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
              />
            </div>
            {login.isError && (
              // By the status the contract declares: 401 wrong credentials; 429 locked
              // after too many attempts, with the wait (SignInLockedNotice); anything
              // else (a dead connection, a broken server) says only that sign-in did not
              // happen. Literal t() keys for scripts/check-i18n.mjs.
              <Alert variant="destructive">
                <AlertDescription>
                  {login.error instanceof SignInLocked ? (
                    <SignInLockedNotice minutesLeft={login.error.minutesLeft} />
                  ) : login.error instanceof SessionNotKept ? (
                    t("auth.sessionNotKept")
                  ) : isUnauthorized(login.error) ? (
                    t("auth.invalidCredentials")
                  ) : (
                    t("auth.signInFailed")
                  )}
                </AlertDescription>
              </Alert>
            )}
            <Button
              type="submit"
              disabled={!username || !password || login.isPending}
            >
              {t("auth.signIn")}
            </Button>
          </form>
        </CardContent>
      </Card>
    </div>
  );
}

// SignInLockedNotice says the door is closed and for how long. The wait is the
// server's own figure; without one the sentence does not invent it.
function SignInLockedNotice({ minutesLeft }: { minutesLeft: number | null }) {
  const { t } = useTranslation();
  if (minutesLeft === null) return <>{t("auth.signInLocked")}</>;
  return <>{t("auth.signInLockedFor", { minutes: minutesLeft })}</>;
}
