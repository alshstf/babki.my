import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Link, useNavigate, useParams } from "@tanstack/react-router";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { useAccounts } from "@/api/accounts";
import { isConflict } from "@/api/operations";
import { useSession } from "@/api/session";
import {
  isBrokerUnreachable,
  isConnectionMissing,
  isTokenRejected,
  useConnection,
  useDeleteConnection,
  useTriggerSync,
  useUpdateConnection,
  type TinvestConnection,
  type TinvestConnectionStatus,
  type TinvestLinkedAccount,
} from "@/api/connections";
import { formatDate, formatDateTime } from "@/lib/dates";
import { ReconcilePanel } from "./reconcile-panel";
import { RunsTable } from "./runs-table";
import { UnparsedList } from "./unparsed-list";

// The same switch as the settings list's badge: a fourth status becomes a
// type error.
function statusVariant(status: TinvestConnectionStatus): "default" | "secondary" | "destructive" {
  switch (status) {
    case "active":
      return "default";
    case "token_revoked":
      return "destructive";
    case "disabled":
      return "secondary";
  }
}

// What «последняя удачная синхронизация» may claim: the field is when the
// last successful run started, per connection while runs are per account. So
// «началась», and with several accounts "at least one of them". Null is «удачных
// синхронизаций ещё не было»; an unparseable instant is a sync whose time could
// not be read, not "none".
function lastSyncLine(
  t: (key: string, vars?: Record<string, string>) => string,
  connection: TinvestConnection,
): string {
  const at = connection.last_successful_sync_at;
  if (!at) return t("connections.detail.neverSynced");
  const time = formatDateTime(at);
  if (!time) return t("connections.detail.lastSyncTimeUnreadable");
  return connection.accounts.length > 1
    ? t("connections.detail.lastSyncMany", { time })
    : t("connections.detail.lastSyncOne", { time });
}

// One linked pair, each side under its own name; the broker's label is
// frozen when the link was made.
function LinkedAccountRow({
  link,
  accountName,
}: {
  link: TinvestLinkedAccount;
  accountName: string | undefined;
}) {
  const { t } = useTranslation();
  const openedOn = link.opened_on ? formatDate(link.opened_on) : "";
  return (
    <li className="rounded-lg border p-3 text-sm">
      <div className="grid gap-0.5">
        <Link
          to="/accounts/$accountId"
          params={{ accountId: link.account_id }}
          className="font-medium text-primary underline underline-offset-4"
        >
          {/* The babki account's name when the accounts list has it, else a plain
             «open it»; the broker's name names the other end. */}
          {accountName ?? t("connections.detail.accountFallback")}
        </Link>
        {/* The broker's label is frozen when the link is made, so «у брокера» is
           qualified. */}
        <span
          className="text-xs text-muted-foreground"
          title={t("connections.detail.brokerAccountNameFrozen")}
        >
          {t("connections.detail.brokerAccount", { name: link.broker_account_name })}
        </span>
        {/* The broker's account type, verbatim and untranslated, frozen like the
           name above, and qualified the same way. */}
        <span
          className="text-xs text-muted-foreground"
          title={t("connections.detail.brokerAccountTypeFrozen")}
        >
          {t("connections.detail.brokerAccountType", { type: link.broker_account_type })}
        </span>
        {openedOn !== "" && (
          <span className="text-xs text-muted-foreground">
            {t("connections.detail.openedOn", { date: openedOn })}
          </span>
        )}
      </div>
    </li>
  );
}

