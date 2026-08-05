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

// The same switch the settings list draws its badge from, and for the same
// reason: three states that are three different pieces of news, so a fourth
// added to the contract lands here as a type error rather than as a colour.
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

// WHAT «ПОСЛЕДНЯЯ УДАЧНАЯ СИНХРОНИЗАЦИЯ» IS ALLOWED TO CLAIM. The field is the
// moment the last successful run STARTED, and it is keyed by the connection
// while runs are made one per broker account (see TinvestConnection in the API
// contract, and Store.LastSuccessfulSyncAt behind it). Two things follow, both
// of them easy to state the other way round by accident:
//
//   - «началась», not «завершилась»: the mirror was not current at this
//     instant, it started becoming current then.
//   - for a connection importing more than one broker account it means AT
//     LEAST ONE of them synced then — never all of them — so the sentence says
//     so out loud instead of leaving the reader to assume the whole connection
//     was up to date.
//
// Null is «удачных синхронизаций ещё не было», which is exactly what null
// means. An instant that will not parse is neither of those: it is a
// successful sync whose time this screen could not read, and saying «ещё не
// было» about it would be inventing a fact from a formatting failure.
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

// One linked pair: the babki account the import feeds, and the broker account
// it is fed from. Both are named, and neither is named with the other's name —
// the two are separate facts and the broker's label is frozen at the moment the
// link was made (see TinvestLinkedAccount.broker_account_name), so it can
// differ from what either side calls the account today.
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
          {/* The babki account's own name when the accounts list holds it, and
              a plain «open it» when it does not — while that list is loading,
              or if it failed. The broker's name is NOT borrowed for the link:
              it names the other end of the pair. */}
          {accountName ?? t("connections.detail.accountFallback")}
        </Link>
        {/* The label is frozen at the moment the link was made and is not
            re-read on every sync (see TinvestLinkedAccount.broker_account_name),
            so «у брокера» is qualified rather than left to read as the name the
            broker uses today. */}
        <span
          className="text-xs text-muted-foreground"
          title={t("connections.detail.brokerAccountNameFrozen")}
        >
          {t("connections.detail.brokerAccount", { name: link.broker_account_name })}
        </span>
        <span className="text-xs text-muted-foreground">
          {/* The broker's own classification word, kept verbatim as the
              evidence of what was connected (ACCOUNT_TYPE_TINKOFF_IIS and the
              like). Not translated: it is the broker's vocabulary, not ours. */}
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

