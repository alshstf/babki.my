import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Link } from "@tanstack/react-router";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Checkbox } from "@/components/ui/checkbox";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { isConflict } from "@/api/operations";
import { useSession } from "@/api/session";
import {
  isBrokerAccountNotImportable,
  isBrokerUnreachable,
  isTokenRejected,
  useCheckToken,
  useCreateConnection,
  type TinvestBrokerAccount,
} from "@/api/connections";

// The T-Invest settings page where an API token is issued, as the broker's
// developer docs give it. Not a deeper path: the host answers 200 to any path,
// so a wrong guess would never look broken.
const TOKEN_SETTINGS_URL = "https://www.tbank.ru/invest/settings/";

type Step = "instructions" | "token" | "accounts";

// ConnectWizardPage connects a T-Invest account: instructions, a read-only token
// checked against the broker, then the accounts to import. The token lives in this
// component's state and in react-query's mutation cache for this page load; it
// never goes into the URL, router state, browser storage or a log, and leaves only
// as a request body to token-check (stores nothing) and the create endpoint.
export function ConnectWizardPage() {
  const { t } = useTranslation();
  const { data: session } = useSession();
  const isOwner = session?.role === "owner";

  const [step, setStep] = useState<Step>("instructions");
  const [token, setToken] = useState("");
  const [accounts, setAccounts] = useState<TinvestBrokerAccount[]>([]);
  const [selected, setSelected] = useState<Record<string, boolean>>({});
  const [names, setNames] = useState<Record<string, string>>({});

  const checkToken = useCheckToken();
  const createConnection = useCreateConnection();

  if (!isOwner) {
    return (
      <Alert>
        <AlertDescription>{t("settings.ownerOnly")}</AlertDescription>
      </Alert>
    );
  }

  const submitToken = () => {
    checkToken.mutate(token, {
      onSuccess: (data) => {
        const initialSelected: Record<string, boolean> = {};
        const initialNames: Record<string, string> = {};
        for (const account of data.accounts) {
          initialSelected[account.broker_account_id] = false;
          initialNames[account.broker_account_id] = account.name;
        }
        setAccounts(data.accounts);
        setSelected(initialSelected);
        setNames(initialNames);
        // Only if the wizard is still on that step: the answer may arrive after
        // «Назад», and moving forward then would override the owner's choice. The
        // functional form reads the current step.
        setStep((current) => (current === "token" ? "accounts" : current));
      },
    });
  };

  // One guarded read of `names` for both the button's rule and what it sends.
  const nameOf = (id: string) => (names[id] ?? "").trim();

  const pickedIds = accounts
    .map((account) => account.broker_account_id)
    .filter((id) => selected[id]);
  const namesFilled = pickedIds.every((id) => nameOf(id) !== "");
  const canCreate = pickedIds.length > 0 && namesFilled && !createConnection.isPending;

  const submitCreate = () => {
    createConnection.mutate({
      token,
      accounts: pickedIds.map((id) => ({
        broker_account_id: id,
        account_name: nameOf(id),
      })),
    });
  };

  return (
    <div className="grid max-w-xl gap-6">
      <h1 className="text-2xl font-bold">{t("connections.wizard.title")}</h1>

      {step === "instructions" && (
        <Card>
          <CardHeader>
            <CardTitle>{t("connections.wizard.instructionsTitle")}</CardTitle>
          </CardHeader>
          <CardContent className="grid gap-4">
            <p className="text-sm text-muted-foreground">
              {t("connections.wizard.instructionsBody")}
            </p>
            <a
              href={TOKEN_SETTINGS_URL}
              target="_blank"
              rel="noreferrer"
              className="text-sm text-primary underline underline-offset-4"
            >
              {t("connections.wizard.instructionsLink")}
            </a>
            <Alert>
              <AlertDescription>{t("connections.wizard.tokenShownOnce")}</AlertDescription>
            </Alert>
            <div className="flex justify-end gap-2">
              <Button variant="outline" asChild>
                <Link to="/settings">{t("common.cancel")}</Link>
              </Button>
              <Button onClick={() => setStep("token")}>{t("connections.wizard.next")}</Button>
            </div>
          </CardContent>
        </Card>
      )}

      {step === "token" && (
        <Card>
          <CardHeader>
            <CardTitle>{t("connections.wizard.tokenTitle")}</CardTitle>
          </CardHeader>
          <CardContent className="grid gap-4">
            <div className="grid gap-2">
              <Label htmlFor="tinvest-token">{t("connections.wizard.tokenLabel")}</Label>
              <Input
                id="tinvest-token"
                type="password"
                autoComplete="off"
                value={token}
                onChange={(e) => {
                  setToken(e.target.value);
                  checkToken.reset();
                }}
              />
            </div>
            {/* By status, not the broker's English: 400 the broker refused the
               token, 502 the broker could not be reached. */}
            {checkToken.isError && (
              <Alert variant="destructive">
                <AlertDescription>
                  {isTokenRejected(checkToken.error)
                    ? t("connections.wizard.tokenRejected")
                    : isBrokerUnreachable(checkToken.error)
                      ? t("connections.wizard.brokerUnreachable")
                      : t("app.error")}
                </AlertDescription>
              </Alert>
            )}
            <div className="flex justify-end gap-2">
              <Button variant="outline" onClick={() => setStep("instructions")}>
                {t("connections.wizard.back")}
              </Button>
              <Button disabled={token.trim() === "" || checkToken.isPending} onClick={submitToken}>
                {t("connections.wizard.checkToken")}
              </Button>
            </div>
          </CardContent>
        </Card>
      )}

      {step === "accounts" && (
        <Card>
          <CardHeader>
            <CardTitle>{t("connections.wizard.accountsTitle")}</CardTitle>
          </CardHeader>
          <CardContent className="grid gap-4">
            {accounts.length === 0 ? (
              // Empty means the token works and has nothing to import, not a refused
              // token (TinvestTokenCheckResponse.accounts).
              <p className="text-sm text-muted-foreground">
                {t("connections.wizard.noAccounts")}
              </p>
            ) : (
              <div className="grid gap-3">
                {accounts.map((account) => {
                  const checkboxId = `tinvest-account-${account.broker_account_id}`;
                  return (
                    <div
                      key={account.broker_account_id}
                      className="grid gap-2 rounded-lg border p-3"
                    >
                      <div className="flex items-center gap-2">
                        <Checkbox
                          id={checkboxId}
                          checked={Boolean(selected[account.broker_account_id])}
                          onCheckedChange={(checked) =>
                            setSelected((prev) => ({
                              ...prev,
                              [account.broker_account_id]: checked === true,
                            }))
                          }
                        />
                        <Label htmlFor={checkboxId}>{account.name}</Label>
                      </div>
                      <Input
                        aria-label={t("connections.wizard.accountNameFieldLabel", {
                          name: account.name,
                        })}
                        placeholder={t("connections.wizard.accountNamePlaceholder")}
                        value={names[account.broker_account_id] ?? ""}
                        onChange={(e) =>
                          setNames((prev) => ({
                            ...prev,
                            [account.broker_account_id]: e.target.value,
                          }))
                        }
                      />
                    </div>
                  );
                })}
              </div>
            )}
            {/* By status: 409 a picked account is already imported elsewhere
               (isConflict); 422 the token works but the broker's account list
               changed since the check (create asks afresh), not a refused token.
               The token captions below cover create refusing the token itself. */}
            {createConnection.isError && (
              <Alert variant="destructive">
                <AlertDescription>
                  {isConflict(createConnection.error)
                    ? t("connections.wizard.createConflict")
                    : isBrokerAccountNotImportable(createConnection.error)
                      ? t("connections.wizard.accountsChanged")
                      : isTokenRejected(createConnection.error)
                        ? t("connections.wizard.tokenRejected")
                        : isBrokerUnreachable(createConnection.error)
                          ? t("connections.wizard.brokerUnreachable")
                          : t("app.error")}
                </AlertDescription>
              </Alert>
            )}
            <div className="flex justify-end gap-2">
              <Button variant="outline" onClick={() => setStep("token")}>
                {t("connections.wizard.back")}
              </Button>
              <Button disabled={!canCreate} onClick={submitCreate}>
                {t("connections.wizard.create")}
              </Button>
            </div>
          </CardContent>
        </Card>
      )}
    </div>
  );
}
