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

// The catalog, and the place to correct it. Instruments come from the trade
// dialogs and the importer; a missing ISIN means never priced, so the holding
// counts at nought in the account's total. Under settings because the catalog is
// instance-wide.
export function InstrumentsPage() {
  const { t } = useTranslation();
  const { data: session } = useSession();
  const [query, setQuery] = useState("");
  const [editing, setEditing] = useState<Instrument | undefined>(undefined);
  const instruments = useInstruments(query);

  const rows =
    instruments.data?.pages.flatMap((page) => page.instruments) ?? [];
  // Owner-only controls, stricter than the server (which allows editors): an
  // editor enters their own trades but does not rewrite facts every member's
  // figures rest on. Both cards on this screen apply the same rule.
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
            aria-label={t("instruments.searchPlaceholder")}
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
                    <Link
                      to="/instruments/$instrumentId"
                      params={{ instrumentId: instrument.id }}
                      className="font-medium hover:underline"
                    >
                      {instrument.name}
                    </Link>
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
                  {/* The row that cannot be priced says so beside the missing ISIN, the
                     field the quote worker searches by; only for types this program
                     values. */}
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