// ConnectionDetailPage is everything one T-Invest connection has to say for
// itself: whether it still works, which accounts it feeds, what the last check
// against the broker found, what every sync run did, and which of the broker's
// operations this program could not read.
export function ConnectionDetailPage() {
  const { t } = useTranslation();
  const { connectionId } = useParams({ from: "/app/settings/connections/$connectionId" });
  const navigate = useNavigate();
  const { data: session } = useSession();
  const isOwner = session?.role === "owner";

  // Every hook before the owner gate below, per the Rules of Hooks. The empty
  // id keeps a non-owner's browser from asking for something the server would
  // refuse anyway (useConnection is disabled on an empty id).
  const connection = useConnection(isOwner ? connectionId : "");
  const accounts = useAccounts();
  const triggerSync = useTriggerSync();
  // Two independent mutation states over one endpoint on purpose: switching the
  // connection off and pasting a new token fail in different ways and are
  // captioned differently, and a single shared state would let one action's
  // refusal appear under the other's button.
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

  const submitToken = () => {
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

          {/* Each banner is drawn from the status that means it, and neither is
              drawn for the other: a token the broker refused waits for the
              owner to paste a new one, a switched-off connection waits for
              nobody. */}
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
            {/* Only an active connection is synced at all — the scheduler
                passes over the other two and this endpoint answers them 409 —
                so the button is dead for them, and the line below the row says
                why. NOT a `title`: a disabled button carries
                `pointer-events-none`, so a tooltip on it is a sentence nobody
                can reach. */}
            <Button
              disabled={data.status !== "active" || triggerSync.isPending}
              onClick={() => triggerSync.mutate(data.id)}
            >
              {t("connections.detail.syncNow")}
            </Button>
            {/* No on/off switch at token_revoked. Switching such a connection
                «on» would set active on a token the broker has already refused:
                the next run fails, the server parks it back, and the button
                will have promised a repair it cannot make. The repair is the
                new token beside it. */}
            {data.status === "active" && (
              <Button
                variant="outline"
                disabled={toggleConnection.isPending}
                onClick={() =>
                  toggleConnection.mutate({ id: data.id, body: { status: "disabled" } })
                }
              >
                {t("connections.detail.disable")}
              </Button>
            )}
            {data.status === "disabled" && (
              <Button
                variant="outline"
                disabled={toggleConnection.isPending}
                onClick={() =>
                  toggleConnection.mutate({ id: data.id, body: { status: "active" } })
                }
              >
                {t("connections.detail.enable")}
              </Button>
            )}
            <Button
              variant="outline"
              onClick={() => {
                setTokenFormOpen((open) => !open);
                replaceToken.reset();
              }}
            >
              {t("connections.detail.newToken")}
            </Button>
            <Button variant="outline" className="text-red-500" onClick={() => setDeleteOpen(true)}>
              {t("connections.detail.delete")}
            </Button>
          </div>
          {data.status !== "active" && (
            <p className="text-sm text-muted-foreground">
              {t("connections.detail.syncOnlyActive")}
            </p>
          )}

          {/* WHAT `queued: false` IS ALLOWED TO SAY. It means a sync was
              already in the queue — and «in the queue» covers one waiting out a
              failed attempt's backoff, which River grows into the hours (see
              TinvestSyncAcceptedResponse in the API contract). «Уже идёт» would
              therefore be false for as long as that wait lasts, so the sentence
              claims only what is true of both. */}
          {triggerSync.data && (
            <Alert>
              <AlertDescription>
                {triggerSync.data.queued
                  ? t("connections.detail.syncQueued")
                  : t("connections.detail.syncAlreadyQueued")}
              </AlertDescription>
            </Alert>
          )}
          {/* By status, never by the server's own sentence. 409 here says one
              thing only: the connection is not active — which, with the button
              disabled for the other two states, means the status moved under
              this screen since it loaded. */}
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
              {/* The same two answers the wizard's token step branches on, by
                  the same statuses: 400 is the broker refusing this token, 502
                  is this server failing to reach the broker at all. */}
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
          {/* Said only where it was witnessed: the server stores a replacement
              token only after the broker has accepted it (PATCH
              .../connections/{id} in the contract), so a successful answer to
              this request IS the broker's acceptance. */}
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
          {/* A connection is created with at least one account and can end up
              with none: deleting a babki account takes its link with it
              (migration 0014's ON DELETE CASCADE on account_id) while leaving
              the connection standing. An empty list is that, and it is said
              rather than drawn as blank space. */}
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

      <ReconcilePanel connectionId={data.id} snapshot={data.last_reconcile} />
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
          {/* WHAT ACTUALLY GOES AND WHAT STAYS, from the contract and from
              migration 0014's cascade: the token, the links, the mirror of the
              broker's operations, the instrument map and the run log are
              deleted; the babki accounts this connection created and the
              journal operations the projection wrote into them are not touched
              — they carry no foreign key back to the connection. Saying so is
              the point of this dialog: «удалить подключение» reads like «удалить
              всё, что оно принесло» unless the difference is spelled out. */}
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
                  // Away from a screen whose subject no longer exists. Without
                  // it the invalidation this mutation fires would refetch the
                  // connection, meet a 404 and leave the owner looking at «нет
                  // такого подключения» where a moment ago there was one.
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
