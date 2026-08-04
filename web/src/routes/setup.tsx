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
import { isConflict } from "@/api/operations";
import { useSetup } from "@/api/session";

export function SetupPage() {
  const { t } = useTranslation();
  const setup = useSetup();
  const [spaceName, setSpaceName] = useState("");
  const [displayName, setDisplayName] = useState("");
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");

  const valid =
    spaceName && displayName && /^[a-z0-9_]{3,32}$/.test(username) &&
    password.length >= 8;

  return (
    <div className="min-h-screen flex items-center justify-center bg-background p-4">
      <Card className="w-full max-w-md">
        <CardHeader>
          <CardTitle className="text-2xl">{t("setup.title")}</CardTitle>
          <p className="text-sm text-muted-foreground">{t("setup.subtitle")}</p>
        </CardHeader>
        <CardContent>
          <form
            className="grid gap-4"
            onSubmit={(e) => {
              e.preventDefault();
              setup.mutate({
                space_name: spaceName,
                display_name: displayName,
                username,
                password,
              });
            }}
          >
            <div className="grid gap-2">
              <Label htmlFor="space">{t("setup.spaceName")}</Label>
              <Input
                id="space"
                placeholder={t("setup.spaceNamePlaceholder")}
                value={spaceName}
                onChange={(e) => setSpaceName(e.target.value)}
              />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="display">{t("setup.displayName")}</Label>
              <Input
                id="display"
                value={displayName}
                onChange={(e) => setDisplayName(e.target.value)}
              />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="username">{t("auth.username")}</Label>
              <Input
                id="username"
                placeholder="a-z, 0-9, _"
                value={username}
                onChange={(e) => setUsername(e.target.value)}
              />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="password">{t("auth.password")}</Label>
              <Input
                id="password"
                type="password"
                placeholder={t("setup.passwordHint")}
                value={password}
                onChange={(e) => setPassword(e.target.value)}
              />
            </div>
            {setup.isError && (
              <Alert variant="destructive">
                {/* By status, not by the server's sentence. 409 is what this
                    endpoint answers when the instance already has a user
                    (ErrAlreadySetUp, mapped in internal/family/http.go), and it
                    is a documented answer for it (api/openapi.yaml); the
                    English prose beside it is promised nowhere and can be
                    reworded at any time, silently turning this into the generic
                    message. There is one other way to a 409 here — two setups
                    racing, where the loser's username was taken a moment ago by
                    the winner — and the sentence holds there too: the instance
                    is set up, and reloading to sign in is exactly what to do. */}
                <AlertDescription>
                  {isConflict(setup.error)
                    ? t("setup.alreadySetUp")
                    : t("setup.genericError")}
                </AlertDescription>
              </Alert>
            )}
            <Button type="submit" disabled={!valid || setup.isPending}>
              {t("setup.submit")}
            </Button>
          </form>
        </CardContent>
      </Card>
    </div>
  );
}
