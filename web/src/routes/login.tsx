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
import { useLogin } from "@/api/session";
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
              // Two sentences, chosen by the status the contract declares. 401
              // is the only refusal this endpoint publishes (see
              // /api/v1/auth/login in the API contract) and it is the one case
              // where naming the cause is naming what the server named.
              // Anything else — a dead connection, which useLogin now lets
              // through rather than holding silently, or a server that broke
              // its own contract — is a failure whose cause this screen has not
              // been told, so it says what it does know: the sign-in did not
              // happen, and pressing the button again is worth doing.
              //
              // Written as two literal-key branches rather than t(cond ? a : b)
              // so both keys stay verifiable by scripts/check-i18n.mjs, which
              // only reads literals.
              <Alert variant="destructive">
                <AlertDescription>
                  {isUnauthorized(login.error)
                    ? t("auth.invalidCredentials")
                    : t("auth.signInFailed")}
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
