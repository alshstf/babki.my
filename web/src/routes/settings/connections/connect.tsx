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

// The T-Invest settings page an owner issues an API token from. Read-only
// navigation, never pre-filled with anything of the owner's — just the page the
// instructions step sends them to.
//
// THIS EXACT ADDRESS AND NOT A DEEPER ONE, because a deeper one cannot be
// checked: the broker's host answers 200 to ANY path under it, so a wrong guess
// would never show up as a broken link — the owner would simply land somewhere
// else with nothing saying so. This is the address the broker's own developer
// documentation gives for issuing a token, which is why the link is captioned
// as the investment settings rather than as an API section: what the page holds
// beyond that is not something this file can verify.
const TOKEN_SETTINGS_URL = "https://www.tbank.ru/invest/settings/";

type Step = "instructions" | "token" | "accounts";

// ConnectWizardPage walks the owner through connecting a T-Invest account:
// read the instructions, paste a read-only token and have it checked against
// the broker, then pick which of the accounts it can see to import.
//
// WHERE THE TOKEN LIVES, both places. Chiefly in this component's own state
// (`token` below), for as long as the wizard is open. But a mutation keeps the
// variables it was called with, so a copy also sits in react-query's cache —
// which outlives this screen, since the cache belongs to the app and not to the
// wizard, until the mutation is garbage-collected or the tab is closed.
//
// Both places are memory of this one page load, and neither survives the tab.
// What does NOT happen, and is what would matter: it never goes into the URL,
// never into router state, never into browser storage — the one key this
// application keeps there is the display-currency preference, written from
// lib/display-currency.ts and nowhere near this screen — and this file never
// logs it. It leaves the browser only as a request body, and only to the two
// endpoints below: token-check, which stores nothing, and — if the owner goes
// through with it — the one that creates the connection.
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
        // Only if the wizard is still where the check was started from. The
        // answer can arrive after the owner has pressed «Назад», and moving
        // them forward then is the screen deciding on its own to leave the page
        // they chose — read as a step, not as a late answer. The functional
        // form is what makes this the CURRENT step rather than the one captured
        // when the request went out.
        setStep((current) => (current === "token" ? "accounts" : current));
      },
    });
  };

  // One guarded read of `names` for both the rule that enables the button and
  // what the button then sends. They used to differ — the rule guarded the
  // missing key, the send did not — and two readings of one map are two
  // readings that can disagree.
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
                409 means one of the picked broker accounts is already imported
                by another connection (isConflict — the journal's own helper,
                since it is the same status code checked the same way). 422
                means the token is fine and the broker's account list is no
                longer the one this step is showing: creating asks for it
                afresh, so an account closed since the check, or a token whose
                access was narrowed, lands here. It is captioned as what it is
                and NOT as a refused token — that was one 400 for both, and the
                owner was told to re-issue a token that never stopped working.
                The token captions below still cover the two ways creating can
                refuse the token itself. */}
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