// ConnectionDetailPage: whether the connection works, the accounts it feeds,
// the last check against the broker, the run log and the unreadable
// operations.
export function ConnectionDetailPage() {
  const { t } = useTranslation();
  const { connectionId } = useParams({ from: "/app/settings/connections/$connectionId" });
  const navigate = useNavigate();
  const { data: session } = useSession();
  const isOwner = session?.role === "owner";

  // Hooks before the owner gate; an empty id keeps a non-owner from asking
  // (useConnection is disabled on it).
  const connection = useConnection(isOwner ? connectionId : "");
  const accounts = useAccounts();
  const triggerSync = useTriggerSync();
  // Two mutation states over one endpoint, so one action's error never
  // appears under the other's button.
  const toggleConnection = useUpdateConnection();
  const replaceToken = useUpdateConnection();
  const deleteConnection = useDeleteConnection();
  const [tokenFormOpen, setTokenFormOpen] = useState(false);
  const [token, setToken] = useState("");
  const [deleteOpen, setDeleteOpen] = useState(false);

  if (!isOwner) {
    return (
      <Alert>
        <AlertDescription>{t("settings.ownerOnly")}</AlertDescription>
      </Alert>
    );
  }

  if (connection.isPending) {
    return <div className="text-muted-foreground">{t("app.loading")}</div>;
  }
  if (connection.isError || !connection.data) {
    return (
      <Alert variant="destructive">
        <AlertDescription>
          {isConnectionMissing(connection.error)
            ? t("connections.detail.notFound")
            : t("app.error")}
        </AlertDescription>
      </Alert>
    );
  }

  const data = connection.data;
  const accountName = (accountId: string) =>
    accounts.data?.find((account) => account.id === accountId)?.name;

  // The "queued" line is about the press that produced it; every other
  // action on the card clears it, and the run log reports the sync.
  const forgetSyncMessage = () => triggerSync.reset();

  const submitToken = () => {
    forgetSyncMessage();
    replaceToken.mutate(
      { id: data.id, body: { token } },
      {
        onSuccess: () => {
          setToken("");
          setTokenFormOpen(false);
        },
      },
    );
  };

  return (
    <div className="grid gap-6">
      <h1 className="text-2xl font-bold">{t("connections.detail.title")}</h1>

      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2">
            {t("connections.tinvest")}
            <Badge variant={statusVariant(data.status)}>
              {t(`connections.statuses.${data.status}`)}
            </Badge>
          </CardTitle>
        </CardHeader>
        <CardContent className="grid gap-4">
          <div className="grid gap-1 text-sm text-muted-foreground">
            <span>{t("connections.tokenLast4", { last4: data.token_last4 })}</span>
            <span>{lastSyncLine(t, data)}</span>
          </div>

          {/* Each banner from the status that means it: a refused token waits for
             a new one, a switched-off connection for nobody. */}
          {data.status === "token_revoked" && (
            <Alert variant="destructive">
              <AlertDescription>{t("connections.detail.revokedBanner")}</AlertDescription>
            </Alert>
          )}
          {data.status === "disabled" && (
            <Alert>
              <AlertDescription>{t("connections.detail.disabledBanner")}</AlertDescription>
            </Alert>
          )}

          <div className="flex flex-wrap gap-2">
            {/* Only active connections sync, so the button is disabled for the rest
               and the line below says why; not a title, which a disabled button
               cannot show. */}
            <Button
              disabled={data.status !== "active" || triggerSync.isPending}
              onClick={() => triggerSync.mutate(data.id)}
            >
              {t("connections.detail.syncNow")}
            </Button>
            {/* No on/off at token_revoked: switching on would set active on a token
               the broker refused; the repair is the new token. */}
            {data.status === "active" && (
              <Button
                variant="outline"
                disabled={toggleConnection.isPending}
                onClick={() => {
                  forgetSyncMessage();
                  toggleConnection.mutate({ id: data.id, body: { status: "disabled" } });
                }}
              >
                {t("connections.detail.disable")}
              </Button>
            )}
            {data.status === "disabled" && (
              <Button
                variant="outline"
                disabled={toggleConnection.isPending}
                onClick={() => {
                  forgetSyncMessage();
                  toggleConnection.mutate({ id: data.id, body: { status: "active" } });
                }}
              >
                {t("connections.detail.enable")}
              </Button>
            )}
            <Button
              variant="outline"
              onClick={() => {
                setTokenFormOpen((open) => !open);
                replaceToken.reset();
                forgetSyncMessage();
              }}
            >
              {t("connections.detail.newToken")}
            </Button>
            {/* The destructive button style the confirmation dialog already uses. */}
            <Button variant="destructive" onClick={() => setDeleteOpen(true)}>
              {t("connections.detail.delete")}
            </Button>
          </div>
          {data.status !== "active" && (
            <p className="text-sm text-muted-foreground">
              {t("connections.detail.syncOnlyActive")}
            </p>
          )}

          {/* queued=false means already in the queue, possibly waiting out a
             backoff of hours, so the sentence does not say «уже идёт». */}
          {triggerSync.data && (
            <Alert>
              <AlertDescription>
                {triggerSync.data.queued
                  ? t("connections.detail.syncQueued")
                  : t("connections.detail.syncAlreadyQueued")}
              </AlertDescription>
            </Alert>
          )}
          {/* By status: 409 means the connection is no longer active. */}
          {triggerSync.isError && (
            <Alert variant="destructive">
              <AlertDescription>
                {isConflict(triggerSync.error)
                  ? t("connections.detail.syncNotActive")
                  : t("app.error")}
              </AlertDescription>
            </Alert>
          )}
          {toggleConnection.isError && (
            <Alert variant="destructive">
              <AlertDescription>{t("app.error")}</AlertDescription>
            </Alert>
          )}

          {tokenFormOpen && (
            <div className="grid max-w-md gap-2">
              <Label htmlFor="tinvest-new-token">
                {t("connections.detail.newTokenLabel")}
              </Label>
              <Input
                id="tinvest-new-token"
                type="password"
                autoComplete="off"
                value={token}
                onChange={(e) => {
                  setToken(e.target.value);
                  replaceToken.reset();
                }}
              />
              {/* The wizard's two answers, by status: 400 the broker refused the
                 token, 502 the broker could not be reached. */}
              {replaceToken.isError && (
                <Alert variant="destructive">
                  <AlertDescription>
                    {isTokenRejected(replaceToken.error)
                      ? t("connections.wizard.tokenRejected")
                      : isBrokerUnreachable(replaceToken.error)
                        ? t("connections.wizard.brokerUnreachable")
                        : t("app.error")}
                  </AlertDescription>
                </Alert>
              )}
              <div>
                <Button
                  disabled={token.trim() === "" || replaceToken.isPending}
                  onClick={submitToken}
                >
                  {t("connections.detail.newTokenSave")}
                </Button>
              </div>
            </div>
          )}
          {/* A replacement is stored only after the broker accepted it, so a
             success is the broker's acceptance. */}
          {replaceToken.isSuccess && !tokenFormOpen && (
            <Alert>
              <AlertDescription>{t("connections.detail.newTokenAccepted")}</AlertDescription>
            </Alert>
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>{t("connections.detail.accountsTitle")}</CardTitle>
        </CardHeader>
        <CardContent>
          {/* Deleting a babki account removes its link (ON DELETE CASCADE) and
             leaves the connection; an empty list says so. */}
          {data.accounts.length === 0 ? (
            <p className="text-sm text-muted-foreground">
              {t("connections.detail.accountsEmpty")}
            </p>
          ) : (
            <ul className="grid gap-2">
              {data.accounts.map((link) => (
                <LinkedAccountRow
                  key={link.link_id}
                  link={link}
                  accountName={accountName(link.account_id)}
                />
              ))}
            </ul>
          )}
        </CardContent>
      </Card>

      <ReconcilePanel connectionId={data.id} reconciles={data.reconciles} />
      <RunsTable connectionId={data.id} links={data.accounts} />
      <UnparsedList connectionId={data.id} />

      <Dialog
        open={deleteOpen}
        onOpenChange={(open) => {
          if (!open) {
            setDeleteOpen(false);
            deleteConnection.reset();
          }
        }}
      >
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>{t("connections.detail.deleteTitle")}</DialogTitle>
          </DialogHeader>
          {/* What goes (token, links, mirror, instrument map, run log) and what
             stays (the accounts and their operations), since «удалить
             подключение» reads like «удалить всё». */}
          <p className="text-sm text-muted-foreground">
            {t("connections.detail.deleteConfirm")}
          </p>
          {deleteConnection.isError && (
            <Alert variant="destructive">
              <AlertDescription>{t("app.error")}</AlertDescription>
            </Alert>
          )}
          <DialogFooter>
            <Button
              variant="outline"
              onClick={() => {
                setDeleteOpen(false);
                deleteConnection.reset();
              }}
            >
              {t("common.cancel")}
            </Button>
            <Button
              variant="destructive"
              disabled={deleteConnection.isPending}
              onClick={() =>
                deleteConnection.mutate(data.id, {
                  // Leave a screen whose connection is gone, rather than refetch into a
                  // 404.
                  onSuccess: () => void navigate({ to: "/settings" }),
                })
              }
            >
              {t("connections.detail.delete")}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
