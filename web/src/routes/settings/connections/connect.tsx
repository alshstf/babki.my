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
  isBrokerUnreachable,
  isTokenRejected,
  useCheckToken,
  useCreateConnection,
  type TinvestBrokerAccount,
} from "@/api/connections";

// Where T-Invest lets an owner issue an API token. Read-only navigation, never
// pre-filled with anything of the owner's — just the page the instructions
// step sends them to.
const TOKEN_SETTINGS_URL = "https://www.tbank.ru/invest/settings/api/";

type Step = "instructions" | "token" | "accounts";

// ConnectWizardPage walks the owner through connecting a T-Invest account:
// read the instructions, paste a read-only token and have it checked against
// the broker, then pick which of the accounts it can see to import.
//
// The token lives ONLY in this component's own state (`token` below) for as
// long as the wizard is open. It is never put in the URL, router state or any
// browser storage, and this file never logs it — it goes exactly twice, both
// times as a request body: once to token-check (which stores nothing) and
// once, if the owner goes through with it, to create the connection.
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
        setStep("accounts");
      },
    });
  };

  const pickedIds = accounts
    .map((account) => account.broker_account_id)
    .filter((id) => selected[id]);
  const namesFilled = pickedIds.every((id) => (names[id] ?? "").trim() !== "");
  const canCreate = pickedIds.length > 0 && namesFilled && !createConnection.isPending;

  const submitCreate = () => {
    createConnection.mutate({
      token,
      accounts: pickedIds.map((id) => ({
        broker_account_id: id,
        account_name: names[id].trim(),
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
            {/* By status, never by the broker's own sentence (that string is
                English prose meant for a log — see api/openapi.yaml on
                POST /api/v1/tinvest/token-check). 400 is the broker refusing
                the token itself; 502 is this server failing to reach the
                broker at all — different news, worth a different sentence. */}
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
              // Empty means the token works and there is nothing to import
              // through it — a different answer from a refused token, and it
              // must not be captioned as one (see TinvestTokenCheckResponse.accounts
              // in the API contract).
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
            {/* Same rule as the token step's error above: status, not prose.
                409 here means one of the picked broker accounts is already
                imported by another connection (isConflict — the journal's own
                helper, since it is the same status code checked the same
                way); the token-specific captions cover the two ways create
                can also refuse the token itself. */}
            {createConnection.isError && (
              <Alert variant="destructive">
                <AlertDescription>
                  {isConflict(createConnection.error)
                    ? t("connections.wizard.createConflict")
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
