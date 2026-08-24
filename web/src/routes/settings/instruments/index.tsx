import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Link } from "@tanstack/react-router";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Badge } from "@/components/ui/badge";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { useInstruments, type Instrument } from "@/api/instruments";
import { useSession } from "@/api/session";
import { InstrumentEditDialog } from "./edit-dialog";
import { CorporateActions } from "./corporate-actions";

// THE CATALOG, AND THE FIRST PLACE IT CAN BE CORRECTED. Instruments are created
// by the trade dialogs and by the importer, and until this screen existed
// nothing in the interface could change one afterwards: a paper entered by hand
// with a misspelled ticker, or with no ISIN at all, stayed that way for ever.
//
// An absent ISIN is not cosmetic. It is the field the quote worker searches the
// broker by, so a paper without one is never priced — and an unpriced holding
// goes into the account's total counted at nought, dragging it down by whatever
// the paper actually cost.
//
// One instance-wide catalog, so this sits in the settings rather than under an
// account: the same row backs every account that holds the paper.
export function InstrumentsPage() {
  const { t } = useTranslation();
  const { data: session } = useSession();
  const [query, setQuery] = useState("");
  const [editing, setEditing] = useState<Instrument | undefined>(undefined);
  const instruments = useInstruments(query);

  const rows =
    instruments.data?.pages.flatMap((page) => page.instruments) ?? [];
  // The catalog is shared by the whole instance, and correcting a row changes
  // it for every member — as does recording a corporate action, which is why
  // both cards on this screen answer this one question the same way.
  //
  // THIS SCREEN IS STRICTER THAN THE SERVER, deliberately and not by accident:
  // the write endpoints require an EDITOR (family.RequireRole is a floor, so an
  // owner passes it too), and this offers the controls to the owner alone. A
  // household's editor can enter their own trades without also rewriting facts
  // that every other member's figures rest on. The sentence that used to stand
  // here called this the server's rule and gave 403 as the proof, which was
  // simply false about an editor: they would have been refused by a screen that
  // said the server had refused them.
  const isOwner = session?.role === "owner";

  return (
    <div className="grid gap-6">
      <Link
        to="/settings"
        className="text-sm text-muted-foreground hover:underline"
      >
        {t("instruments.back")}
      </Link>
      <h1 className="text-2xl font-bold">{t("instruments.title")}</h1>
      <Card>
        <CardHeader>
          <CardTitle>{t("instruments.searchTitle")}</CardTitle>
        </CardHeader>
        <CardContent className="grid gap-4">
          <Input
            data-testid="instrument-search"
            placeholder={t("instruments.searchPlaceholder")}
            value={query}
            onChange={(e) => setQuery(e.target.value)}
          />
          {instruments.isError && (
            <Alert variant="destructive">
              <AlertDescription>{t("app.error")}</AlertDescription>
            </Alert>
          )}
          {instruments.isPending && (
            <p className="text-sm text-muted-foreground">{t("app.loading")}</p>
          )}
          {!instruments.isPending &&
            !instruments.isError &&
            rows.length === 0 && (
              <p
                className="text-sm text-muted-foreground"
                data-testid="instruments-empty"
              >
                {t("instruments.empty")}
              </p>
            )}
          <ul className="grid gap-2">
            {rows.map((instrument) => (
              <li
                key={instrument.id}
                data-testid="instrument-row"
                className="flex flex-wrap items-center justify-between gap-2 rounded-md border p-3"
              >
                <div className="grid gap-0.5">
                  <div className="flex flex-wrap items-center gap-2">
                    <span className="font-medium">{instrument.name}</span>
                    <Badge variant="secondary">
                      {t(`instrumentTypes.${instrument.type}`)}
                    </Badge>
                    {instrument.frozen && (
                      <Badge variant="outline">
                        {t("instruments.fields.frozen")}
                      </Badge>
                    )}
                  </div>
                  <div className="text-xs text-muted-foreground">
                    {[instrument.ticker, instrument.isin, instrument.currency]
                      .filter((part) => part !== "")
                      .join(" · ")}
                  </div>
                  {/* THE ROW THAT CANNOT BE PRICED SAYS SO, beside the field
                      that is missing. This is the whole reason a reader comes
                      here: an ISIN is what the quote worker searches by, and a
                      paper without one is never valued — which shows up on the
                      positions screen as «Нет котировки» and in the account's
                      total as a holding counted at nought. Said only for the
                      types this program values at all: a currency or a metal
                      has no quote coming either way, and the sentence would be
                      true of the ISIN and false about the consequence. */}
                  {instrument.isin === "" &&
                    (instrument.type === "share" ||
                      instrument.type === "bond" ||
                      instrument.type === "etf") && (
                      <div
                        className="text-xs text-amber-600"
                        data-testid="instrument-no-isin"
                      >
                        {t("instruments.noIsin")}
                      </div>
                    )}
                </div>
                {isOwner && (
                  <Button
                    variant="outline"
                    size="sm"
                    data-testid={`instrument-edit-${instrument.ticker || instrument.id}`}
                    onClick={() => setEditing(instrument)}
                  >
                    {t("common.edit")}
                  </Button>
                )}
              </li>
            ))}
          </ul>
          {instruments.hasNextPage && (
            <Button
              variant="outline"
              data-testid="instruments-more"
              disabled={instruments.isFetchingNextPage}
              onClick={() => void instruments.fetchNextPage()}
            >
              {t("instruments.more")}
            </Button>
          )}
        </CardContent>
      </Card>
      <CorporateActions canEdit={isOwner} />
      <InstrumentEditDialog
        instrument={editing}
        onOpenChange={(open) => {
          if (!open) setEditing(undefined);
        }}
      />
    </div>
  );
}
