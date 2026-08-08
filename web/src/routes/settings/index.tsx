import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Link } from "@tanstack/react-router";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { useSession } from "@/api/session";
import { useUpdateSpace, type UpdateSpaceBody } from "@/api/space";
import { useTaxResidencies } from "@/api/tax-residencies";
import { useConnections, type TinvestConnectionStatus } from "@/api/connections";
import { ApiError } from "@/api/operations";
import { CostBasisNotice } from "@/components/cost-basis-notice";
import { countryName } from "@/lib/country";
import { COMMON_CURRENCIES } from "@/lib/currencies";


export function SettingsPage() {
  const { t } = useTranslation();
  const { data: session } = useSession();
  const updateSpace = useUpdateSpace();
  const isOwner = session?.role === "owner";
  // Only the owner may change either setting, and the list is only ever used
  // by the form below — nobody else's screen pays for the request.
  const residencies = useTaxResidencies(isOwner);

  // The layout route only renders once a session is loaded, so `session` is
  // already available here — safe to seed local state from it once.
  const [currency, setCurrency] = useState(() =>
    session && COMMON_CURRENCIES.includes(session.base_currency) ? session.base_currency : "custom",
  );
  const [customCurrency, setCustomCurrency] = useState(() =>
    session && !COMMON_CURRENCIES.includes(session.base_currency) ? session.base_currency : "",
  );
  const [country, setCountry] = useState(() => session?.tax_residency ?? "");

  if (!isOwner) {
    return (
      <Alert>
        <AlertDescription>{t("settings.ownerOnly")}</AlertDescription>
      </Alert>
    );
  }

  const effectiveCurrency = currency === "custom" ? customCurrency.toUpperCase() : currency;
  const validCurrency = /^[A-Z]{3}$/.test(effectiveCurrency);
  const currencyChanged = effectiveCurrency !== session?.base_currency;
  const countryChanged = country !== session?.tax_residency;
  const canSave =
    (currencyChanged || countryChanged) &&
    (!currencyChanged || validCurrency) &&
    !updateSpace.isPending;

  // Only what actually changed is sent: an unchanged field left out of the
  // body is left alone by the server (the PATCH is a partial update), so
  // picking a country never rewrites the base currency as a side effect.
  const save = () => {
    const body: UpdateSpaceBody = {};
    if (currencyChanged) body.base_currency = effectiveCurrency;
    if (countryChanged) body.tax_residency = country;
    updateSpace.mutate(body);
  };

  // What the SELECTED country implies for the figures — from the server's own
  // list, so the answer is visible before saving rather than after. Falls back
  // to the session's rules for the country already saved, which is what the
  // list would say anyway and is the only thing available while it loads (or
  // if a country somehow stored outside this form is not in it at all).
  const selectedRules =
    residencies.data?.find((r) => r.country === country) ??
    (country === session?.tax_residency ? session?.cost_basis_rules : undefined);

  // Sorted by the name actually shown, not by the ISO code the server sorts
  // by: a person picking their country reads down the Russian names, and
  // "Великобритания" between "Канада" and "Казахстан" looks like a bug. Which
  // countries are offered still comes entirely from the server.
  const countries = [...(residencies.data ?? [])].sort((a, b) =>
    countryName(a.country).localeCompare(countryName(b.country), "ru"),
  );

  // A country stored outside this form — a hand-edited row, or one the server's
  // table dropped since — has no option to match. Radix fills its trigger by
  // portaling the SELECTED ITEM's own text into it, and falls back to the
  // placeholder only for an empty value, so such a country renders an empty
  // box: the screen says nothing at all about the one setting it is here to
  // show, directly above a notice that explains that very country's
  // consequences. Naming it, and saying plainly that its rules are not known,
  // is the same answer the server already gives (MethodUnknown /
  // PerimeterUnknown / unknown_country) — the selector must not be the one
  // place that stays quiet about it.
  const selectedIsOffered = countries.some((rules) => rules.country === country);

  return (
    <div className="grid gap-6">
      <h1 className="text-2xl font-bold">{t("settings.title")}</h1>
      <Card className="max-w-md">
        <CardContent className="grid gap-6">
          <div className="grid gap-2">
            <Label htmlFor="base-currency">{t("settings.baseCurrency")}</Label>
            <Select
              value={currency}
              onValueChange={(v) => {
                setCurrency(v);
                updateSpace.reset();
              }}
            >
              <SelectTrigger id="base-currency">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {COMMON_CURRENCIES.map((code) => (
                  <SelectItem key={code} value={code}>
                    {code}
                  </SelectItem>
                ))}
                <SelectItem value="custom">{t("accounts.dialog.otherCurrency")}</SelectItem>
              </SelectContent>
            </Select>
            {currency === "custom" && (
              <Input
                placeholder={t("accounts.dialog.currencyPlaceholder")}
                value={customCurrency}
                maxLength={3}
                onChange={(e) => {
                  setCustomCurrency(e.target.value);
                  updateSpace.reset();
                }}
              />
            )}
            <p className="text-xs text-muted-foreground">{t("settings.baseCurrencyHint")}</p>
          </div>

          <div className="grid gap-2">
            <Label htmlFor="tax-residency">{t("settings.taxResidency")}</Label>
            {residencies.isError ? (
              <Alert variant="destructive">
                <AlertDescription>{t("app.error")}</AlertDescription>
              </Alert>
            ) : (
              <Select
                value={country}
                disabled={countries.length === 0}
                onValueChange={(v) => {
                  setCountry(v);
                  updateSpace.reset();
                }}
              >
                <SelectTrigger id="tax-residency">
                  {/* Children override what Radix renders, so they are passed
                      ONLY for the unmatched country — `undefined` otherwise
                      leaves the ordinary selected-item text in place. */}
                  <SelectValue placeholder={t("app.loading")}>
                    {countries.length > 0 && !selectedIsOffered
                      ? t("settings.unknownCountry", { country: countryName(country) })
                      : undefined}
                  </SelectValue>
                </SelectTrigger>
                <SelectContent>
                  {countries.map((rules) => (
                    <SelectItem key={rules.country} value={rules.country}>
                      {countryName(rules.country)}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            )}
            <p className="text-xs text-muted-foreground">{t("settings.taxResidencyHint")}</p>
            {selectedRules && <CostBasisNotice rules={selectedRules} confirmWhenSupported />}
          </div>

          {updateSpace.isError && (
            <Alert variant="destructive">
              <AlertDescription>
                {updateSpace.error instanceof ApiError && updateSpace.error.status === 403
                  ? t("settings.forbidden")
                  : t("app.error")}
              </AlertDescription>
            </Alert>
          )}
          {/* A saved setting has to say so here. Nothing else on this screen
              does: the fields already held the new values before the request
              went out, and the only thing that moved afterwards was the Save
              button greying out — which is also what an unchanged form looks
              like, and what a form the reader never touched looks like. The
              settings themselves only become visible somewhere else entirely,
              on /accounts (#33).

              One neutral sentence, because this condition covers three
              different saves — currency alone, country alone, or both — and a
              wording that named a field would be false on two of them. It also
              stops short of claiming the new figures are already on screen
              elsewhere: the caches this save invalidates are still refetching
              at the moment it appears.

              role="status" rather than the Alert's own role="alert": this is
              confirmation of something the reader just asked for, and screen
              readers give "alert" the assertive treatment reserved for
              problems. It is cleared by updateSpace.reset() the moment either
              field is touched again, so it can never stand over a form that
              has since changed. */}
          {updateSpace.isSuccess && (
            <Alert role="status" data-testid="settings-saved">
              <AlertDescription>{t("settings.saved")}</AlertDescription>
            </Alert>
          )}
          <div>
            <Button disabled={!canSave} onClick={save}>
              {t("common.save")}
            </Button>
          </div>
        </CardContent>
      </Card>
      <ConnectionsSection />
    </div>
  );
}

// statusVariant picks how a connection's state is drawn. A switch over the
// contract's own three values rather than a two-way test, so a fourth state
// added there arrives here as a type error instead of quietly rendering as
// «everything is fine» or as «nothing to do».
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

// ConnectionsSection lists the space's broker connections (T-Invest is the
// only broker there is one of today) and the way to add another. It reads
// GET /api/v1/tinvest/connections itself rather than depending on anything
// the currency/residency form above already fetched, so a failure or a slow
// answer there never blanks this section out.
function ConnectionsSection() {
  const { t } = useTranslation();
  const connections = useConnections();
  const list = connections.data ?? [];

  return (
    <Card className="max-w-md">
      <CardHeader>
        <CardTitle>{t("connections.title")}</CardTitle>
      </CardHeader>
      <CardContent className="grid gap-4">
        {connections.isLoading && (
          <p className="text-sm text-muted-foreground">{t("app.loading")}</p>
        )}
        {connections.isError && (
          <Alert variant="destructive">
            <AlertDescription>{t("app.error")}</AlertDescription>
          </Alert>
        )}
        {!connections.isLoading && !connections.isError && list.length === 0 && (
          <p className="text-sm text-muted-foreground">{t("connections.empty")}</p>
        )}
        {list.length > 0 && (
          <ul className="grid gap-2">
            {list.map((connection) => (
              <li key={connection.id}>
                <Link
                  to="/settings/connections/$connectionId"
                  params={{ connectionId: connection.id }}
                  className="flex items-center justify-between rounded-lg border p-3 text-sm hover:bg-muted"
                >
                  <div className="grid gap-0.5">
                    <span className="font-medium">{t("connections.tinvest")}</span>
                    <span className="text-xs text-muted-foreground">
                      {t("connections.tokenLast4", { last4: connection.token_last4 })}
                    </span>
                  </div>
                  {/* Three states, three looks, because two of them are not
                      the same news. `token_revoked` is a verdict the server
                      reached by asking the broker: this connection has stopped
                      importing and will not start again until the owner pastes
                      a new token. `disabled` is the owner's own doing and needs
                      nothing from anyone. Drawn alike — both quiet grey — the
                      one that is waiting for a person looks exactly like the
                      one that is waiting for nobody. */}
                  <Badge variant={statusVariant(connection.status)}>
                    {t(`connections.statuses.${connection.status}`)}
                  </Badge>
                </Link>
              </li>
            ))}
          </ul>
        )}
        <div>
          <Button asChild>
            <Link to="/settings/connections/new">{t("connections.connect")}</Link>
          </Button>
        </div>
      </CardContent>
    </Card>
  );
}
